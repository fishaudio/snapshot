// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// Ending a restore. A restore that will not run, because the gates refused it,
// its preflight failed, or its snapshot was not ready in time, ends the way a
// failed restore does: each destination container gets restore-failed in its
// control directory and its placeholder is killed. The container then starts
// again at once, without a restore, instead of when its own wait for one runs
// out.
//
// restore-failed is also what keeps a later restore out of that container,
// across container and agent restarts: runRestore never restores into a
// container that holds it. The Pod's terminal condition keeps the restore
// from running again in the first place; the marker covers the cases where
// that condition was not written.

// Reasons of the preflight waits for the snapshot itself, which
// config.restore.snapshotWaitSeconds bounds.
const (
	snapshotPendingReason = "SnapshotPending"
	contentPendingReason  = "ContentPending"
	artifactPendingReason = "ArtifactPending"
)

func waitsForSnapshot(reason string) bool {
	switch reason {
	case snapshotPendingReason, contentPendingReason, artifactPendingReason:
		return true
	}
	return false
}

// endRestore ends the destination containers of a restore that ends with
// none of them restored, then records reason and message as its outcome.
// Destinations whose container has not started yet are ended by later passes,
// which their start triggers. It returns whether the Pod needs another pass.
//
// The containers are ended before the outcome is recorded: if the record
// fails, the next pass decides again, and the marker keeps any restore it
// decides on out of the containers ended here.
func (w *NodeController) endRestore(ctx context.Context, pod *corev1.Pod, destinations []string, reason, message string) bool {
	uid := string(pod.UID)
	unstarted := w.endDestinations(ctx, pod, destinations)
	if len(unstarted) == 0 {
		w.endingRestores.Delete(uid)
	} else {
		w.endingRestores.Store(uid, unstarted)
	}
	if err := w.finishRestore(ctx, pod, corev1.ConditionFalse, reason, message); err != nil {
		return true
	}
	return len(unstarted) != 0
}

// continueEndingRestore ends the destinations of an ended restore that had
// not started when it ended. It returns whether some still have not.
func (w *NodeController) continueEndingRestore(ctx context.Context, pod *corev1.Pod) bool {
	uid := string(pod.UID)
	value, ending := w.endingRestores.Load(uid)
	if !ending {
		return false
	}
	if pod.DeletionTimestamp != nil || !isRestorePodActive(pod) {
		w.endingRestores.Delete(uid)
		return false
	}
	unstarted := w.endDestinations(ctx, pod, value.([]string))
	if len(unstarted) == 0 {
		w.endingRestores.Delete(uid)
		return false
	}
	w.endingRestores.Store(uid, unstarted)
	return true
}

// endDestinations ends each destination whose container has started, and
// returns the destinations whose container never has. One that ran and
// stopped only gets the marker, through the node's path of its control
// volume, which its next start reads.
func (w *NodeController) endDestinations(ctx context.Context, pod *corev1.Pod, destinations []string) []string {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	var unstarted []string
	for _, destination := range destinations {
		if containerID := w.runningDestinationContainer(ctx, pod, destination); containerID != "" {
			w.endDestination(ctx, pod, destination, containerID,
				w.log.WithValues("pod", podKey, "destination_container", destination, "container_id", containerID))
			continue
		}
		if containerID, stopped := stoppedDestinationContainer(pod, destination); stopped {
			w.markRestoreFailedOnNode(ctx, pod, destination, containerID,
				w.log.WithValues("pod", podKey, "destination_container", destination, "container_id", containerID))
			continue
		}
		unstarted = append(unstarted, destination)
	}
	return unstarted
}

// stoppedDestinationContainer returns the ID of a destination's container
// that ran and stopped, and whether there is one.
func stoppedDestinationContainer(pod *corev1.Pod, destination string) (string, bool) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != destination {
			continue
		}
		for _, state := range []corev1.ContainerState{status.State, status.LastTerminationState} {
			if state.Terminated != nil {
				return snapshotruntime.StripCRIScheme(state.Terminated.ContainerID), true
			}
		}
	}
	return "", false
}

