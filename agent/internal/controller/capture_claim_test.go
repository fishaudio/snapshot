// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func notFinished(string) bool { return false }
func finishedAll(string) bool { return true }

func TestCaptureClaims_TheCachesOwnerTakesAFreeClaim(t *testing.T) {
	var c captureClaims
	pod := makeSourcePod()

	owner, release := c.claim(pod, "b", "a", notFinished)
	assert.Equal(t, "a", owner, "a work order the cache does not name waits for the one it names")
	assert.Nil(t, release)

	owner, release = c.claim(pod, "a", "a", notFinished)
	assert.Equal(t, "a", owner)
	require.NotNil(t, release)
	release()

	owner, release = c.claim(pod, "a", "b", notFinished)
	assert.Equal(t, "a", owner, "the owner keeps its claim on later passes, whatever the cache names")
	require.NotNil(t, release)
	release()
}

// The race the claim closes: the cache names another owner once a sibling created in the same
// second arrives, but the work order that already began keeps the pod.
func TestCaptureClaims_ASiblingTheCacheNowNamesWaits(t *testing.T) {
	var c captureClaims
	pod := makeSourcePod()

	_, release := c.claim(pod, "podsnapshotcontent-b", "podsnapshotcontent-b", notFinished)
	require.NotNil(t, release)

	owner, waiting := c.claim(pod, "podsnapshotcontent-a", "podsnapshotcontent-a", notFinished)
	assert.Equal(t, "podsnapshotcontent-b", owner)
	assert.Nil(t, waiting)
	release()

	owner, waiting = c.claim(pod, "podsnapshotcontent-a", "podsnapshotcontent-a", notFinished)
	assert.Equal(t, "podsnapshotcontent-b", owner, "between the owner's passes the pod stays its")
	assert.Nil(t, waiting)
}

func TestCaptureClaims_HandOverOnlyWhenTheOwnerIsDoneAndNotRunning(t *testing.T) {
	var c captureClaims
	pod := makeSourcePod()

	_, release := c.claim(pod, "a", "a", notFinished)
	require.NotNil(t, release)

	owner, waiting := c.claim(pod, "b", "b", finishedAll)
	assert.Equal(t, "a", owner, "a pass of the owner is running: even a deleted owner keeps the pod until it returns")
	assert.Nil(t, waiting)

	release()
	owner, waiting = c.claim(pod, "b", "b", notFinished)
	assert.Equal(t, "a", owner, "the cache does not show the owner done yet")
	assert.Nil(t, waiting)

	owner, releaseB := c.claim(pod, "b", "b", func(name string) bool { return name == "a" })
	assert.Equal(t, "b", owner)
	require.NotNil(t, releaseB)
	releaseB()

	assert.Empty(t, c.finish("a"), "a work order that holds no claim drops none")
	assert.Equal(t, []string{"inference/worker-0"}, c.finish("b"))

	owner, waiting = c.claim(pod, "c", "b", notFinished)
	assert.Equal(t, "b", owner, "a free claim still goes to the work order the cache names")
	assert.Nil(t, waiting)
}

func TestCaptureClaims_FinishKeepsARunningClaim(t *testing.T) {
	var c captureClaims
	pod := makeSourcePod()

	_, release := c.claim(pod, "a", "a", notFinished)
	require.NotNil(t, release)
	assert.Empty(t, c.finish("a"))
	release()
	assert.Equal(t, []string{"inference/worker-0"}, c.finish("a"))
}

func TestCaptureClaims_ClaimsArePerPod(t *testing.T) {
	var c captureClaims
	pod := makeSourcePod()
	other := makeSourcePod()
	other.Name = "worker-1"
	other.UID = "other-pod-uid"

	_, release := c.claim(pod, "a", "a", notFinished)
	require.NotNil(t, release)
	owner, releaseOther := c.claim(other, "b", "b", notFinished)
	assert.Equal(t, "b", owner)
	require.NotNil(t, releaseOther)
}

