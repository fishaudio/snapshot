// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const fanInMapping = "prefill=snap-prefill,decode=snap-decode"

// fanInRestorePod is a prefill/decode restore Pod: each GPU container restores
// from its own PodSnapshot, and a router sidecar starts normally.
func fanInRestorePod() *corev1.Pod {
	pod := restorePod(nil)
	pod.Spec.Containers = []corev1.Container{
		{Name: "prefill", Image: "worker:prefill"},
		{Name: "decode", Image: "worker:decode"},
		{Name: "router", Image: "router:latest"},
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "prefill", ContainerID: "containerd://prefill-id", State: running},
		{Name: "decode", ContainerID: "containerd://decode-id", State: running},
		{Name: "router", ContainerID: "containerd://router-id", State: running},
	}
	shaped, err := podcontract.Build(pod, podcontract.Request{SnapshotMappings: []podcontract.SnapshotMapping{
		{Destination: "prefill", SnapshotName: "snap-prefill"},
		{Destination: "decode", SnapshotName: "snap-decode"},
	}}, podcontract.Options{})
	if err != nil {
		panic(err)
	}
	// Build writes the canonical, sorted form. Keep the order a user writes,
	// which the Pod's messages follow.
	shaped.Annotations[podcontract.RestoreFromMapAnnotation] = fanInMapping
	return shaped
}

// readySnapshotObjectsFor returns a Ready PodSnapshot named name and its bound
// content, which checkpointed container.
func readySnapshotObjectsFor(name, contentUID, container string) (*snapshotv1alpha1.PodSnapshot, *snapshotv1alpha1.PodSnapshotContent) {
	snapshot, content := readySnapshotObjects()
	contentName := "podsnapshotcontent-" + name
	snapshot.Name = name
	snapshot.UID = ktypes.UID(name + "-uid")
	snapshot.Spec.Source.PodRef.Containers = []string{container}
	snapshot.Status.BoundPodSnapshotContentName = &contentName
	content.Name = contentName
	content.UID = ktypes.UID(contentUID)
	content.Spec.PodSnapshotRef = snapshotv1alpha1.PodSnapshotReference{Namespace: snapshot.Namespace, Name: name, UID: snapshot.UID}
	content.Spec.Source.PodRef.Containers = []string{container}
	return snapshot, content
}

// fanInSources are the PodSnapshots of fanInRestorePod. snap-decode
// checkpointed a container named worker, so its artifact and destination
// names differ.
type fanInSources struct {
	prefillSnapshot *snapshotv1alpha1.PodSnapshot
	prefillContent  *snapshotv1alpha1.PodSnapshotContent
	decodeSnapshot  *snapshotv1alpha1.PodSnapshot
	decodeContent   *snapshotv1alpha1.PodSnapshotContent
}

func readyFanInSources() *fanInSources {
	s := &fanInSources{}
	s.prefillSnapshot, s.prefillContent = readySnapshotObjectsFor("snap-prefill", "content-prefill", "prefill")
	s.decodeSnapshot, s.decodeContent = readySnapshotObjectsFor("snap-decode", "content-decode", "worker")
	return s
}

// objects returns the PodSnapshots and contents a test has not removed.
func (s *fanInSources) objects() []runtime.Object {
	var objects []runtime.Object
	for _, snapshot := range []*snapshotv1alpha1.PodSnapshot{s.prefillSnapshot, s.decodeSnapshot} {
		if snapshot != nil {
			objects = append(objects, snapshot)
		}
	}
	for _, content := range []*snapshotv1alpha1.PodSnapshotContent{s.prefillContent, s.decodeContent} {
		if content != nil {
			objects = append(objects, content)
		}
	}
	return objects
}

// writeContentArtifact creates the artifact directory, with a manifest, that
// legacy content resolves to.
func writeContentArtifact(t *testing.T, w *NodeController, content *snapshotv1alpha1.PodSnapshotContent) string {
	t.Helper()
	container := content.Spec.Source.PodRef.Containers[0]
	path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), container)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o700))
	require.NoError(t, types.WriteManifest(path, &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: container},
	}))
	return path
}

