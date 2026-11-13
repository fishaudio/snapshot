// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// controlVolume stands in for a restore Pod's snapshot-control emptyDir: one
// directory per container, which outlives the container's restarts. It also
// records whose placeholder the agent killed.
type controlVolume struct {
	mu     sync.Mutex
	pids   map[int]string
	files  map[string][]string
	killed []string
}

func newControlVolume(pids map[int]string) *controlVolume {
	return &controlVolume{pids: pids, files: map[string][]string{}}
}

// install points the controller's sentinel and signal operations at v.
func (v *controlVolume) install(w *NodeController) {
	w.writeControlSentinelFn = func(pid int, name string, _ []byte) error {
		v.mu.Lock()
		defer v.mu.Unlock()
		container, ok := v.pids[pid]
		if !ok {
			return fmt.Errorf("no process %d", pid)
		}
		v.files[container] = append(v.files[container], name)
		return nil
	}
	w.writePodControlSentinelFn = func(_, container, name string, _ []byte) error {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.files[container] = append(v.files[container], name)
		return nil
	}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		container, ok := v.pids[pid]
		if !ok {
			return false, fmt.Errorf("no process %d", pid)
		}
		return slices.Contains(v.files[container], name), nil
	}
	w.sendSignalFn = func(_ logr.Logger, pid int, _ syscall.Signal, _ string) error {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.killed = append(v.killed, v.pids[pid])
		return nil
	}
}

// start records a new process of container, as after a restart.
func (v *controlVolume) start(pid int, container string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.pids[pid] = container
}

func (v *controlVolume) write(container, name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[container] = append(v.files[container], name)
}

func (v *controlVolume) holds(container, name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Contains(v.files[container], name)
}

func (v *controlVolume) kills() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.killed)
}

// appliedRestoredCondition reads the condition off the last status apply.
func appliedRestoredCondition(t *testing.T, w *NodeController) corev1.PodCondition {
	t.Helper()
	var applied struct {
		Status struct {
			Conditions []corev1.PodCondition `json:"conditions"`
		} `json:"status"`
	}
	require.NoError(t, json.Unmarshal(lastPodStatusApply(t, w).GetPatch(), &applied))
	require.Len(t, applied.Status.Conditions, 1)
	return applied.Status.Conditions[0]
}

// useClock drives the restore queue's delays and the controller's clock from
// clock.
func useClock(t *testing.T, w *NodeController, clock *clocktesting.FakeClock) {
	t.Helper()
	w.restoreQueue.ShutDown()
	w.restoreQueue = workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[client.ObjectKey]{Clock: clock})
	t.Cleanup(w.restoreQueue.ShutDown)
	w.nowFn = clock.Now
}

func startedAt(pod *corev1.Pod, container string, at time.Time) {
	for i := range pod.Status.ContainerStatuses {
		if pod.Status.ContainerStatuses[i].Name == container {
			pod.Status.ContainerStatuses[i].State = corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at)},
			}
		}
	}
}

// failFirstStatusWrite makes the controller's first Pod status write fail.
func failFirstStatusWrite(w *NodeController) {
	failed := false
	w.clientset.(*fake.Clientset).PrependReactor("patch", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		if action.(clientgotesting.PatchAction).GetSubresource() != "status" || failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.New("status write failed")
	})
}

// A restore the preflight gate refuses ends every destination container like a
// failed restore, so each starts without a restore at once. The sidecar that
// was never a destination is left alone, and the condition keeps its reason
// and message.
func TestPreflightRefusalEndsEveryDestination(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "decode-id": 200, "router-id": 300}}
	volume := newControlVolume(map[int]string{100: "prefill", 200: "decode", 300: "router"})
	volume.install(w)
	w.compareFn = func(_ compat.Gate, _, target compat.Environment) []compat.Mismatch {
		if target.Image == "worker:decode" {
			return []compat.Mismatch{{Check: "cpu-arch", Source: "amd64", Target: "arm64"}}
		}
		return nil
	}
	w.restoreFn = noRestoreExpected(t)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.ElementsMatch(t, []string{"prefill", "decode"}, volume.kills())
	assert.True(t, volume.holds("prefill", podcontract.RestoreFailedFile))
	assert.True(t, volume.holds("decode", podcontract.RestoreFailedFile))
	assert.False(t, volume.holds("router", podcontract.RestoreFailedFile))
	condition := appliedRestoredCondition(t, w)
	assert.Equal(t, podcontract.RestoreReasonIncompatible, condition.Reason)
	assert.Equal(t, "Refused restore; this node cannot run the checkpoint: "+
		"PodSnapshot snap-decode (decode): cpu-arch: source amd64, target arm64", condition.Message)
}

