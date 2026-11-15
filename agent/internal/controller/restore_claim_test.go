// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// claimedBy is the content of a claim that owner created for a container.
func claimedBy(owner, containerID string) string {
	return string(podcontract.FormatRestoreClaim(podcontract.RestoreClaim{Owner: owner, ContainerID: containerID}))
}

// claimHarness is a single-destination restore Pod whose PodSnapshot is ready,
// with its control volume.
type claimHarness struct {
	w      *NodeController
	pod    *corev1.Pod
	volume *controlVolume
	rt     *containerPIDRuntime
}

func newClaimHarness(t *testing.T) *claimHarness {
	t.Helper()
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	writeContentArtifact(t, w, content)
	rt := &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
	w.runtime = rt
	volume := newControlVolume(map[int]string{100: "main"})
	volume.install(w)
	w.restoreFn = noRestoreExpected(t)
	return &claimHarness{w: w, pod: pod, volume: volume, rt: rt}
}

// restart replaces the destination container with a new one, pid 200, in the
// same Pod: the control volume and whatever it holds stay.
func (h *claimHarness) restart() {
	h.volume.start(200, "main")
	h.rt.pids["restarted-container"] = 200
	h.pod.Status.ContainerStatuses[0].ContainerID = "containerd://restarted-container"
	h.pod.Status.ContainerStatuses[0].RestartCount = 1
}

// recovering marks the Pod as an agent that stopped during its restore left
// it.
func (h *claimHarness) recovering() {
	h.pod.Status.Conditions = append(h.pod.Status.Conditions, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
}

// restoreInto restores successfully into pid, the container's placeholder,
// and runs during with the volume as the restore finds it.
func (h *claimHarness) restoreInto(pid int, during func()) *int {
	calls := 0
	h.w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		calls++
		during()
		return executor.RestoreResult{PlaceholderHostPID: pid, RestoredPID: 1}, nil
	}
	return &calls
}

// The agent claims the container before the restore touches it. The
// workload, giving up while the restore runs, then finds the claim and must
// not start: its exclusive create fails.
func TestTheAgentClaimsADestinationBeforeRestoringIt(t *testing.T) {
	h := newClaimHarness(t)
	calls := h.restoreInto(100, func() {
		assert.Equal(t, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID),
			h.volume.read("main", podcontract.RestoreClaimFile), "the restore began before the agent claimed the container")
		created, err := h.w.claimControlFileFn(100, podcontract.RestoreClaimFile,
			[]byte(claimedBy(podcontract.ControlFileOwnerWorkload, "")))
		require.NoError(t, err)
		assert.False(t, created, "the workload claimed a container the agent was restoring")
	})

	processQueuedRestorePod(t, h.w, h.pod)

	assert.Equal(t, 1, *calls)
	assert.Equal(t, podcontract.RestoreReasonSucceeded, appliedRestoredCondition(t, h.w).Reason)
	assert.True(t, h.volume.holds("main", podcontract.RestoreCompleteFile))
	assert.Equal(t, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID),
		h.volume.read("main", podcontract.RestoreClaimFile), "the claim stays for the container's later starts")
}

// The workload gave up waiting and claimed its container just before the
// agent came to restore it, and has not written restore-failed yet. The agent
// leaves the container to the workload: no restore, no kill, no marker of its
// own.
func TestARestoreStaysOutOfAContainerItsWorkloadClaimedFirst(t *testing.T) {
	h := newClaimHarness(t)
	h.volume.write("main", podcontract.RestoreClaimFile, claimedBy(podcontract.ControlFileOwnerWorkload, ""))

	processQueuedRestorePod(t, h.w, h.pod)

	condition := appliedRestoredCondition(t, h.w)
	assert.Equal(t, podcontract.RestoreReasonClaimedByWorkload, condition.Reason)
	assert.Equal(t, "Restore from PodSnapshot snapshot-a did not run: the workload of 1 destination container(s) "+
		"started without a restore first: main: destination container was claimed by its workload first: "+
		"it starts without a restore, so it is not restored", condition.Message)
	assert.Empty(t, h.volume.kills())
	assert.False(t, h.volume.holds("main", podcontract.RestoreFailedFile), "restore-failed is the workload's to write")
	assert.Equal(t, claimedBy(podcontract.ControlFileOwnerWorkload, ""), h.volume.read("main", podcontract.RestoreClaimFile))
	assert.Equal(t, podcontract.RestoreOutcomeFailed, podcontract.ClassifyRestoreOutcome([]corev1.PodCondition{condition}))
	assert.True(t, h.w.restoreHandled(h.pod))
}