// makeFanInController serves s and writes an artifact for each content.
func makeFanInController(t *testing.T, pod *corev1.Pod, s *fanInSources) *NodeController {
	t.Helper()
	w := makeTestController(t, pod, s.objects()...)
	for _, content := range []*snapshotv1alpha1.PodSnapshotContent{s.prefillContent, s.decodeContent} {
		if content != nil {
			writeContentArtifact(t, w, content)
		}
	}
	return w
}

// countGets counts the controller's API reads.
func countGets(t *testing.T, w *NodeController, objects []runtime.Object) *int {
	t.Helper()
	gets := 0
	w.client = ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithRuntimeObjects(objects...).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, delegated client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets++
				return delegated.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	return &gets
}

func noRestoreExpected(t *testing.T) func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		t.Error("no destination may restore")
		return executor.RestoreResult{}, nil
	}
}

func assertNoRestoreFinalizer(t *testing.T, w *NodeController) {
	t.Helper()
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if patch, ok := action.(clientgotesting.PatchAction); ok && patch.GetSubresource() == "" {
			t.Fatal("the restore finalizer must not be added")
		}
	}
}

func TestRestorePodRequestedByRestoreFromMap(t *testing.T) {
	pod := fanInRestorePod()
	w := makeTestController(t, pod)

	w.enqueueRestorePod(pod)

	assert.Equal(t, 1, w.restoreQueue.Len())
}

func TestPreflightRestoreFromMapPlansEachPodSnapshot(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	decodeAction := httpPostRestoreSpec()
	sources.decodeContent.Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{
		Name:      "worker",
		Lifecycle: &snapshotv1alpha1.SnapshotLifecycle{PostRestore: decodeAction},
	}}
	w := makeFanInController(t, pod, sources)
	gets := countGets(t, w, sources.objects())

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, 4, *gets, "each PodSnapshot and its content are read once")
	assert.Nil(t, plan.artifact)
	assert.Equal(t, []podcontract.ContainerMapping{
		{Source: "prefill", Destination: "prefill"},
		{Source: "worker", Destination: "decode"},
	}, plan.mappings, "every destination, in annotation order")
	require.Len(t, plan.sources, 2)

	prefill := plan.sourceFor("prefill")
	assert.Equal(t, "snap-prefill", prefill.artifact.SnapshotName)
	assert.Equal(t, "content-prefill", prefill.artifact.ContentUID)
	assert.Equal(t, "prefill", prefill.artifact.SourceContainerName)
	assert.Equal(t, []podcontract.ContainerMapping{{Source: "prefill", Destination: "prefill"}}, prefill.mappings)
	assert.Nil(t, prefill.postRestore)

	decode := plan.sourceFor("decode")
	assert.Equal(t, "snap-decode", decode.artifact.SnapshotName)
	assert.Equal(t, "content-decode", decode.artifact.ContentUID)
	assert.Equal(t, "worker", decode.artifact.SourceContainerName)
	assert.NotNil(t, decode.postRestore, "each PodSnapshot's own post-restore action")

	assert.Same(t, plan, plan.sourceFor("router"), "a sidecar has no source")
	assert.Equal(t, "PodSnapshots snap-prefill (prefill), snap-decode (decode)", plan.from())
}

func TestPreflightRestoreFromMapReadsASharedPodSnapshotOnce(t *testing.T) {
	pod := fanInRestorePod()
	pod.Annotations[podcontract.RestoreFromMapAnnotation] = "prefill=snap-prefill,decode=snap-prefill"
	sources := readyFanInSources()
	sources.decodeSnapshot, sources.decodeContent = nil, nil
	w := makeFanInController(t, pod, sources)
	gets := countGets(t, w, sources.objects())

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, 2, *gets)
	require.Len(t, plan.sources, 1)
	assert.Equal(t, []podcontract.ContainerMapping{
		{Source: "prefill", Destination: "prefill"},
		{Source: "prefill", Destination: "decode"},
	}, plan.sources[0].mappings)
	assert.Equal(t, "PodSnapshot snap-prefill (prefill, decode)", plan.from())
}

