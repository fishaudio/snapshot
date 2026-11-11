// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// restoreSource is one PodSnapshot of a restore-from-map Pod during preflight.
type restoreSource struct {
	// group holds the annotation entries that name this PodSnapshot.
	group []podcontract.SnapshotMapping
	plan  *restorePlan
	// action is the post-restore action saved with the checkpoint.
	action *snapshotv1alpha1.PostRestoreAction
}

// preflightRestoreFromMap is preflightRestore for a restore-from-map Pod. It
// runs the same checks once per PodSnapshot, for the destinations that restore
// from it, and returns a plan with one source per PodSnapshot. A terminal
// error from any PodSnapshot fails the Pod. Otherwise a pending PodSnapshot
// keeps the whole Pod pending, so no destination starts before every
// PodSnapshot is ready.
func (w *NodeController) preflightRestoreFromMap(ctx context.Context, pod *corev1.Pod) (*restorePlan, error) {
	snapshotMappings, err := podcontract.SnapshotMappingsFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, err
	}
	// The destinations are checked without any PodSnapshot, so a Pod that
	// breaks the contract fails before any API read.
	if err := podcontract.Validate(pod, podcontract.Request{SnapshotMappings: snapshotMappings}); err != nil {
		return nil, err
	}

	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	var sources []restoreSource
	var pending []*restorePendingError
	for _, group := range groupBySnapshot(snapshotMappings) {
		plan, action, err := w.resolveRestoreSource(ctx, pod, podKey, group)
		if err == nil {
			sources = append(sources, restoreSource{group: group, plan: plan, action: action})
			continue
		}
		err = withRestoreSource(group, err)
		var pendingErr *restorePendingError
		if !errors.As(err, &pendingErr) {
			return nil, err
		}
		pending = append(pending, pendingErr)
	}
	if len(pending) != 0 {
		return nil, joinPendingErrors(pending)
	}

	skipCompatCheck := w.skipCompatCheckRequested(pod)
	for _, source := range sources {
		err := w.preflightCompatibility(ctx, pod, source.plan.artifact, source.plan.mappings, skipCompatCheck)
		if err != nil {
			return nil, withRestoreSource(source.group, err)
		}
	}
	if err := w.preflightPodIP(pod); err != nil {
		return nil, err
	}
	plan := &restorePlan{skipCompatCheck: skipCompatCheck}
	for _, source := range sources {
		source.plan.postRestore, source.plan.postRestorePeriod, err = planPostRestore(
			pod, source.plan.mappings, source.action, w.hookExec, w.hookHTTP)
		if err != nil {
			return nil, withRestoreSource(source.group, err)
		}
		plan.sources = append(plan.sources, source.plan)
	}
	for _, mapping := range snapshotMappings {
		plan.mappings = append(plan.mappings, podcontract.ContainerMapping{
			Source:      plan.sourceFor(mapping.Destination).artifact.SourceContainerName,
			Destination: mapping.Destination,
		})
	}
	return plan, nil
}

// resolveRestoreSource reads one PodSnapshot of a restore-from-map Pod and its
// bound content, and resolves the artifact that group's destinations restore
// from. It also returns the checkpoint's post-restore action.
func (w *NodeController) resolveRestoreSource(
	ctx context.Context,
	pod *corev1.Pod,
	podKey string,
	group []podcontract.SnapshotMapping,
) (*restorePlan, *snapshotv1alpha1.PostRestoreAction, error) {
	snapshot, err := w.getPodSnapshot(ctx, pod, group[0].SnapshotName)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePodSnapshotForRestore(snapshot); err != nil {
		return nil, nil, err
	}
	content, err := w.getPodSnapshotContentFromSnapshot(ctx, pod, snapshot)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePodSnapshotContentForRestore(content); err != nil {
		return nil, nil, err
	}
	sourceContainer, err := singleTargetContainer(content.Spec.Source.PodRef.Containers)
	if err != nil {
		return nil, nil, err
	}
	mappings := make([]podcontract.ContainerMapping, 0, len(group))
	for _, mapping := range group {
		mappings = append(mappings, podcontract.ContainerMapping{Source: sourceContainer, Destination: mapping.Destination})
	}
	if err := podcontract.ValidateContainerMappings(mappings, sourceContainer); err != nil {
		return nil, nil, err
	}
	target := &restoreTarget{SnapshotName: snapshot.Name, ContentUID: string(content.UID), SourceContainerName: sourceContainer}
	artifact, err := w.resolveRestoreArtifact(podKey, target, content)
	if err != nil {
		return nil, nil, err
	}
	return &restorePlan{artifact: artifact, mappings: mappings}, postRestoreAction(content, sourceContainer), nil
}

