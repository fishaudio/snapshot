// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

type restoreOptions struct {
	ManifestPath string
	Namespace    string
	KubeContext  string
	SnapshotName string
}

func runRestoreFlow(ctx context.Context, opts restoreOptions) (*result, error) {
	if strings.TrimSpace(opts.ManifestPath) == "" {
		return nil, fmt.Errorf("restore requires --manifest")
	}
	pod, clientset, crClient, namespace, err := loadRunContext(opts.ManifestPath, opts.Namespace, opts.KubeContext)
	if err != nil {
		return nil, err
	}
	request, err := restoreRequest(pod, strings.TrimSpace(opts.SnapshotName), func(name string) (*snapshotv1alpha1.PodSnapshot, error) {
		snapshot := &snapshotv1alpha1.PodSnapshot{}
		if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, snapshot); err != nil {
			return nil, fmt.Errorf("get PodSnapshot %s/%s: %w", namespace, name, err)
		}
		return snapshot, nil
	})
	if err != nil {
		return nil, err
	}

	candidate := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:         pod.Name,
			GenerateName: pod.GenerateName,
			Namespace:    namespace,
			Labels:       pod.Labels,
			Annotations:  pod.Annotations,
		},
		Spec: *pod.Spec.DeepCopy(),
	}
	candidate.Spec.RestartPolicy = corev1.RestartPolicyNever
	restorePod, err := podcontract.Build(
		candidate,
		request,
		podcontract.Options{
			SeccompProfile:    podcontract.DefaultSeccompLocalhostProfile,
			EnableStartupGate: true,
		},
	)
	if err != nil {
		return nil, err
	}
	restorePod, err = clientset.CoreV1().Pods(namespace).Create(ctx, restorePod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("restore pod %s/%s already exists", namespace, pod.Name)
	}
	if err != nil {
		return nil, err
	}
	return &result{
		Name:        restorePod.Name,
		Namespace:   namespace,
		PodSnapshot: strings.Join(requestedSnapshots(request), ","),
		RestorePod:  restorePod.Name,
		Status:      "requested",
	}, nil
}

// restoreRequest says what the manifest's Pod restores from: the PodSnapshot
// named by --snapshot, or one PodSnapshot per destination container named by
// the manifest's restore-from-map annotation. getSnapshot reads a PodSnapshot
// in the restore namespace, so each one is checked before the Pod is created.
func restoreRequest(
	pod *corev1.Pod,
	snapshotName string,
	getSnapshot func(name string) (*snapshotv1alpha1.PodSnapshot, error),
) (podcontract.Request, error) {
	snapshotMappings, err := podcontract.SnapshotMappingsFromAnnotations(pod.Annotations)
	if err != nil {
		return podcontract.Request{}, err
	}
	if snapshotMappings != nil {
		if snapshotName != "" {
			return podcontract.Request{}, fmt.Errorf("--snapshot cannot be combined with %s in the manifest", podcontract.RestoreFromMapAnnotation)
		}
		request := podcontract.Request{SnapshotMappings: snapshotMappings}
		for _, name := range requestedSnapshots(request) {
			snapshot, err := getSnapshot(name)
			if err != nil {
				return podcontract.Request{}, err
			}
			if _, err := checkpointedContainer(snapshot); err != nil {
				return podcontract.Request{}, err
			}
		}
		return request, nil
	}
	if snapshotName == "" {
		return podcontract.Request{}, fmt.Errorf("--snapshot is required unless the manifest sets %s", podcontract.RestoreFromMapAnnotation)
	}
	snapshot, err := getSnapshot(snapshotName)
	if err != nil {
		return podcontract.Request{}, err
	}
	container, err := checkpointedContainer(snapshot)
	if err != nil {
		return podcontract.Request{}, err
	}
	mappings, err := podcontract.ContainerMappingsFromAnnotations(pod.Annotations, container)
	if err != nil {
		return podcontract.Request{}, err
	}
	return podcontract.Request{SnapshotName: snapshotName, SourceContainer: container, Mappings: mappings}, nil
}

// checkpointedContainer returns the one container the PodSnapshot checkpoints.
func checkpointedContainer(snapshot *snapshotv1alpha1.PodSnapshot) (string, error) {
	containers := snapshot.Spec.Source.PodRef.Containers
	if len(containers) != 1 || strings.TrimSpace(containers[0]) == "" {
		return "", fmt.Errorf("PodSnapshot %s/%s must checkpoint exactly one container", snapshot.Namespace, snapshot.Name)
	}
	return containers[0], nil
}

// requestedSnapshots lists the PodSnapshots a request restores from, each
// once, in the order it names them.
func requestedSnapshots(request podcontract.Request) []string {
	if len(request.SnapshotMappings) == 0 {
		return []string{request.SnapshotName}
	}
	var names []string
	seen := make(map[string]struct{}, len(request.SnapshotMappings))
	for _, mapping := range request.SnapshotMappings {
		if _, found := seen[mapping.SnapshotName]; !found {
			seen[mapping.SnapshotName] = struct{}{}
			names = append(names, mapping.SnapshotName)
		}
	}
	return names
}