// runningDestinationContainer finds a destination's running container the
// way resolveRestoreContainerID does, but asks each source once: a container
// that has not started is ended by the pass its start triggers.
func (w *NodeController) runningDestinationContainer(ctx context.Context, pod *corev1.Pod, destination string) string {
	if containerID := restoreContainerIDFromStatus(pod, destination); containerID != "" {
		return containerID
	}
	if started, ok := w.startedRestoreContainers.Load(restoreContainerKey(pod.UID, destination)); ok {
		return started.(string)
	}
	resolveCtx, cancel := context.WithTimeout(ctx, containerResolveAttemptTimeout)
	defer cancel()
	containerID, err := w.runtime.ResolveContainerIDByPod(resolveCtx, pod.Name, pod.Namespace, destination)
	if err != nil {
		return ""
	}
	return containerID
}

// endDestination ends one destination container that its restore did not
// reach: restore-failed in its control directory, then its placeholder
// killed. A container that already holds restore-failed or restore-complete
// was told its outcome before, by an earlier pass, an earlier agent or its
// own workload, and may be running its workload by now, so it is left alone.
func (w *NodeController) endDestination(ctx context.Context, pod *corev1.Pod, destination, containerID string, log logr.Logger) {
	hostPID, _, err := w.runtime.ResolveContainer(ctx, containerID)
	if err != nil {
		log.V(1).Info("Cannot resolve the destination container to end it; marking it through the host path", "error", err.Error())
		w.markRestoreFailedOnNode(ctx, pod, destination, containerID, log)
		return
	}
	told, err := w.destinationTold(hostPID)
	if err != nil {
		log.Error(err, "Cannot tell whether the destination container was already told its restore outcome; ending it")
	} else if told {
		log.V(1).Info("Destination container already holds its restore outcome; leaving it alone")
		return
	}
	if err := w.writeControlSentinelFn(hostPID, podcontract.RestoreFailedFile, restoreFailedSentinelContents); err != nil {
		log.Error(err, "Failed to write restore-failed sentinel")
	}
	err = w.sendSignalFn(log, hostPID, syscall.SIGKILL, "restore ended")
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Error(err, "Failed to kill the placeholder of an ended restore")
		return
	}
	log.Info("Ended a destination container that will not be restored")
}

// destinationTold reports whether a destination container's control
// directory holds restore-failed or restore-complete.
func (w *NodeController) destinationTold(hostPID int) (bool, error) {
	for _, name := range []string{podcontract.RestoreFailedFile, podcontract.RestoreCompleteFile} {
		exists, err := w.controlSentinelExistsFn(hostPID, name)
		if err != nil || exists {
			return exists, err
		}
	}
	return false, nil
}

// markRestoreFailedOnNode writes restore-failed for a destination container
// with no process to write through, through the node's path of the control
// volume. kubelet names that path after the Pod's UID on the node, which in a
// vCluster differs from this Pod's; the runtime knows it while the container
// exists.
func (w *NodeController) markRestoreFailedOnNode(ctx context.Context, pod *corev1.Pod, destination, containerID string, log logr.Logger) {
	podUID := string(pod.UID)
	if nodeUID, err := w.runtime.ResolveContainerPodUID(ctx, containerID); err == nil {
		podUID = nodeUID
	} else {
		log.V(1).Info("Cannot read the Pod's UID on the node; using its own", "error", err.Error())
	}
	if err := w.writePodControlSentinelFn(podUID, destination, podcontract.RestoreFailedFile, restoreFailedSentinelContents); err != nil {
		log.Error(err, "Failed to write restore-failed sentinel through the host path")
	}
}