// sourceFor returns the plan of the PodSnapshot that destination restores
// from: its source in a restore-from-map plan, or the plan itself.
func (p *restorePlan) sourceFor(destination string) *restorePlan {
	for _, source := range p.sources {
		for _, mapping := range source.mappings {
			if mapping.Destination == destination {
				return source
			}
		}
	}
	return p
}

// snapshotOf names the PodSnapshot that destination of a restore-from-map Pod
// restores from. It is empty for a restore-from Pod.
func (p *restorePlan) snapshotOf(destination string) string {
	if source := p.sourceFor(destination); source != p {
		return source.artifact.SnapshotName
	}
	return ""
}

// from names what the plan restores from, for the Pod's condition and events.
func (p *restorePlan) from() string {
	if len(p.sources) == 0 {
		return "PodSnapshot " + p.artifact.SnapshotName
	}
	mappings := make([]podcontract.SnapshotMapping, 0, len(p.mappings))
	for _, mapping := range p.mappings {
		mappings = append(mappings, podcontract.SnapshotMapping{
			Destination:  mapping.Destination,
			SnapshotName: p.snapshotOf(mapping.Destination),
		})
	}
	return describeSnapshotMappings(mappings)
}

// groupBySnapshot splits restore-from-map entries by PodSnapshot, in the order
// each PodSnapshot first appears.
func groupBySnapshot(mappings []podcontract.SnapshotMapping) [][]podcontract.SnapshotMapping {
	index := make(map[string]int, len(mappings))
	var groups [][]podcontract.SnapshotMapping
	for _, mapping := range mappings {
		i, found := index[mapping.SnapshotName]
		if !found {
			i = len(groups)
			index[mapping.SnapshotName] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], mapping)
	}
	return groups
}

// describeSnapshotMappings names each PodSnapshot with the destinations that
// restore from it: "PodSnapshots snap-prefill (prefill), snap-decode (decode)".
func describeSnapshotMappings(mappings []podcontract.SnapshotMapping) string {
	groups := groupBySnapshot(mappings)
	entries := make([]string, 0, len(groups))
	for _, group := range groups {
		destinations := make([]string, 0, len(group))
		for _, mapping := range group {
			destinations = append(destinations, mapping.Destination)
		}
		entries = append(entries, fmt.Sprintf("%s (%s)", group[0].SnapshotName, strings.Join(destinations, ", ")))
	}
	if len(entries) == 1 {
		return "PodSnapshot " + entries[0]
	}
	return "PodSnapshots " + strings.Join(entries, ", ")
}

// restoreSourceError is a preflight error of one PodSnapshot of a
// restore-from-map Pod. source names the PodSnapshot and the destinations that
// restore from it.
type restoreSourceError struct {
	source string
	err    error
}

func (e *restoreSourceError) Error() string { return e.source + ": " + e.err.Error() }

func (e *restoreSourceError) Unwrap() error { return e.err }

// withRestoreSource names group's PodSnapshot and destinations in err. A
// pending error stays pending, with the name in its message.
func withRestoreSource(group []podcontract.SnapshotMapping, err error) error {
	source := describeSnapshotMappings(group)
	var pending *restorePendingError
	if errors.As(err, &pending) {
		return newRestorePendingError(pending.reason, source+": "+pending.message)
	}
	return &restoreSourceError{source: source, err: err}
}

// restoreSourceOf returns the PodSnapshot that a restore-from-map preflight
// error names, or "" for any other error.
func restoreSourceOf(err error) string {
	var sourceErr *restoreSourceError
	if errors.As(err, &sourceErr) {
		return sourceErr.source
	}
	return ""
}

// joinPendingErrors keeps a restore-from-map Pod pending under the reason of
// its first pending PodSnapshot, with every pending message.
func joinPendingErrors(pending []*restorePendingError) error {
	messages := make([]string, 0, len(pending))
	for _, p := range pending {
		messages = append(messages, p.message)
	}
	return newRestorePendingError(pending[0].reason, strings.Join(messages, "; "))
}