// An agent that stopped after it claimed the container, before or during the
// restore, comes back to it. Its own claim on the same container lets the
// restore run; on a container that started since, it does not, and whatever
// waits there is told that no restore is coming.
func TestAnAgentThatStoppedAfterItClaimedComesBackToItsClaim(t *testing.T) {
	t.Run("same container", func(t *testing.T) {
		h := newClaimHarness(t)
		h.recovering()
		h.volume.write("main", podcontract.RestoreClaimFile, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID))
		calls := h.restoreInto(100, func() {})

		processQueuedRestorePod(t, h.w, h.pod)

		assert.Equal(t, 1, *calls)
		assert.Equal(t, podcontract.RestoreReasonSucceeded, appliedRestoredCondition(t, h.w).Reason)
		assert.Empty(t, h.volume.kills())
	})

	t.Run("a container that started since", func(t *testing.T) {
		h := newClaimHarness(t)
		h.recovering()
		h.volume.write("main", podcontract.RestoreClaimFile, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID))
		h.restart()

		processQueuedRestorePod(t, h.w, h.pod)

		condition := appliedRestoredCondition(t, h.w)
		assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason, "the claimed restore did not finish: a failure")
		assert.Contains(t, condition.Message, "main: destination container was claimed by the agent for an earlier container "+
			testContainerID+": its restore is used, so it is not restored again")
		assert.Empty(t, h.volume.kills(), "the container that started since is not killed")
		assert.Equal(t, string(podcontract.FormatAgentRestoreFailed()), h.volume.read("main", podcontract.RestoreFailedFile))
		assert.Equal(t, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID),
			h.volume.read("main", podcontract.RestoreClaimFile), "a lost claim changes nothing")
	})
}

// The claim outlives the container that made it. In a later container of the
// same Pod it keeps the restore out, whoever made it.
func TestAClaimFromAnEarlierContainerKeepsTheRestoreOut(t *testing.T) {
	for name, tc := range map[string]struct {
		claim      string
		wantReason string
		wantFailed bool
	}{
		"the workload's": {
			claim:      claimedBy(podcontract.ControlFileOwnerWorkload, ""),
			wantReason: podcontract.RestoreReasonClaimedByWorkload,
		},
		"the agent's": {
			claim:      claimedBy(podcontract.ControlFileOwnerAgent, testContainerID),
			wantReason: podcontract.RestoreReasonFailed,
			wantFailed: true,
		},
		// Its creator stopped before it wrote who it was.
		"one that cannot be read": {
			claim:      "",
			wantReason: podcontract.RestoreReasonFailed,
			wantFailed: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newClaimHarness(t)
			h.volume.write("main", podcontract.RestoreClaimFile, tc.claim)
			h.restart()

			processQueuedRestorePod(t, h.w, h.pod)

			assert.Equal(t, tc.wantReason, appliedRestoredCondition(t, h.w).Reason)
			assert.Empty(t, h.volume.kills())
			assert.Equal(t, tc.wantFailed, h.volume.holds("main", podcontract.RestoreFailedFile),
				"only a claim that is not the workload's gets restore-failed, so what waits for the restore stops")
			assert.Equal(t, tc.claim, h.volume.read("main", podcontract.RestoreClaimFile))
		})
	}
}

// A claim the agent cannot decide, because the control directory cannot be
// reached, is a failed restore, ended as one.
func TestARestoreThatCannotClaimItsContainerFails(t *testing.T) {
	h := newClaimHarness(t)
	h.w.claimControlFileFn = func(int, string, []byte) (bool, error) {
		return false, errors.New("open /snapshot-control: no such file or directory")
	}

	processQueuedRestorePod(t, h.w, h.pod)

	condition := appliedRestoredCondition(t, h.w)
	assert.Equal(t, podcontract.RestoreReasonFailed, condition.Reason)
	assert.Contains(t, condition.Message, "claim the destination container: open /snapshot-control")
	assert.Equal(t, []string{"main"}, h.volume.kills())
}

// Ending a restore claims each destination first. A container whose workload
// claimed it is starting without a restore and is left alone; one claimed for
// an earlier container is told that no restore is coming, without a kill.
func TestEndingARestoreKillsOnlyContainersTheAgentClaimed(t *testing.T) {
	started := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		claim      string
		wantKills  []string
		wantFailed string
	}{
		"unclaimed": {
			wantKills:  []string{"main"},
			wantFailed: string(podcontract.FormatAgentRestoreFailed()),
		},
		"claimed by its workload": {
			claim: claimedBy(podcontract.ControlFileOwnerWorkload, ""),
		},
		"claimed by the agent for an earlier container": {
			claim:      claimedBy(podcontract.ControlFileOwnerAgent, "earlier-container"),
			wantFailed: string(podcontract.FormatAgentRestoreFailed()),
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			startedAt(pod, "main", started)
			w := makeTestController(t, pod) // the PodSnapshot never appears
			w.nowFn = func() time.Time { return started.Add(61 * time.Second) }
			w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
			volume := newControlVolume(map[int]string{100: "main"})
			volume.install(w)
			if tc.claim != "" {
				volume.write("main", podcontract.RestoreClaimFile, tc.claim)
			}
			w.restoreFn = noRestoreExpected(t)

			processQueuedRestorePod(t, w, pod)

			assert.Equal(t, podcontract.RestoreReasonSnapshotNotReady, appliedRestoredCondition(t, w).Reason)
			assert.Equal(t, tc.wantKills, volume.kills())
			assert.Equal(t, tc.wantFailed, volume.read("main", podcontract.RestoreFailedFile))
			if tc.claim == "" {
				assert.Equal(t, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID),
					volume.read("main", podcontract.RestoreClaimFile))
			}
		})
	}
}

