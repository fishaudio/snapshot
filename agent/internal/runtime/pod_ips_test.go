// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakePodSandboxService struct {
	sandboxes []*runtimeapi.PodSandbox
	listErr   error
	statuses  map[string]*runtimeapi.PodSandboxStatus
	statusErr error

	filter    *runtimeapi.PodSandboxFilter
	statusIDs []string
}

func (f *fakePodSandboxService) ListPodSandbox(_ context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	f.filter = filter
	return f.sandboxes, f.listErr
}

func (f *fakePodSandboxService) PodSandboxStatus(_ context.Context, id string, _ bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.statusIDs = append(f.statusIDs, id)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &runtimeapi.PodSandboxStatusResponse{Status: f.statuses[id]}, nil
}

func readySandbox(id, podUID string, createdAt int64) *runtimeapi.PodSandbox {
	return &runtimeapi.PodSandbox{
		Id:        id,
		State:     runtimeapi.PodSandboxState_SANDBOX_READY,
		CreatedAt: createdAt,
		Labels:    map[string]string{kubeletPodUIDLabel: podUID},
	}
}

func sandboxNetwork(ip string, additional ...string) *runtimeapi.PodSandboxStatus {
	network := &runtimeapi.PodSandboxNetworkStatus{Ip: ip}
	for _, extra := range additional {
		network.AdditionalIps = append(network.AdditionalIps, &runtimeapi.PodIP{Ip: extra})
	}
	return &runtimeapi.PodSandboxStatus{Network: network}
}

func TestResolvePodSandboxIPs(t *testing.T) {
	t.Run("primary IP first, then the additional ones", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"sandbox": sandboxNetwork("10.0.0.7", "fd00::7")},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.7", "fd00::7"}, ips)
		assert.Equal(t, runtimeapi.PodSandboxState_SANDBOX_READY, svc.filter.GetState().GetState())
		assert.Equal(t, map[string]string{kubeletPodUIDLabel: "pod-uid"}, svc.filter.GetLabelSelector())
	})

	t.Run("the newest of the Pod's ready sandboxes", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{
				readySandbox("old", "pod-uid", 1),
				readySandbox("new", "pod-uid", 2),
			},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"old": sandboxNetwork("10.0.0.6"),
				"new": sandboxNetwork("10.0.0.7"),
			},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.7"}, ips)
	})

	// A runtime that ignored the filter would otherwise hand over another
	// Pod's IP.
	t.Run("sandboxes the filter should have dropped", func(t *testing.T) {
		notReady := readySandbox("not-ready", "pod-uid", 3)
		notReady.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("other-pod", "other-uid", 2), notReady},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"other-pod": sandboxNetwork("10.0.0.8"),
				"not-ready": sandboxNetwork("10.0.0.9"),
			},
		}

		_, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.Error(t, err)
		assert.Empty(t, svc.statusIDs)
	})

	t.Run("host network", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"sandbox": {}},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Empty(t, ips)
	})

	for name, svc := range map[string]*fakePodSandboxService{
		"no sandbox":   {},
		"list fails":   {listErr: errors.New("unavailable")},
		"status fails": {sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 1)}, statusErr: errors.New("not found")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

			require.Error(t, err)
		})
	}

	t.Run("no Pod UID", func(t *testing.T) {
		svc := &fakePodSandboxService{}

		_, err := resolvePodSandboxIPs(context.Background(), svc, "")

		require.Error(t, err)
		assert.Nil(t, svc.filter, "an empty UID would select every sandbox")
	})
}
