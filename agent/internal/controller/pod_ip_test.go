// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestPrimaryPodIP(t *testing.T) {
	for name, tc := range map[string]struct {
		ips     []string
		hostIP  string
		want    string
		wantErr bool
	}{
		"one IPv4":                             {ips: []string{"10.0.0.7"}, want: "10.0.0.7"},
		"one IPv6":                             {ips: []string{"fd00::7"}, want: "fd00::7"},
		"the first of one family":              {ips: []string{"10.0.0.7", "10.0.0.8"}, want: "10.0.0.7"},
		"dual-stack on an IPv4 node":           {ips: []string{"10.0.0.7", "fd00::7"}, hostIP: "192.168.0.1", want: "10.0.0.7"},
		"dual-stack on an IPv6 node":           {ips: []string{"10.0.0.7", "fd00::7"}, hostIP: "fd00::1", want: "fd00::7"},
		"the node's family before the primary": {ips: []string{"fd00::7", "10.0.0.7"}, hostIP: "192.168.0.1", want: "10.0.0.7"},
		"an IPv4-mapped address is IPv4":       {ips: []string{"::ffff:10.0.0.7", "fd00::7"}, hostIP: "192.168.0.1", want: "::ffff:10.0.0.7"},
		"dual-stack without a host IP":         {ips: []string{"10.0.0.7", "fd00::7"}, wantErr: true},
		"no IP":                                {wantErr: true},
		"an IP that does not parse":            {ips: []string{"10.0.0.7", "not-an-ip"}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := primaryPodIP(tc.ips, tc.hostIP)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// tcpRestore is a restore-from Pod with a Ready snapshot and its artifact,
// on an agent that restores established TCP connections.
func tcpRestore(t *testing.T, runtime *fakeRuntime) (*NodeController, *corev1.Pod) {
	t.Helper()
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.config.CRIU.TcpEstablished = true
	w.runtime = runtime
	writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"},
	})
	return w, pod
}

// targetPodIPs records the TargetPodIP of each restore request.
func targetPodIPs(w *NodeController) *[]string {
	var mu sync.Mutex
	var ips []string
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ips = append(ips, req.TargetPodIP)
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 1}, nil
	}
	return &ips
}

// kubelet publishes the Pod IP only at its next relist; the sandbox already
// has it.
func TestPreflightRestoreUsesTheSandboxIPBeforeTheAPIHasIt(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7", plan.podIP)
	assert.Equal(t, []string{string(pod.UID)}, runtime.sandboxPodUIDs)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"10.0.0.7"}, *targets)
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreSucceeded"`)
}

func TestPreflightRestorePrefersTheAPIPodIP(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	pod.Status.PodIP = "10.0.0.9"
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Empty(t, plan.podIP)
	assert.Empty(t, runtime.sandboxPodUIDs, "the runtime is not asked when the API has the IP")

	w.reconcileRestorePod(context.Background(), pod)

	assert.Equal(t, []string{"10.0.0.9"}, *targets)
}

func TestPreflightRestoreWaitsWhileTheSandboxHasNoUsableIP(t *testing.T) {
	for name, tc := range map[string]struct {
		sandboxIPs []string
		hostIP     string
	}{
		"the runtime call fails":       {},
		"dual-stack without a host IP": {sandboxIPs: []string{"10.0.0.7", "fd00::7"}},
		"an IP that does not parse":    {sandboxIPs: []string{"not-an-ip"}},
	} {
		t.Run(name, func(t *testing.T) {
			w, pod := tcpRestore(t, &fakeRuntime{sandboxIPs: tc.sandboxIPs})
			pod.Status.HostIP = tc.hostIP
			w.restoreFn = noRestoreExpected(t)

			plan, err := w.preflightRestore(context.Background(), pod)

			assert.Nil(t, plan)
			assert.Equal(t, "PodIPPending", pendingRestoreReason(t, err))
			assert.EqualError(t, err, "Waiting for restore Pod inference/restore-worker to receive an IP address")
		})
	}
}

func TestPreflightRestoreUsesTheSandboxIPOfTheNodesFamily(t *testing.T) {
	w, pod := tcpRestore(t, &fakeRuntime{sandboxIPs: []string{"fd00::7", "10.0.0.7"}})
	pod.Status.HostIP = "192.168.0.1"

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7", plan.podIP)
}

func TestPreflightRestoreWithoutTCPRestoreNeedsNoPodIP(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	w.config.CRIU.TcpEstablished = false
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Empty(t, plan.podIP)
	assert.Empty(t, runtime.sandboxPodUIDs)

	w.reconcileRestorePod(context.Background(), pod)

	assert.Equal(t, []string{""}, *targets)
}

func TestPreflightRestoreFromMapUsesTheSandboxIPForEveryDestination(t *testing.T) {
	pod := fanInRestorePod()
	w := makeFanInController(t, pod, readyFanInSources())
	w.config.CRIU.TcpEstablished = true
	w.runtime = &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	targets := targetPodIPs(w)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"10.0.0.7", "10.0.0.7"}, *targets)
}
