// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// snapshotsCheckpointing serves PodSnapshots that each checkpoint the given
// containers, and records which ones were read.
func snapshotsCheckpointing(containers map[string][]string, read *[]string) func(string) (*snapshotv1alpha1.PodSnapshot, error) {
	return func(name string) (*snapshotv1alpha1.PodSnapshot, error) {
		*read = append(*read, name)
		checkpointed, found := containers[name]
		if !found {
			return nil, errors.New("not found")
		}
		return &snapshotv1alpha1.PodSnapshot{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "inference"},
			Spec: snapshotv1alpha1.PodSnapshotSpec{Source: snapshotv1alpha1.PodSnapshotSource{
				PodRef: snapshotv1alpha1.PodReference{Name: "source", Containers: checkpointed},
			}},
		}, nil
	}
}

func manifestPod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "restored", Annotations: annotations}}
}

func TestRestoreRequest(t *testing.T) {
	snapshots := map[string][]string{
		"worker-snapshot": {"main"},
		"snap-prefill":    {"prefill"},
		"snap-decode":     {"decode"},
		"two-containers":  {"a", "b"},
	}

	t.Run("--snapshot", func(t *testing.T) {
		var read []string
		pod := manifestPod(map[string]string{podcontract.RestoreContainerMapAnnotation: "main=engine-0,main=engine-1"})

		request, err := restoreRequest(pod, "worker-snapshot", snapshotsCheckpointing(snapshots, &read))

		require.NoError(t, err)
		assert.Equal(t, podcontract.Request{
			SnapshotName:    "worker-snapshot",
			SourceContainer: "main",
			Mappings: []podcontract.ContainerMapping{
				{Source: "main", Destination: "engine-0"},
				{Source: "main", Destination: "engine-1"},
			},
		}, request)
		assert.Equal(t, []string{"worker-snapshot"}, requestedSnapshots(request))
	})

	t.Run("restore-from-map in the manifest", func(t *testing.T) {
		var read []string
		pod := manifestPod(map[string]string{
			podcontract.RestoreFromMapAnnotation: "prefill=snap-prefill,decode=snap-decode,decode-1=snap-decode",
		})

		request, err := restoreRequest(pod, "", snapshotsCheckpointing(snapshots, &read))

		require.NoError(t, err)
		assert.Equal(t, []podcontract.SnapshotMapping{
			{Destination: "prefill", SnapshotName: "snap-prefill"},
			{Destination: "decode", SnapshotName: "snap-decode"},
			{Destination: "decode-1", SnapshotName: "snap-decode"},
		}, request.SnapshotMappings)
		assert.Equal(t, []string{"snap-prefill", "snap-decode"}, read, "each PodSnapshot is checked once")
		assert.Equal(t, []string{"snap-prefill", "snap-decode"}, requestedSnapshots(request))
	})

	for name, tc := range map[string]struct {
		annotations map[string]string
		snapshot    string
	}{
		"no PodSnapshot named":            {},
		"--snapshot and restore-from-map": {annotations: map[string]string{podcontract.RestoreFromMapAnnotation: "prefill=snap-prefill"}, snapshot: "snap-prefill"},
		"a PodSnapshot that is missing":   {annotations: map[string]string{podcontract.RestoreFromMapAnnotation: "prefill=snap-prefill,decode=missing"}},
		"a PodSnapshot of two containers": {
			annotations: map[string]string{podcontract.RestoreFromMapAnnotation: "prefill=two-containers"},
		},
		"restore-from-map and restore-from": {annotations: map[string]string{
			podcontract.RestoreFromMapAnnotation: "prefill=snap-prefill",
			podcontract.RestoreFromAnnotation:    "snap-prefill",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var read []string

			_, err := restoreRequest(manifestPod(tc.annotations), tc.snapshot, snapshotsCheckpointing(snapshots, &read))

			require.Error(t, err)
		})
	}
}