// A refusal found while restoring, after the agent claimed the container,
// still ends it: the claim is the agent's own.
func TestARestoreRefusedAfterTheClaimEndsTheContainer(t *testing.T) {
	r := newGatedRestore(t)
	r.controller.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{testContainerID: 100}}
	volume := newControlVolume(map[int]string{100: "main"})
	volume.install(r.controller)
	r.controller.restoreFn = refuseWith(compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"})

	r.runRestore(t)

	assert.Equal(t, podcontract.RestoreReasonIncompatible, r.condition(t).Reason)
	assert.Equal(t, []string{"main"}, volume.kills())
	assert.Equal(t, claimedBy(podcontract.ControlFileOwnerAgent, testContainerID), volume.read("main", podcontract.RestoreClaimFile))
	assert.True(t, volume.holds("main", podcontract.RestoreFailedFile))
}

// In a Pod with several destinations each is claimed on its own.
func TestEachDestinationIsClaimedOnItsOwn(t *testing.T) {
	for name, tc := range map[string]struct {
		decodeClaim string
		wantReason  string
		wantMessage string
	}{
		"the other one restores": {
			wantReason: podcontract.RestoreReasonPartiallySucceeded,
			wantMessage: "Restored 1 of 2 destination containers from PodSnapshots snap-prefill (prefill), snap-decode (decode); " +
				"not restored: prefill: destination container was claimed by its workload first: it starts without a restore, so it is not restored",
		},
		"its workload claimed the other one too": {
			decodeClaim: claimedBy(podcontract.ControlFileOwnerWorkload, ""),
			wantReason:  podcontract.RestoreReasonClaimedByWorkload,
			wantMessage: "Restore from PodSnapshots snap-prefill (prefill), snap-decode (decode) did not run: " +
				"the workload of 2 destination container(s) started without a restore first: " +
				"prefill: destination container was claimed by its workload first: it starts without a restore, so it is not restored; " +
				"decode: destination container was claimed by its workload first: it starts without a restore, so it is not restored",
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod := fanInRestorePod()
			w := makeFanInController(t, pod, readyFanInSources())
			w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"prefill-id": 100, "decode-id": 200, "router-id": 300}}
			volume := newControlVolume(map[int]string{100: "prefill", 200: "decode", 300: "router"})
			volume.install(w)
			volume.write("prefill", podcontract.RestoreClaimFile, claimedBy(podcontract.ControlFileOwnerWorkload, ""))
			if tc.decodeClaim != "" {
				volume.write("decode", podcontract.RestoreClaimFile, tc.decodeClaim)
			}
			var mu sync.Mutex
			var restored []string
			w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
				mu.Lock()
				defer mu.Unlock()
				restored = append(restored, req.DestinationContainerName)
				return executor.RestoreResult{PlaceholderHostPID: 200, RestoredPID: 1}, nil
			}

			processQueuedRestorePod(t, w, pod)

			condition := appliedRestoredCondition(t, w)
			assert.Equal(t, tc.wantReason, condition.Reason)
			assert.Equal(t, tc.wantMessage, condition.Message)
			assert.NotContains(t, restored, "prefill")
			assert.Empty(t, volume.kills())
			assert.False(t, volume.holds("router", podcontract.RestoreClaimFile), "a container that is no destination is never claimed")
		})
	}
}

// Destinations whose workloads started without a restore in different ways
// are reported under the reason that covers them all.
func TestRestoreVerdictWhenWorkloadsStartedWithoutARestoreInDifferentWays(t *testing.T) {
	declined := func(destination, reason string) restoreResult {
		return restoreResult{destination: destination, state: restoreResultDeclined, reason: "it started without a restore", declinedReason: reason}
	}

	all := tallyRestoreResults([]restoreResult{
		declined("prefill", podcontract.RestoreReasonClaimedByWorkload),
		declined("decode", podcontract.RestoreReasonClaimedByWorkload),
	}).verdict("snap")
	mixed := tallyRestoreResults([]restoreResult{
		declined("prefill", podcontract.RestoreReasonClaimedByWorkload),
		declined("decode", podcontract.RestoreReasonEndedByWorkload),
	}).verdict("snap")

	assert.Equal(t, podcontract.RestoreReasonClaimedByWorkload, all.reason)
	assert.Equal(t, podcontract.RestoreReasonEndedByWorkload, mixed.reason)
}
