// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// Without the check, a CRIU built without LZ4 would write the snapshot
// uncompressed and report success.
func TestCheckpointRefusesCompressionTheNodesCRIUCannotDo(t *testing.T) {
	cfg := &types.AgentConfig{CRIU: types.CRIUSettings{Compress: true}}

	_, err := checkpoint(context.Background(), nil, logr.Discard(), CheckpointRequest{ContentUID: "content-uid", ContainerName: "main"}, cfg,
		func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
			t.Error("the source must not be touched")
			return nil, 0, errors.New("unreachable")
		})

	require.ErrorIs(t, err, errCompressionUnsupported)
	assert.Equal(t, StepConfigureCheckpoint, StepOf(err))
}

func TestCheckpointCompressesWhereCRIUCan(t *testing.T) {
	cfg := &types.AgentConfig{
		Storage:            types.StorageSpec{BasePath: types.CheckpointBasePath},
		CRIU:               types.CRIUSettings{Compress: true},
		CRIUMemCompression: true,
	}
	stop := errors.New("stop after the check")
	inspected := false

	_, err := checkpoint(context.Background(), nil, logr.Discard(), CheckpointRequest{ContentUID: "content-uid", ContainerName: "main"}, cfg,
		func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
			inspected = true
			return nil, 0, stop
		})

	require.ErrorIs(t, err, stop)
	assert.True(t, inspected)
}

func TestNSRestoreArgsPassTheRestoreWorkers(t *testing.T) {
	threads := func(n int) *int { return &n }
	for name, tc := range map[string]struct {
		threads *int
		want    []string
	}{
		"unset":          {},
		"CRIU's default": {threads: threads(1)},
		"CRIU chooses":   {threads: threads(0), want: []string{"--decompress-threads", "0"}},
		"a bound":        {threads: threads(8), want: []string{"--decompress-threads", "8"}},
	} {
		t.Run(name, func(t *testing.T) {
			args, err := nsrestoreArgs(RestoreRequest{DecompressThreads: tc.threads}, &types.RestoreContainerSnapshot{}, "/checkpoint")

			require.NoError(t, err)
			base := []string{"--checkpoint-path", "/checkpoint", "--bundle-dir", nsmount.SnapshotBinDst}
			assert.Equal(t, append(base, tc.want...), args)
		})
	}
}