// The wait for a PodSnapshot that does not appear ends
// config.restore.snapshotWaitSeconds after the destination container started,
// in a pass due at that moment rather than at the next resync.
func TestSnapshotWaitEndsTheRestoreWhenTheSnapshotDoesNotAppear(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	startedAt(pod, "main", started)
	w := makeTestController(t, pod) // the PodSnapshot does not exist
	clock := clocktesting.NewFakeClock(started.Add(45 * time.Second))
	useClock(t, w, clock)
	w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
	volume := newControlVolume(map[int]string{100: "main"})
	volume.install(w)
	w.restoreFn = noRestoreExpected(t)

	processQueuedRestorePod(t, w, pod)

	assert.Equal(t, snapshotPendingReason, appliedRestoredCondition(t, w).Reason)
	assert.Empty(t, volume.kills(), "the wait has 15s left")
	clock.Step(15 * time.Second)
	require.Eventually(t, func() bool { return w.restoreQueue.Len() == 1 }, time.Second, time.Millisecond,
		"no pass is due when the wait runs out")
	item, shutdown := w.restoreQueue.Get()
	require.False(t, shutdown)
	w.processRestoreQueueItem(context.Background(), item)

	condition := appliedRestoredCondition(t, w)
	assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
	assert.Equal(t, "Stopped waiting for the snapshot after 60s (config.restore.snapshotWaitSeconds): "+
		"Waiting for PodSnapshot inference/snapshot-a; the destination containers start without a restore", condition.Message)
	assert.Equal(t, []string{"main"}, volume.kills())
	assert.True(t, volume.holds("main", podcontract.RestoreFailedFile))
	assert.True(t, w.restoreHandled(pod))
}

// Every way of the snapshot not being ready to restore counts against the wait,
// and the end reaches every destination of a restore-from-map Pod.
func TestSnapshotWaitCoversEveryWayTheSnapshotIsNotReady(t *testing.T) {
	for name, mutate := range map[string]func(*fanInSources){
		"PodSnapshot not created": func(s *fanInSources) { s.decodeSnapshot, s.decodeContent = nil, nil },
		"PodSnapshot not Ready": func(s *fanInSources) {
			s.decodeSnapshot.Status = snapshotv1alpha1.PodSnapshotStatus{}
			s.decodeContent = nil
		},
		"content not Ready": func(s *fanInSources) { s.decodeContent.Status.Conditions = nil },
		"artifact not published": func(s *fanInSources) {
			s.decodeContent.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: testStoreID}
		},
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
			pod := fanInRestorePod()
			startedAt(pod, "prefill", started)
			startedAt(pod, "decode", started.Add(2*time.Second))
			sources := readyFanInSources()
			mutate(sources)
			w := makeFanInController(t, pod, sources)
			w.config.Restore.SnapshotWaitSeconds = ptr.To(30)
			w.nowFn = func() time.Time { return started.Add(30 * time.Second) }
			w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "decode-id": 200, "router-id": 300}}
			volume := newControlVolume(map[int]string{100: "prefill", 200: "decode", 300: "router"})
			volume.install(w)
			w.restoreFn = noRestoreExpected(t)

			requeue := w.reconcileRestorePod(context.Background(), pod)

			assert.False(t, requeue)
			condition := appliedRestoredCondition(t, w)
			assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
			assert.Contains(t, condition.Message, "Stopped waiting for the snapshot after 30s (config.restore.snapshotWaitSeconds): PodSnapshot snap-decode (decode): ")
			assert.ElementsMatch(t, []string{"prefill", "decode"}, volume.kills(), "the wait counts from the first destination's start")
		})
	}
}

