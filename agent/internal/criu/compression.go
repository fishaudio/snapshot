// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"strings"

	criulib "github.com/checkpoint-restore/go-criu/v8"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// compressBlockMode is CRIU's compress_mode for block compression, the only
// mode besides off (0).
const compressBlockMode = 1

// applyCompression sets the dump's page compression options. Without
// Compress they stay unset, so the dump request is unchanged.
func applyCompression(opts *criurpc.CriuOpts, settings *types.CRIUSettings) {
	if !settings.Compress {
		return
	}
	opts.Compress = proto.Uint32(compressBlockMode)
	if settings.CompressBlockBytes > 0 {
		opts.CompressBlockSize = proto.Uint32(settings.CompressBlockBytes)
	}
}

// SetDecompressThreads sets how many workers the restore decodes compressed
// blocks and large zero runs with. A compressed image records its blocks in
// each pagemap entry, so a restore needs no other option to read it, and an
// uncompressed image has no blocks to decode. CRIU's default, 1, is left
// unset, so the restore request is unchanged.
func SetDecompressThreads(opts *criurpc.CriuOpts, threads int) {
	if threads != 1 {
		opts.DecompressThreads = proto.Uint32(uint32(threads))
	}
}

type featureChecker interface {
	FeatureCheck(features *criurpc.CriuFeatures) (*criurpc.CriuFeatures, error)
}

// MemCompressionSupported asks the CRIU binary at binaryPath, or on PATH when
// empty, whether it was built with memory page compression. A CRIU that does
// not know the feature reports it missing.
func MemCompressionSupported(binaryPath string) (bool, error) {
	c := criulib.MakeCriu()
	if strings.TrimSpace(binaryPath) != "" {
		c.SetCriuPath(binaryPath)
	}
	return memCompressionSupported(c)
}

func memCompressionSupported(c featureChecker) (bool, error) {
	features, err := c.FeatureCheck(&criurpc.CriuFeatures{MemCompression: proto.Bool(true)})
	if err != nil {
		return false, err
	}
	return features.GetMemCompression(), nil
}
