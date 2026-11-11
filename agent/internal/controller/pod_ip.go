// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
)

// preflightPodIP keeps a restore that restores established TCP connections
// pending until the restore Pod has an IP address. The API's status.podIP
// comes first. kubelet publishes it only at its next relist after the sandbox
// starts, up to a second later, so until then the IP comes from the Pod's
// sandbox in the node runtime. That IP is returned; it is empty when the API
// has one or the restore needs none.
func (w *NodeController) preflightPodIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	if !w.config.CRIU.TcpEstablished || pod.Status.PodIP != "" {
		return "", nil
	}
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	ip, err := w.sandboxPodIP(ctx, pod)
	if err != nil {
		w.log.V(1).Info("Restore Pod has no IP address in the API or its sandbox yet", "pod", podKey, "error", err.Error())
		return "", newRestorePendingError("PodIPPending", fmt.Sprintf("Waiting for restore Pod %s to receive an IP address", podKey))
	}
	w.log.V(1).Info("Using the restore Pod's IP address from its sandbox", "pod", podKey, "pod_ip", ip)
	return ip, nil
}

// sandboxPodIP returns the IP that kubelet will publish as the Pod's
// status.podIP, from the IPs the node runtime reports for its sandbox. The
// runtime knows the Pod by its UID on the node; in a vCluster tenant that UID
// differs, no sandbox matches, and the restore waits for the API.
func (w *NodeController) sandboxPodIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	ips, err := w.runtime.ResolvePodSandboxIPs(ctx, string(pod.UID))
	if err != nil {
		return "", err
	}
	return primaryPodIP(ips, pod.Status.HostIP)
}

// primaryPodIP picks status.podIP from a sandbox's IPs the way kubelet does:
// the first IP of the node's primary IP family, or else the first of the other
// family. kubelet sets the Pod's status.hostIP to the node's primary IP, so
// hostIP gives the family. Without it, only IPs of one family can be told
// apart, and IPs of both are an error. kubelet publishes no IP when one does
// not parse, so neither does this.
func primaryPodIP(ips []string, hostIP string) (string, error) {
	var ipv4, ipv6 string
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			return "", fmt.Errorf("sandbox reports an invalid IP address %q", ip)
		}
		if addr.Unmap().Is4() {
			ipv4 = cmp.Or(ipv4, ip)
		} else {
			ipv6 = cmp.Or(ipv6, ip)
		}
	}
	switch {
	case ipv4 == "" && ipv6 == "":
		return "", errors.New("sandbox reports no IP address")
	case ipv6 == "":
		return ipv4, nil
	case ipv4 == "":
		return ipv6, nil
	}
	host, err := netip.ParseAddr(hostIP)
	if err != nil {
		return "", errors.New("sandbox reports IPv4 and IPv6 addresses, and the Pod has no host IP to choose between them")
	}
	if host.Unmap().Is4() {
		return ipv4, nil
	}
	return ipv6, nil
}