func TestSnapshotWaitLeavesARestoreWaiting(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		wait   *int
		mutate func(*corev1.Pod)
	}{
		// Zero keeps the unbounded wait of earlier agents.
		"at zero": {
			wait:   ptr.To(0),
			mutate: func(pod *corev1.Pod) { startedAt(pod, "main", started) },
		},
		// The workload's own wait for the restore starts with its container,
		// so the agent's does too: an image pull does not count.
		"before a destination container started": {
			mutate: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].ContainerID = ""
				pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			tc.mutate(pod)
			w := makeTestController(t, pod)
			w.config.Restore.SnapshotWaitSeconds = tc.wait
			w.nowFn = func() time.Time { return started.Add(time.Hour) }
			volume := newControlVolume(map[int]string{})
			volume.install(w)

			requeue := w.reconcileRestorePod(context.Background(), pod)

			assert.True(t, requeue)
			assert.Equal(t, snapshotPendingReason, appliedRestoredCondition(t, w).Reason)
			assert.Empty(t, volume.kills())
			assert.False(t, volume.holds("main", podcontract.RestoreFailedFile))
		})
	}
}

// The PodSnapshot appears after the agent gave up waiting for it, and the
// destination container has restarted and runs without a restore. Nothing
// restores into it, or kills it again: not the agent that gave up, not one
// that restarted since, and not one whose record of the outcome was lost.
func TestARestoreIsNeverRunAfterTheAgentGaveUp(t *testing.T) {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	podKey := client.ObjectKey{Namespace: "inference", Name: "restore-worker"}

	// giveUp runs the pass that ends the restore, then restarts the container
	// and creates the PodSnapshot it waited for. It returns the Pod as the
	// next pass reads it.
	giveUp := func(t *testing.T, w *NodeController, volume *controlVolume, pod *corev1.Pod) *corev1.Pod {
		t.Helper()
		w.nowFn = func() time.Time { return started.Add(61 * time.Second) }
		processQueuedRestorePod(t, w, pod)
		require.Equal(t, []string{"main"}, volume.kills(), "the agent did not give up")

		volume.start(200, "main")
		w.runtime.(*containerPIDRuntime).pids["restarted-container"] = 200
		snapshot, content := readySnapshotObjects()
		require.NoError(t, w.client.Create(context.Background(), snapshot))
		require.NoError(t, w.client.Create(context.Background(), content))
		writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), nil)

		live, err := w.clientset.CoreV1().Pods(podKey.Namespace).Get(context.Background(), podKey.Name, metav1.GetOptions{})
		require.NoError(t, err)
		live.Status.ContainerStatuses[0].ContainerID = "containerd://restarted-container"
		live.Status.ContainerStatuses[0].RestartCount = 1
		startedAt(live, "main", started.Add(62*time.Second))
		return live
	}
	setup := func(t *testing.T) (*NodeController, *controlVolume, *corev1.Pod) {
		t.Helper()
		pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
		startedAt(pod, "main", started)
		w := makeTestController(t, pod)
		w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
		volume := newControlVolume(map[int]string{100: "main"})
		volume.install(w)
		w.restoreFn = noRestoreExpected(t)
		return w, volume, pod
	}

	t.Run("the agent that gave up", func(t *testing.T) {
		w, volume, pod := setup(t)
		live := giveUp(t, w, volume, pod)

		processQueuedRestorePod(t, w, live)

		assert.Equal(t, []string{"main"}, volume.kills())
	})

	t.Run("an agent that restarted since", func(t *testing.T) {
		w, volume, pod := setup(t)
		live := giveUp(t, w, volume, pod)
		require.Equal(t, podcontract.RestoreReasonFailed, restoredPodCondition(live).Reason)
		restarted := makeTestController(t, live)
		restarted.config = w.config
		restarted.client = w.client
		restarted.runtime = w.runtime
		volume.install(restarted)
		restarted.restoreFn = noRestoreExpected(t)

		processQueuedRestorePod(t, restarted, live)

		assert.Equal(t, []string{"main"}, volume.kills())
		assert.True(t, sawEventReason(restarted.clientset.(*fake.Clientset), restoreAlreadyFailedReason))
	})

	// The end's status write failed, so the next pass decides again and finds
	// the snapshot ready. restore-failed keeps the restore out.
	t.Run("an agent whose record of the outcome was lost", func(t *testing.T) {
		w, volume, pod := setup(t)
		failFirstStatusWrite(w)
		live := giveUp(t, w, volume, pod)
		require.Nil(t, restoredPodCondition(live), "the outcome was recorded")

		processQueuedRestorePod(t, w, live)

		assert.Equal(t, []string{"main"}, volume.kills(), "the container running without a restore was killed again")
		condition := appliedRestoredCondition(t, w)
		assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
		assert.Contains(t, condition.Message, "main: destination container already holds restore-failed")
	})

	// An agent that restarts during a restore replays it unless restore-complete
	// says it finished. restore-failed keeps that replay out too.
	t.Run("an agent that restarted during the restore", func(t *testing.T) {
		w, volume, pod := setup(t)
		live := giveUp(t, w, volume, pod)
		live.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodConditionType(podcontract.RestoredCondition),
			Status: corev1.ConditionFalse,
			Reason: podcontract.RestoreReasonInProgress,
		}}
		restarted := makeTestController(t, live)
		restarted.config = w.config
		restarted.client = w.client
		restarted.runtime = w.runtime
		volume.install(restarted)
		restarted.restoreFn = noRestoreExpected(t)

		processQueuedRestorePod(t, restarted, live)

		assert.Equal(t, []string{"main"}, volume.kills())
		condition := appliedRestoredCondition(t, restarted)
		assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
		assert.Contains(t, condition.Message, "destination container already holds restore-failed")
	})
}