func TestPreflightRestoreFromMapPendingPodSnapshotKeepsThePodPending(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate      func(*fanInSources)
		wantReason  string
		wantMessage []string
	}{
		"PodSnapshot not created yet": {
			mutate:      func(s *fanInSources) { s.decodeSnapshot, s.decodeContent = nil, nil },
			wantReason:  "SnapshotPending",
			wantMessage: []string{"PodSnapshot snap-decode (decode): Waiting for PodSnapshot inference/snap-decode"},
		},
		"PodSnapshot not Ready": {
			mutate: func(s *fanInSources) {
				s.decodeSnapshot.Status = snapshotv1alpha1.PodSnapshotStatus{}
				s.decodeContent = nil
			},
			wantReason:  "SnapshotPending",
			wantMessage: []string{"PodSnapshot snap-decode (decode): Waiting for PodSnapshot inference/snap-decode to become Ready"},
		},
		"content not Ready": {
			mutate:      func(s *fanInSources) { s.decodeContent.Status.Conditions = nil },
			wantReason:  "ContentPending",
			wantMessage: []string{"PodSnapshot snap-decode (decode): Waiting for PodSnapshotContent podsnapshotcontent-snap-decode to become Ready"},
		},
		"artifact not published": {
			mutate: func(s *fanInSources) {
				s.decodeContent.Spec.Storage = &snapshotv1alpha1.CheckpointStorageBinding{StoreID: testStoreID}
			},
			wantReason:  "ArtifactPending",
			wantMessage: []string{"PodSnapshot snap-decode (decode): Waiting for a published artifact for container worker"},
		},
		"both pending": {
			mutate: func(s *fanInSources) {
				s.prefillSnapshot.Status = snapshotv1alpha1.PodSnapshotStatus{}
				s.decodeContent.Status.Conditions = nil
			},
			wantReason: "SnapshotPending",
			wantMessage: []string{
				"PodSnapshot snap-prefill (prefill): Waiting for PodSnapshot inference/snap-prefill to become Ready",
				"PodSnapshot snap-decode (decode): Waiting for PodSnapshotContent",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := fanInRestorePod()
			sources := readyFanInSources()
			tc.mutate(sources)
			w := makeFanInController(t, pod, sources)
			w.restoreFn = noRestoreExpected(t)

			plan, err := w.preflightRestore(context.Background(), pod)

			assert.Nil(t, plan)
			assert.Equal(t, tc.wantReason, pendingRestoreReason(t, err))
			for _, want := range tc.wantMessage {
				assert.Contains(t, err.Error(), want)
			}

			requeue := w.reconcileRestorePod(context.Background(), pod)

			assert.True(t, requeue)
			payload := string(lastPodStatusApply(t, w).GetPatch())
			assert.Contains(t, payload, `"reason":"`+tc.wantReason+`"`)
			assert.Contains(t, payload, tc.wantMessage[0])
			assertNoRestoreFinalizer(t, w)
		})
	}
}

// A PodSnapshot that can never restore fails the Pod at once, even while
// another one is still pending.
func TestPreflightRestoreFromMapFailedPodSnapshotFailsThePod(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	sources.prefillSnapshot.Status = snapshotv1alpha1.PodSnapshotStatus{}
	sources.decodeSnapshot.Status.Conditions = []metav1.Condition{{
		Type: snapshotv1alpha1.PodSnapshotConditionFailed, Status: metav1.ConditionTrue, Message: "dump aborted",
	}}
	w := makeFanInController(t, pod, sources)
	w.restoreFn = noRestoreExpected(t)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
	assert.Contains(t, payload, "PodSnapshot snap-decode (decode): PodSnapshot inference/snap-decode failed: dump aborted")
	assertNoRestoreFinalizer(t, w)
}

