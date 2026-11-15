// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// Claiming a destination container. On a node without the restore proxy the
// destination container runs its own command, which may stop waiting for the
// restore and start the workload without one. The restore must then stay out
// of that container, or two copies of the workload run in it, on the same
// GPUs. restore-failed cannot settle that alone: the agent reads it only
// before the restore begins, and the restore itself clears it.
//
// So the agent and the workload claim the container: whoever first creates
// restore-claim in its control directory, with one exclusive create, owns it.
// The agent claims before it touches the container or its GPUs for a restore,
// and before it ends the container with restore-failed and a kill. A
// container it did not claim it neither restores nor kills. The claim is never
// removed, so it also keeps the restore out of the container's later starts.

// destinationClaim is what the agent found when it claimed a destination
// container.
type destinationClaim struct {
	// ours means the agent may act on the container: it created the claim
	// now, or created it before for this same container, in a pass or an
	// agent that did not finish.
	ours bool
	// holder is the claim found when it is not ours.
	holder podcontract.RestoreClaim
	// unreadable says why the claim found could not be read; holder is then
	// empty.
	unreadable error
}

// heldByWorkload reports whether the claim is the workload's: one that reads
// owner=workload. Any other claim that is not ours, an unreadable one
// included, is treated like a claim the agent made for an earlier container.
func (c destinationClaim) heldByWorkload() bool {
	return !c.ours && c.unreadable == nil && c.holder.Owner == podcontract.ControlFileOwnerWorkload
}

// describe says who holds a claim that is not ours.
func (c destinationClaim) describe() string {
	switch {
	case c.ours:
		return "is claimed by the agent"
	case c.unreadable != nil:
		return fmt.Sprintf("holds a restore claim that cannot be read (%v)", c.unreadable)
	case c.holder.Owner == podcontract.ControlFileOwnerAgent:
		return fmt.Sprintf("was claimed by the agent for an earlier container %s", c.holder.ContainerID)
	default:
		return "was claimed by its workload"
	}
}

// claimDestination claims the destination container with runtime ID
// containerID for the agent, through hostPID, a process in it. It fails only
// when the claim could not be decided, for example because the control
// directory cannot be reached.
func (w *NodeController) claimDestination(hostPID int, containerID string) (destinationClaim, error) {
	containerID = snapshotruntime.StripCRIScheme(containerID)
	contents := podcontract.FormatRestoreClaim(podcontract.RestoreClaim{
		Owner:       podcontract.ControlFileOwnerAgent,
		ContainerID: containerID,
	})
	created, err := w.claimControlFileFn(hostPID, podcontract.RestoreClaimFile, contents)
	if created {
		if err != nil {
			// Ours, and whole: only the temporary file it was written
			// through is left behind.
			w.log.Error(err, "Claimed a destination container but could not clean up after it", "container_id", containerID)
		}
		return destinationClaim{ours: true}, nil
	}
	if err != nil {
		return destinationClaim{}, fmt.Errorf("claim the destination container: %w", err)
	}
	holder, exists, err := w.readClaim(hostPID)
	switch {
	case err != nil:
		return destinationClaim{unreadable: err}, nil
	case !exists:
		// Nobody removes a claim, so one that went away was not the agent's.
		return destinationClaim{unreadable: errors.New("it was removed")}, nil
	}
	return destinationClaim{ours: holder.HeldByAgentFor(containerID), holder: holder}, nil
}

// A claim is created whole, but one made by a writer that creates the file
// before it writes it can be read empty or cut short for a moment. The agent
// reads such a claim again, up to claimReadAttempts times claimReadInterval
// apart, before it takes it as unreadable.
const (
	claimReadAttempts = 5
	claimReadInterval = 20 * time.Millisecond
)

// readClaim reads the claim in a destination container's control directory,
// through hostPID, a process in it. exists is false when there is none. A
// claim that does not parse is read again, as above, before it is reported.
func (w *NodeController) readClaim(hostPID int) (claim podcontract.RestoreClaim, exists bool, err error) {
	for attempt := range claimReadAttempts {
		if attempt != 0 {
			time.Sleep(claimReadInterval)
		}
		data, found, readErr := w.readControlFileFn(hostPID, podcontract.RestoreClaimFile)
		if readErr != nil || !found {
			// Not a regular file, or no claim at all: reading again does
			// not change that.
			return podcontract.RestoreClaim{}, found, readErr
		}
		claim, err = podcontract.ParseRestoreClaim(data)
		if err == nil {
			return claim, true, nil
		}
	}
	return podcontract.RestoreClaim{}, true, err
}

// claimDestination claims the destination container before the restore touches
// it or its GPUs. A container that the agent cannot claim is neither restored
// nor killed. When its workload claimed it, the destination is declined: the
// workload starts without a restore. Any other claim means the Pod's one
// restore into the destination was used by an earlier container, or by a
// claimer that stopped before it said who it was: the restore fails, and
// restore-failed tells whatever waits in the container that no restore is
// coming.
func (op *restoreOperation) claimDestination(ctx context.Context) error {
	w := op.controller
	hostPID, _, err := w.runtime.ResolveContainer(ctx, op.containerID)
	if err != nil {
		// Without a process in the container there is nothing to claim it
		// through, and nothing to restore into either.
		return op.failRestore(ctx, fmt.Errorf("resolve the destination container to claim it: %w", err))
	}
	claim, err := w.claimDestination(hostPID, op.containerID)
	if err != nil {
		return op.failRestore(ctx, err)
	}
	if claim.ours {
		return nil
	}
	if claim.heldByWorkload() {
		op.log.Info("The destination container's workload claimed it first; leaving it to start without a restore")
		return &restoreDeclinedError{
			reason:  podcontract.RestoreReasonClaimedByWorkload,
			message: "destination container was claimed by its workload first: it starts without a restore, so it is not restored",
		}
	}
	op.log.Info("Not restoring a destination container whose claim is not this agent's", "claim", claim.describe())
	w.tellNoRestoreComing(hostPID, op.log)
	return fmt.Errorf("destination container %s: its restore is used, so it is not restored again", claim.describe())
}

// tellNoRestoreComing writes restore-failed into a destination container the
// agent leaves alone, unless it holds one already, so a proxy or workload
// that waits there for a restore stops waiting.
func (w *NodeController) tellNoRestoreComing(hostPID int, log logr.Logger) {
	if told, err := w.controlSentinelExistsFn(hostPID, podcontract.RestoreFailedFile); err == nil && told {
		return
	}
	if err := w.writeControlSentinelFn(hostPID, podcontract.RestoreFailedFile, restoreFailedSentinelContents); err != nil {
		log.Error(err, "Failed to write restore-failed sentinel")
	}
}