// refuseEndedDestination keeps a restore out of a destination container that
// holds restore-failed: a failed or ended restore of this Pod, or its own
// workload, told it that no restore is coming, and it may be running its
// workload without one by now. The container is neither restored nor killed.
func (op *restoreOperation) refuseEndedDestination(ctx context.Context) error {
	hostPID, _, err := op.controller.runtime.ResolveContainer(ctx, op.containerID)
	if err != nil {
		// The restore cannot reach a container the runtime cannot resolve
		// either, and reports that itself.
		return nil
	}
	ended, err := op.controller.controlSentinelExistsFn(hostPID, podcontract.RestoreFailedFile)
	if err != nil {
		return fmt.Errorf("check for a restore-failed sentinel before restoring: %w", err)
	}
	if ended {
		return errors.New("destination container already holds restore-failed: its restore ended before, and it may run without one, so it is not restored")
	}
	return nil
}

// endableDestinations names the containers of the Pod that a restore the
// agent ends was going to restore into. A restore-from Pod without a
// container map names its destination only through its PodSnapshot; when
// that cannot be read, the destinations are the containers that carry a
// destination's control mount.
func (w *NodeController) endableDestinations(ctx context.Context, pod *corev1.Pod) []string {
	named, err := w.RestoreDestinations(ctx, pod)
	var destinations []string
	for _, container := range pod.Spec.Containers {
		if err == nil {
			if slices.Contains(named, container.Name) {
				destinations = append(destinations, container.Name)
			}
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == podcontract.SnapshotControlVolumeName &&
				mount.MountPath == podcontract.SnapshotControlMountPath &&
				mount.SubPath == container.Name {
				destinations = append(destinations, container.Name)
				break
			}
		}
	}
	return destinations
}

// endRestoreAfterSnapshotWait ends a restore whose snapshot is still not ready
// to restore config.restore.snapshotWaitSeconds after its first destination
// container started. Until then it schedules a pass for that moment. It
// reports whether it ended the restore, and if so whether the Pod needs
// another pass.
func (w *NodeController) endRestoreAfterSnapshotWait(ctx context.Context, pod *corev1.Pod, pending *restorePendingError) (ended, requeue bool) {
	wait := w.config.Restore.SnapshotWait()
	if wait <= 0 || !waitsForSnapshot(pending.reason) {
		return false, false
	}
	destinations := w.endableDestinations(ctx, pod)
	started := snapshotWaitStart(pod, destinations)
	if started.IsZero() {
		// Nothing waits yet; the first destination's start requeues the Pod.
		return false, false
	}
	if remaining := started.Add(wait).Sub(w.nowFn()); remaining > 0 {
		w.restoreQueue.AddAfter(client.ObjectKeyFromObject(pod), remaining)
		return false, false
	}
	w.log.Info("Ending restore; its snapshot is not ready to restore",
		"pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
		"snapshot_wait", wait.String(),
		"reason", pending.reason,
		"message", pending.message,
	)
	message := fmt.Sprintf("Stopped waiting for the snapshot after %ds (config.restore.snapshotWaitSeconds): %s; the destination containers start without a restore",
		int64(wait/time.Second), pending.message)
	return true, w.endRestore(ctx, pod, destinations, podcontract.RestoreReasonFailed, message)
}

// snapshotWaitStart is when a restore Pod started waiting for its snapshot:
// when its first destination container started, which is also when a
// workload's own wait for the restore starts. It is zero while none has.
func snapshotWaitStart(pod *corev1.Pod, destinations []string) time.Time {
	var first time.Time
	consider := func(startedAt metav1.Time) {
		if !startedAt.IsZero() && (first.IsZero() || startedAt.Time.Before(first)) {
			first = startedAt.Time
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !slices.Contains(destinations, status.Name) {
			continue
		}
		for _, state := range []corev1.ContainerState{status.State, status.LastTerminationState} {
			if state.Running != nil {
				consider(state.Running.StartedAt)
			}
			if state.Terminated != nil {
				consider(state.Terminated.StartedAt)
			}
		}
	}
	return first
}