func TestPreflightRestoreFromMapMissingArtifactFailsThePod(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeTestController(t, pod, sources.objects()...)
	writeContentArtifact(t, w, sources.prefillContent) // none for snap-decode
	w.restoreFn = noRestoreExpected(t)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
	assert.Contains(t, payload, "PodSnapshot snap-decode (decode): artifact ")
	assert.Contains(t, payload, "for Ready PodSnapshotContent podsnapshotcontent-snap-decode is missing")
	assertNoRestoreFinalizer(t, w)
}

func TestPreflightRestoreFromMapIncompatiblePodSnapshotRefusesThePod(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	var compared []string
	w.compareFn = func(_ compat.Gate, _, target compat.Environment) []compat.Mismatch {
		compared = append(compared, target.Image)
		if target.Image == "worker:decode" {
			return []compat.Mismatch{{Check: "cpu-arch", Source: "amd64", Target: "arm64"}}
		}
		return nil
	}
	w.restoreFn = noRestoreExpected(t)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"worker:prefill", "worker:decode"}, compared,
		"each PodSnapshot is compared with its own destination")
	condition := restoredPodCondition(pod)
	require.NotNil(t, condition)
	assert.Equal(t, podcontract.RestoreReasonIncompatible, condition.Reason)
	assert.Equal(t, "Refused restore; this node cannot run the checkpoint: "+
		"PodSnapshot snap-decode (decode): cpu-arch: source amd64, target arm64", condition.Message)
	assertNoRestoreFinalizer(t, w)
}

func TestPreflightRestoreFromMapRejectsInvalidPods(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"with restore-from": func(pod *corev1.Pod) {
			pod.Annotations[podcontract.RestoreFromAnnotation] = "snap-prefill"
		},
		"with a container map": func(pod *corev1.Pod) {
			pod.Annotations[podcontract.RestoreContainerMapAnnotation] = "main=prefill"
		},
		"duplicate destination": func(pod *corev1.Pod) {
			pod.Annotations[podcontract.RestoreFromMapAnnotation] = "prefill=snap-prefill,prefill=snap-decode"
		},
		"missing destination": func(pod *corev1.Pod) {
			pod.Annotations[podcontract.RestoreFromMapAnnotation] = "prefill=snap-prefill,missing=snap-decode"
		},
		"shared control directory": func(pod *corev1.Pod) {
			pod.Spec.Containers[1].VolumeMounts[0].SubPath = "prefill"
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := fanInRestorePod()
			mutate(pod)
			sources := readyFanInSources()
			w := makeFanInController(t, pod, sources)
			gets := countGets(t, w, sources.objects())
			w.restoreFn = noRestoreExpected(t)

			requeue := w.reconcileRestorePod(context.Background(), pod)

			assert.False(t, requeue)
			assert.Zero(t, *gets, "an invalid Pod fails before any API read")
			assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreFailed"`)
			assertNoRestoreFinalizer(t, w)
		})
	}
}