// Workers whose caches disagree all believe they are the owner; exactly one gets the pod.
func TestCaptureClaims_OneOfManyConcurrentClaimantsWins(t *testing.T) {
	for round := range 50 {
		var c captureClaims
		pod := makeSourcePod()
		const claimants = 8
		owners := make([]string, claimants)
		won := make([]bool, claimants)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range claimants {
			wg.Go(func() {
				<-start
				name := fmt.Sprintf("content-%d", i)
				owner, release := c.claim(pod, name, name, notFinished)
				owners[i] = owner
				won[i] = release != nil
			})
		}
		close(start)
		wg.Wait()

		winners := 0
		for i := range claimants {
			if won[i] {
				winners++
			}
		}
		require.Equal(t, 1, winners, "round %d", round)
		for i := range claimants {
			assert.Equal(t, owners[0], owners[i], "round %d: every claimant names the same owner", round)
		}
	}
}

// twoContainerCaptures returns the two work orders of a pod with two snapshot containers, created
// in the same second, and the pod with both containers running and ready. The decode work order's
// name sorts after the prefill one's, so the cache names prefill the owner once it holds both.
func twoContainerCaptures() (*snapshotv1alpha1.PodSnapshotContent, *snapshotv1alpha1.PodSnapshotContent, *corev1.Pod) {
	created := metav1.Unix(1000, 0)
	decode := makeWorkOrder("podsnapshotcontent-b", "node-a", "decode")
	decode.CreationTimestamp = created
	decode.Spec.Source.PodRef.Containers = []string{"decode"}
	prefill := makeWorkOrder("podsnapshotcontent-a", "node-a", "prefill")
	prefill.CreationTimestamp = created
	prefill.Spec.Source.PodRef.Containers = []string{"prefill"}
	pod := makeSourcePod()
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "prefill", Ready: true, ContainerID: "containerd://prefill-id",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: "decode", Ready: true, ContainerID: "containerd://decode-id",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	return decode, prefill, pod
}

// dumpRecorder stands in for the dump: it records each dumped container, and holds the dump of
// blockOn until release is closed.
type dumpRecorder struct {
	mu      sync.Mutex
	dumped  []string
	blockOn string
	started chan struct{}
	release chan struct{}
}

func newDumpRecorder(blockOn string) *dumpRecorder {
	return &dumpRecorder{blockOn: blockOn, started: make(chan struct{}), release: make(chan struct{})}
}

func (d *dumpRecorder) fn(_ context.Context, params CheckpointParams) (*pagebroker.PublishedArtifact, error) {
	d.mu.Lock()
	d.dumped = append(d.dumped, params.ContainerName)
	d.mu.Unlock()
	if params.ContainerName == d.blockOn {
		close(d.started)
		<-d.release
	}
	return nil, nil
}

func (d *dumpRecorder) containers() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dumped...)
}

// cacheContent mirrors the informer: it stores the API's current copy of the work order in the cache.
func cacheContent(t *testing.T, w *NodeController, name string) {
	t.Helper()
	require.NoError(t, w.contentIndexer.Update(mustUnstructured(t, getContent(t, w, name))))
}

func uncacheContent(t *testing.T, w *NodeController, name string) {
	t.Helper()
	obj, exists, err := w.contentIndexer.GetByKey(name)
	require.NoError(t, err)
	require.True(t, exists)
	require.NoError(t, w.contentIndexer.Delete(obj))
}

