// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"testing"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func dumpOptions(t *testing.T, settings *types.CRIUSettings) *criurpc.CriuOpts {
	t.Helper()
	opts, err := BuildDumpOptions(&types.CheckpointContainerSnapshot{PID: 42, RootFS: t.TempDir()}, settings, t.TempDir(), logr.Discard())
	require.NoError(t, err)
	return opts
}

func TestBuildDumpOptionsLeavesCompressionUnsetByDefault(t *testing.T) {
	opts := dumpOptions(t, &types.CRIUSettings{})

	assert.Nil(t, opts.Compress)
	assert.Nil(t, opts.CompressBlockSize)
	assert.Nil(t, opts.CompressAcceleration)
}

func TestBuildDumpOptionsCompressesInBlocks(t *testing.T) {
	t.Run("CRIU's block size", func(t *testing.T) {
		opts := dumpOptions(t, &types.CRIUSettings{Compress: true})

		require.NotNil(t, opts.Compress)
		assert.Equal(t, uint32(compressBlockMode), opts.GetCompress())
		assert.Nil(t, opts.CompressBlockSize)
	})

	t.Run("a configured block size", func(t *testing.T) {
		opts := dumpOptions(t, &types.CRIUSettings{Compress: true, CompressBlockBytes: 1 << 20})

		assert.Equal(t, uint32(compressBlockMode), opts.GetCompress())
		require.NotNil(t, opts.CompressBlockSize)
		assert.Equal(t, uint32(1<<20), opts.GetCompressBlockSize())
	})
}

func TestSetDecompressThreads(t *testing.T) {
	for name, tc := range map[string]struct {
		threads int
		want    *uint32
	}{
		"CRIU's default stays unset": {threads: 1},
		"CRIU chooses":               {threads: 0, want: proto.Uint32(0)},
		"a bound":                    {threads: 8, want: proto.Uint32(8)},
	} {
		t.Run(name, func(t *testing.T) {
			opts := &criurpc.CriuOpts{}

			SetDecompressThreads(opts, tc.threads)

			assert.Equal(t, tc.want, opts.DecompressThreads)
		})
	}
}

// The workers are the restoring agent's to choose; a value a snapshot
// recorded when it was taken does not reach the restore.
func TestBuildRestoreOptsIgnoresTheRecordedDecompressThreads(t *testing.T) {
	threads := 8
	manifest := &types.CheckpointManifest{
		CRIUDump: types.CRIUDumpManifest{
			CRIU:   types.CRIUSettings{Compress: true, DecompressThreads: &threads},
			ExtMnt: map[string]string{"root": "/"},
		},
	}

	opts, err := BuildRestoreOpts(manifest, "", "", logr.Discard())

	require.NoError(t, err)
	assert.Nil(t, opts.DecompressThreads)
	assert.Nil(t, opts.Compress, "a restore needs no compression option")
}

type fakeFeatureChecker struct {
	features *criurpc.CriuFeatures
	err      error
	asked    *criurpc.CriuFeatures
}

func (f *fakeFeatureChecker) FeatureCheck(features *criurpc.CriuFeatures) (*criurpc.CriuFeatures, error) {
	f.asked = features
	return f.features, f.err
}

func TestMemCompressionSupported(t *testing.T) {
	for name, tc := range map[string]struct {
		checker *fakeFeatureChecker
		want    bool
		wantErr bool
	}{
		"built with LZ4":    {checker: &fakeFeatureChecker{features: &criurpc.CriuFeatures{MemCompression: proto.Bool(true)}}, want: true},
		"built without LZ4": {checker: &fakeFeatureChecker{features: &criurpc.CriuFeatures{MemCompression: proto.Bool(false)}}},
		// A CRIU from before the feature leaves the field out of its answer.
		"older CRIU":          {checker: &fakeFeatureChecker{features: &criurpc.CriuFeatures{}}},
		"feature check fails": {checker: &fakeFeatureChecker{err: errors.New("criu not found")}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := memCompressionSupported(tc.checker)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
			assert.True(t, tc.checker.asked.GetMemCompression(), "the check asks for memory page compression")
		})
	}
}
