// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The restoring agent's config.criu.decompressThreads applies, whatever the
// snapshot was taken with.
func TestExecuteRestorePassesTheNodesRestoreWorkers(t *testing.T) {
	for name, threads := range map[string]*int{"unset": nil, "set": new(int)} {
		t.Run(name, func(t *testing.T) {
			pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			w := makeTestController(t, pod)
			w.config.CRIU.DecompressThreads = threads
			var got executor.RestoreRequest
			w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
				got = req
				return executor.RestoreResult{}, nil
			}
			plan := &restorePlan{artifact: &restoreArtifact{}}

			_, err := w.newRestoreOperation(pod, plan, "main", "container-id", time.Now()).executeRestore(context.Background())

			require.NoError(t, err)
			assert.Equal(t, threads, got.DecompressThreads)
		})
	}
}