// The capture that began while it was the only work order cached keeps the pod: the sibling the
// cache names once it holds both waits for that dump, and then for its outcome to reach the cache.
func TestReconcileCapture_SiblingWaitsForTheCaptureThatBegan(t *testing.T) {
	decode, prefill, pod := twoContainerCaptures()
	w := makeNodeController(t, &fakeCheckpointer{}, decode, prefill, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	dumps := newDumpRecorder("decode")
	w.checkpointFn = dumps.fn
	ctx := context.Background()

	// The decode worker starts while only its own work order is cached.
	uncacheContent(t, w, prefill.Name)
	done := make(chan error, 1)
	go func() { done <- w.reconcileCapture(ctx, decode.Name) }()
	<-dumps.started

	// The prefill work order arrives; it sorts first, so the cache now names it the owner.
	cacheContent(t, w, prefill.Name)
	chosen, err := w.captureOwnerForPod(pod)
	require.NoError(t, err)
	require.Equal(t, prefill.Name, chosen)

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	assert.Equal(t, []string{"decode"}, dumps.containers(), "a second dump must not start while the first runs")
	assert.Empty(t, getContent(t, w, prefill.Name).Status.Conditions)

	close(dumps.release)
	require.NoError(t, <-done)
	assert.True(t, isContentReady(getContent(t, w, decode.Name)))

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	assert.Equal(t, []string{"decode"}, dumps.containers(),
		"the next capture waits until the cache shows the first one done, which its crash sweep reads")

	// The informer delivers the decode work order's Ready; its reconcile hands the pod on.
	cacheContent(t, w, decode.Name)
	require.Equal(t, 0, w.captureQueue.Len())
	require.NoError(t, w.reconcileCapture(ctx, decode.Name))
	require.Equal(t, 1, w.captureQueue.Len())
	next, _ := w.captureQueue.Get()
	assert.Equal(t, prefill.Name, next)
	w.captureQueue.Done(next)

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	assert.Equal(t, []string{"decode", "prefill"}, dumps.containers())
	assert.True(t, isContentReady(getContent(t, w, prefill.Name)))
}

// A work order deleted during its dump keeps the pod until that pass returns; its last reconcile,
// with the work order gone from the cache, hands the pod on.
func TestReconcileCapture_DeletedOwnerKeepsThePodUntilItsDumpReturns(t *testing.T) {
	decode, prefill, pod := twoContainerCaptures()
	w := makeNodeController(t, &fakeCheckpointer{}, decode, prefill, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	dumps := newDumpRecorder("decode")
	w.checkpointFn = dumps.fn
	ctx := context.Background()

	uncacheContent(t, w, prefill.Name)
	done := make(chan error, 1)
	go func() { done <- w.reconcileCapture(ctx, decode.Name) }()
	<-dumps.started
	cacheContent(t, w, prefill.Name)
	uncacheContent(t, w, decode.Name)

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	assert.Equal(t, []string{"decode"}, dumps.containers())

	close(dumps.release)
	require.NoError(t, <-done)

	require.NoError(t, w.reconcileCapture(ctx, decode.Name))
	require.Equal(t, 1, w.captureQueue.Len())

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	assert.Equal(t, []string{"decode", "prefill"}, dumps.containers())
}

// One after the other, as when the caches agree from the start: the second waits for the first.
func TestReconcileCapture_TwoContainersOfAPodAreCapturedInTurn(t *testing.T) {
	decode, prefill, pod := twoContainerCaptures()
	w := makeNodeController(t, &fakeCheckpointer{}, decode, prefill, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	dumps := newDumpRecorder("")
	w.checkpointFn = dumps.fn
	ctx := context.Background()

	require.NoError(t, w.reconcileCapture(ctx, decode.Name))
	assert.Empty(t, dumps.containers(), "the cache names the prefill work order the owner")

	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	require.NoError(t, w.reconcileCapture(ctx, decode.Name))
	assert.Equal(t, []string{"prefill"}, dumps.containers())

	cacheContent(t, w, prefill.Name)
	require.NoError(t, w.reconcileCapture(ctx, prefill.Name))
	require.NoError(t, w.reconcileCapture(ctx, decode.Name))
	assert.Equal(t, []string{"prefill", "decode"}, dumps.containers())
}
