// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
)

// captureClaims gives each source pod to one capture work order at a time.
//
// A pod can be the source of several work orders, one per container, and each runs on its own
// queue key, so two can reconcile at once. Once a work order owns the pod it reads and changes the
// live pod: the crash sweep kills its containers, and the dump ends its target. Two at once break
// each other: one's dump reads as a crash to the other, and the containers share state, such as the
// pod's /dev/shm, that both dumps touch.
//
// The owner used to be recomputed on every pass from the worker's own view of the informer cache,
// and nothing recorded it. A work order that began while it was the only one cached went on to dump;
// a sibling created in the same second (creation timestamps have one-second resolution) whose name
// sorted first then chose itself, and the two dumped at once.
//
// A claim records the owner per pod UID. A work order takes a free claim only when the cache names
// it the owner (chooseActiveContent: oldest first, then by name), and keeps it across passes. Another
// may take it over only when the cache shows the owner finished (Ready, Failed, being deleted, or
// gone) and no pass of the owner is running. So the cache still decides the order, but the order no
// longer changes once a capture has begun, and the next capture starts only after the previous one's
// outcome is in the cache, which is what the next crash sweep reads.
//
// Claims live in memory: only this agent captures on its node, and after a restart the cache is
// synced before any work order runs, so every worker then sees the same owner.
type captureClaims struct {
	mu    sync.Mutex
	byPod map[ktypes.UID]*captureClaim
}

type captureClaim struct {
	// owner is the work order's name.
	owner string
	// podKey is the pod's "<namespace>/<name>", its podRefIndex key.
	podKey string
	// running says a pass of the owner holds the claim now.
	running bool
}

// claim decides whether work order name may go on with pod in this pass. chosen is the owner the
// cache names (chooseActiveContent), and finished reports whether the cache shows a work order done.
// When name may go on, claim returns name and a release function that the pass calls when it
// returns; otherwise it returns the owner to wait for (empty when the cache names none) and nil.
func (c *captureClaims) claim(pod *corev1.Pod, name, chosen string, finished func(name string) bool) (string, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	held := c.byPod[pod.UID]
	if held != nil && held.owner != name {
		if held.running || !finished(held.owner) {
			return held.owner, nil
		}
		delete(c.byPod, pod.UID)
		held = nil
	}
	if held == nil {
		if chosen != name {
			return chosen, nil
		}
		held = &captureClaim{owner: name, podKey: pod.Namespace + "/" + pod.Name}
		if c.byPod == nil {
			c.byPod = map[ktypes.UID]*captureClaim{}
		}
		c.byPod[pod.UID] = held
	}
	held.running = true
	return name, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		held.running = false
	}
}

// finish drops the claims work order name holds, which a pass calls when the cache shows the work
// order done. It returns the pod keys whose claims it dropped, so the pods' other work orders can be
// woken rather than wait for the next resync.
func (c *captureClaims) finish(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var podKeys []string
	for uid, held := range c.byPod {
		if held.owner == name && !held.running {
			delete(c.byPod, uid)
			podKeys = append(podKeys, held.podKey)
		}
	}
	return podKeys
}

// captureFinished reports whether the cache shows the work order done: Ready, Failed, being
// deleted, or gone. An unreadable cache entry counts as not done, so it keeps its claim.
func (w *NodeController) captureFinished(name string) bool {
	content, err := w.cachedContent(name)
	if err != nil {
		return false
	}
	return content == nil || !content.DeletionTimestamp.IsZero() || isContentTerminal(content)
}

// finishCapture drops the claims of a work order the cache shows done and queues the other work
// orders of their pods, which may be waiting for it.
func (w *NodeController) finishCapture(name string) {
	for _, podKey := range w.captureClaims.finish(name) {
		objs, err := w.contentIndexer.ByIndex(podRefIndex, podKey)
		if err != nil {
			w.log.Error(err, "Failed to look up PodSnapshotContent by source pod", "pod", podKey)
			continue
		}
		for _, obj := range objs {
			if other, ok := contentNameFromInformerObj(obj); ok && other != name {
				w.captureQueue.Add(other)
			}
		}
	}
}