func TestReconcileRestoreFromMapRestoresEachDestinationFromItsPodSnapshotConcurrently(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)

	started := make(chan executor.RestoreRequest, 2)
	release := make(chan struct{})
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		started <- req
		<-release
		if req.DestinationContainerName == "prefill" {
			return executor.RestoreResult{PlaceholderHostPID: 100, RestoredPID: 1}, nil
		}
		return executor.RestoreResult{PlaceholderHostPID: 101, RestoredPID: 1}, nil
	}
	sentinels := make(chan int, 2)
	w.writeControlSentinelFn = func(pid int, name string, _ []byte) error {
		assert.Equal(t, podcontract.RestoreCompleteFile, name)
		sentinels <- pid
		return nil
	}

	done := make(chan bool, 1)
	go func() { done <- w.reconcileRestorePod(context.Background(), pod) }()

	requests := map[string]executor.RestoreRequest{}
	for range 2 {
		select {
		case req := <-started:
			requests[req.DestinationContainerName] = req
		case <-time.After(time.Second):
			t.Fatal("restore workers did not overlap")
		}
	}
	inProgress := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, inProgress, `"reason":"RestoreInProgress"`)
	assert.Contains(t, inProgress, "Restoring 2 destination container(s) from PodSnapshots snap-prefill (prefill), snap-decode (decode)")
	close(release)
	assert.False(t, <-done)

	require.Len(t, requests, 2)
	assert.Equal(t, "content-prefill", requests["prefill"].ContentUID)
	assert.Equal(t, "prefill", requests["prefill"].ArtifactContainerName)
	assert.Equal(t, "content-decode", requests["decode"].ContentUID)
	assert.Equal(t, "worker", requests["decode"].ArtifactContainerName)
	assert.Equal(t, "decode-id", requests["decode"].ContainerID)
	assert.ElementsMatch(t, []int{100, 101}, []int{<-sentinels, <-sentinels})

	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"status":"True"`)
	assert.Contains(t, payload, `"reason":"RestoreSucceeded"`)
	assert.Contains(t, payload, "Restored 2 destination container(s) from PodSnapshots snap-prefill (prefill), snap-decode (decode): prefill, decode")
	clientset := w.clientset.(*fake.Clientset)
	var requested []string
	for _, event := range eventsForReason(clientset, restoreRequestedReason) {
		requested = append(requested, event.Message)
	}
	assert.ElementsMatch(t, []string{
		"Restore requested from PodSnapshot snap-prefill for destination prefill",
		"Restore requested from PodSnapshot snap-decode for destination decode",
	}, requested)
	live, err := clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
}

func TestReconcileRestoreFromMapReportsAFailedDestination(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		if req.DestinationContainerName == "decode" {
			return executor.RestoreResult{}, errors.New("restore failed")
		}
		return executor.RestoreResult{PlaceholderHostPID: 100, RestoredPID: 1}, nil
	}
	var mu sync.Mutex
	var written []string
	w.writeControlSentinelFn = func(pid int, name string, _ []byte) error {
		mu.Lock()
		defer mu.Unlock()
		written = append(written, name)
		return nil
	}
	killed := 0
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error {
		mu.Lock()
		defer mu.Unlock()
		killed++
		return nil
	}

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestorePartiallySucceeded"`)
	assert.Contains(t, payload, "Restored 1 of 2 destination containers from PodSnapshots snap-prefill (prefill), snap-decode (decode); "+
		"not restored: decode: restore failed")
	assert.True(t, isRestoreTerminal(pod))
	assert.ElementsMatch(t, []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile}, written)
	assert.Equal(t, 1, killed, "only the failed destination's placeholder is killed")
}

func TestReconcileRestoreFromMapReportsAllDestinationsFailed(t *testing.T) {
	pod := fanInRestorePod()
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		return executor.RestoreResult{}, errors.New(req.DestinationContainerName + " restore failed")
	}

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
	assert.Contains(t, payload, "Restore failed for all 2 destination container(s) from PodSnapshots snap-prefill (prefill), snap-decode (decode): "+
		"prefill: prefill restore failed; decode: decode restore failed")
}

// containerPIDRuntime resolves each container ID to its own PID.
type containerPIDRuntime struct {
	*fakeRuntime
	pids map[string]int
}

func (r *containerPIDRuntime) ResolveContainer(_ context.Context, id string) (int, *specs.Spec, error) {
	if pid, ok := r.pids[id]; ok {
		return pid, &specs.Spec{}, nil
	}
	return 0, nil, errors.New("unknown container")
}

// After an agent restart, each destination checks its own completion
// sentinel: a destination that already restored is not restored again.
func TestReconcileRestoreFromMapRecoversEachDestinationOnItsOwn(t *testing.T) {
	pod := fanInRestorePod()
	pod.Finalizers = []string{restorePodFinalizer}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
	sources := readyFanInSources()
	w := makeFanInController(t, pod, sources)
	w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "decode-id": 200}}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		if name == podcontract.RestoreFailedFile {
			return false, nil
		}
		assert.Equal(t, podcontract.RestoreCompleteFile, name)
		return pid == 100, nil
	}
	var mu sync.Mutex
	var restored []string
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		restored = append(restored, req.DestinationContainerName+" from "+req.ContentUID)
		return executor.RestoreResult{PlaceholderHostPID: 200, RestoredPID: 1}, nil
	}

	processQueuedRestorePod(t, w, pod)

	assert.Equal(t, []string{"decode from content-decode"}, restored)
	live, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, hasFinalizer(live, restorePodFinalizer))
	condition := restoredPodCondition(live)
	require.NotNil(t, condition)
	assert.Equal(t, podcontract.RestoreReasonSucceeded, condition.Reason)
}