// A destination whose container had not started when the restore ended is
// ended once it starts, and only once.
func TestADestinationThatStartsLateIsEndedWhenItStarts(t *testing.T) {
	pod := fanInRestorePod()
	pod.Status.ContainerStatuses[1] = corev1.ContainerStatus{
		Name:  "decode",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	rt := &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "router-id": 300}}
	w.runtime = rt
	volume := newControlVolume(map[int]string{100: "prefill", 300: "router"})
	volume.install(w)
	w.compareFn = func(compat.Gate, compat.Environment, compat.Environment) []compat.Mismatch {
		return []compat.Mismatch{{Check: "cpu-arch", Source: "amd64", Target: "arm64"}}
	}
	w.restoreFn = noRestoreExpected(t)

	processQueuedRestorePod(t, w, pod)

	assert.Equal(t, []string{"prefill"}, volume.kills())
	assert.Equal(t, podcontract.RestoreReasonIncompatible, appliedRestoredCondition(t, w).Reason)

	started := pod.DeepCopy()
	started.Status.ContainerStatuses[1] = corev1.ContainerStatus{
		Name:        "decode",
		ContainerID: "containerd://decode-id",
		State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	rt.pids["decode-id"] = 200
	volume.start(200, "decode")

	processQueuedRestorePod(t, w, started)
	processQueuedRestorePod(t, w, started)

	assert.Equal(t, []string{"prefill", "decode"}, volume.kills())
	assert.True(t, volume.holds("decode", podcontract.RestoreFailedFile))
	_, ending := w.endingRestores.Load(string(pod.UID))
	assert.False(t, ending)
}

// A second end, after the first one's status write failed, leaves alone the
// container that already holds restore-failed and may run without a restore.
func TestEndingADestinationTwiceKillsItOnce(t *testing.T) {
	r := newGatedRestore(t, compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"})
	r.controller.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
	volume := newControlVolume(map[int]string{100: "main"})
	volume.install(r.controller)
	failFirstStatusWrite(r.controller)

	r.reconcile(t)
	require.False(t, r.controller.restoreHandled(r.pod), "the outcome was recorded")
	r.reconcile(t)

	assert.Equal(t, []string{"main"}, volume.kills())
	assert.Equal(t, podcontract.RestoreReasonIncompatible, r.condition(t).Reason)
}

// A restore that fails after an agent restart ends the destinations that were
// still waiting, and spares the one that restored before the restart.
func TestEndingSparesARestoredDestination(t *testing.T) {
	pod := fanInRestorePod()
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
	sources := readyFanInSources()
	sources.decodeSnapshot, sources.decodeContent = nil, nil
	w := makeFanInController(t, pod, sources)
	w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "decode-id": 200}}
	volume := newControlVolume(map[int]string{100: "prefill", 200: "decode"})
	volume.install(w)
	volume.write("prefill", podcontract.RestoreCompleteFile)
	w.restoreFn = noRestoreExpected(t)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	condition := appliedRestoredCondition(t, w)
	assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
	assert.Contains(t, condition.Message, "PodSnapshot inference/snap-decode disappeared while restore was in progress")
	assert.Equal(t, []string{"decode"}, volume.kills())
	assert.False(t, volume.holds("prefill", podcontract.RestoreFailedFile))
}
