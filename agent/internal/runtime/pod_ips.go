// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// podSandboxIPsTimeout bounds both calls. The restore preflight waits on them.
const podSandboxIPsTimeout = 2 * time.Second

type podSandboxService interface {
	ListPodSandbox(context.Context, *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error)
	PodSandboxStatus(context.Context, string, bool) (*runtimeapi.PodSandboxStatusResponse, error)
}

// resolvePodSandboxIPs returns the IPs that the runtime reports for the ready
// sandbox of the Pod with podUID, found by the label kubelet sets on it: the
// primary IP first, then the additional ones, the order kubelet reads them in.
// A sandbox on the host network has none.
func resolvePodSandboxIPs(ctx context.Context, svc podSandboxService, podUID string) ([]string, error) {
	if podUID == "" {
		return nil, errors.New("pod UID is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, podSandboxIPsTimeout)
	defer cancel()

	sandboxes, err := svc.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{
		State:         &runtimeapi.PodSandboxStateValue{State: runtimeapi.PodSandboxState_SANDBOX_READY},
		LabelSelector: map[string]string{kubeletPodUIDLabel: podUID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes of pod %s: %w", podUID, err)
	}
	// The filter is checked again: another Pod's IP must never be used.
	sandboxes = slices.DeleteFunc(sandboxes, func(sandbox *runtimeapi.PodSandbox) bool {
		return sandbox.GetState() != runtimeapi.PodSandboxState_SANDBOX_READY ||
			sandbox.GetLabels()[kubeletPodUIDLabel] != podUID
	})
	if len(sandboxes) == 0 {
		return nil, fmt.Errorf("pod %s has no ready sandbox", podUID)
	}
	// While kubelet replaces a Pod's sandbox, the newest one is current.
	newest := slices.MaxFunc(sandboxes, func(a, b *runtimeapi.PodSandbox) int {
		return cmp.Compare(a.GetCreatedAt(), b.GetCreatedAt())
	})
	response, err := svc.PodSandboxStatus(ctx, newest.GetId(), false)
	if err != nil {
		return nil, fmt.Errorf("failed to get status of sandbox %s: %w", newest.GetId(), err)
	}
	network := response.GetStatus().GetNetwork()
	var ips []string
	if ip := network.GetIp(); ip != "" {
		ips = append(ips, ip)
	}
	for _, additional := range network.GetAdditionalIps() {
		ips = append(ips, additional.GetIp())
	}
	return ips, nil
}