func TestRestoreTallyNamesEachDestinationsPodSnapshot(t *testing.T) {
	result := func(destination, snapshot string, state restoreResultState, reason string) restoreResult {
		return restoreResult{destination: destination, snapshot: snapshot, state: state, reason: reason}
	}
	tests := []struct {
		name        string
		results     []restoreResult
		wantReason  string
		wantMessage string
	}{
		{
			name: "a destination is still pending",
			results: []restoreResult{
				result("prefill", "snap-prefill", restoreResultSucceeded, ""),
				result("decode", "snap-decode", restoreResultPending, ""),
			},
			wantReason:  podcontract.RestoreReasonInProgress,
			wantMessage: "Restore from PodSnapshots snap-prefill (prefill), snap-decode (decode) remains in progress: 1 succeeded, 0 failed, 1 pending (decode)",
		},
		{
			name: "two destinations share a PodSnapshot",
			results: []restoreResult{
				result("engine-0", "snap-a", restoreResultSucceeded, ""),
				result("decode", "snap-decode", restoreResultSucceeded, ""),
				result("engine-1", "snap-a", restoreResultSucceeded, ""),
			},
			wantReason:  podcontract.RestoreReasonSucceeded,
			wantMessage: "Restored 3 destination container(s) from PodSnapshots snap-a (engine-0, engine-1), snap-decode (decode): engine-0, decode, engine-1",
		},
		{
			name: "every destination refused",
			results: []restoreResult{
				result("prefill", "snap-prefill", restoreResultIncompatible, "gpu-count: source 1, target 0"),
			},
			wantReason: podcontract.RestoreReasonIncompatible,
			wantMessage: "Refused restore; this node cannot run the checkpoint: " +
				"PodSnapshot snap-prefill (prefill): gpu-count: source 1, target 0",
		},
		{
			name: "one failed and one refused",
			results: []restoreResult{
				result("prefill", "snap-prefill", restoreResultFailed, "CRIU restore failed: boom"),
				result("decode", "snap-decode", restoreResultIncompatible, "gpu-count: source 1, target 0"),
			},
			wantReason: podcontract.RestoreReasonFailed,
			wantMessage: "Restore failed for 1 destination container(s) and refused 1 incompatible destination(s) " +
				"from PodSnapshots snap-prefill (prefill), snap-decode (decode): prefill: CRIU restore failed: boom",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := tallyRestoreResults(test.results).verdict("")

			assert.Equal(t, test.wantReason, verdict.reason)
			assert.Equal(t, test.wantMessage, verdict.message)
		})
	}
}

func TestRestoreDestinationsFromMap(t *testing.T) {
	t.Run("needs no API read", func(t *testing.T) {
		pod := fanInRestorePod()
		w := makeTestController(t, pod) // no PodSnapshot exists yet
		gets := countGets(t, w, nil)

		got, err := w.RestoreDestinations(context.Background(), pod)

		require.NoError(t, err)
		assert.Equal(t, []string{"prefill", "decode"}, got, "the router sidecar is no destination")
		assert.Zero(t, *gets)
	})

	for name, annotations := range map[string]map[string]string{
		"with restore-from":     {podcontract.RestoreFromMapAnnotation: fanInMapping, podcontract.RestoreFromAnnotation: "snap-prefill"},
		"duplicate destination": {podcontract.RestoreFromMapAnnotation: "prefill=snap-a,prefill=snap-b"},
		"malformed":             {podcontract.RestoreFromMapAnnotation: "prefill"},
	} {
		t.Run(name, func(t *testing.T) {
			pod := fanInRestorePod()
			pod.Annotations = annotations
			w := makeTestController(t, pod)

			_, err := w.RestoreDestinations(context.Background(), pod)

			require.Error(t, err)
		})
	}
}
