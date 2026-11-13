// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package types defines shared data types used across snapshot packages.
package types

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// CheckpointBasePath is the fixed agent-side checkpoint mount. The privileged
// helper independently enforces the same path.
const CheckpointBasePath = "/checkpoints"

// AgentConfig holds static checkpoint settings plus runtime fields populated at startup.
type AgentConfig struct {
	NodeName               string          `yaml:"-"`
	HostKernelVersion      string          `yaml:"-"`
	HostCPUFeatures        []string        `yaml:"-"`
	CustomStorageAvailable bool            `yaml:"-"`
	Storage                StorageSpec     `yaml:"storage"`
	Overlay                OverlaySettings `yaml:"overlay"`
	PageBroker             PageBrokerSpec  `yaml:"pageBroker"`
	Checkpoint             CheckpointSpec  `yaml:"checkpoint"`
	Restore                RestoreSpec     `yaml:"restore"`
	CRIU                   CRIUSettings    `yaml:"criu"`
	// CRIUMemCompression reports that the node's CRIU can compress memory
	// pages, as its feature check said at startup.
	CRIUMemCompression bool `yaml:"-"`
}

func (c *AgentConfig) LoadEnvOverrides() {
	if v := os.Getenv("NODE_NAME"); v != "" {
		c.NodeName = v
	}
}

func (c *AgentConfig) Validate() error {
	storageType := strings.TrimSpace(c.Storage.Type)
	if storageType == "" {
		storageType = "pvc"
	}
	if storageType != "pvc" {
		return &ConfigError{Field: "storage.type", Message: fmt.Sprintf("unsupported storage type %q; only pvc is implemented today", storageType)}
	}
	basePath := c.Storage.BasePath
	if basePath != CheckpointBasePath {
		return &ConfigError{Field: "storage.basePath", Message: fmt.Sprintf("storage.basePath must be %q", CheckpointBasePath)}
	}
	c.Storage.BasePath = basePath
	if strings.TrimSpace(c.PageBroker.ControlSocketPath) == "" {
		return &ConfigError{Field: "pageBroker.controlSocketPath", Message: "pageBroker.controlSocketPath is required"}
	}
	c.PageBroker.RestoreMode = strings.ToLower(strings.TrimSpace(c.PageBroker.RestoreMode))
	switch c.PageBroker.RestoreMode {
	case "":
		c.PageBroker.RestoreMode = "direct"
	case "staged", "direct":
	default:
		return &ConfigError{Field: "pageBroker.restoreMode", Message: "pageBroker.restoreMode must be staged or direct"}
	}
	if c.CRIU.TcpClose && c.CRIU.TcpEstablished {
		return &ConfigError{
			Field:   "criu",
			Message: "tcpClose and tcpEstablished cannot both be true",
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.CRIU.ImageIoMode)) {
	case "", "writeback", "direct":
	default:
		return &ConfigError{
			Field:   "criu.imageIoMode",
			Message: fmt.Sprintf("unsupported imageIoMode %q; expected %q, %q, or empty", c.CRIU.ImageIoMode, "writeback", "direct"),
		}
	}
	if c.CRIU.CompressBlockBytes != 0 && !c.CRIU.Compress {
		return &ConfigError{Field: "criu.compressBlockBytes", Message: "criu.compressBlockBytes needs criu.compress"}
	}
	if threads := c.CRIU.DecompressThreads; threads != nil && (*threads < 0 || *threads > MaxDecompressThreads) {
		return &ConfigError{
			Field:   "criu.decompressThreads",
			Message: fmt.Sprintf("criu.decompressThreads must be between 0 and %d", MaxDecompressThreads),
		}
	}
	if err := c.Checkpoint.Validate(); err != nil {
		return err
	}
	return c.Restore.Validate()
}

// StorageSpec holds snapshot storage settings that are local to the agent deployment.
type StorageSpec struct {
	Type     string `yaml:"type"`
	BasePath string `yaml:"basePath"`
}

type PageBrokerSpec struct {
	// RestoreMode selects the restore source. Empty defaults to direct.
	RestoreMode       string `yaml:"restoreMode"`
	ControlSocketPath string `yaml:"controlSocketPath"`

	// StagingPath is the memory-backed volume CRIU writes the image into. The
	// agent measures it to reject a capture that provably cannot fit; empty
	// turns that check off, which is what a config predating the field does.
	StagingPath string `yaml:"stagingPath"`
}

// DefaultCheckpointTimeoutSeconds bounds a dump for a config written before the field existed.
const DefaultCheckpointTimeoutSeconds = 3600

// maxTimeoutSeconds is the largest value that survives conversion to a time.Duration, which counts
// nanoseconds in an int64. Past it the multiplication wraps negative, and a negative timeout reads
// as "no timeout" at every call site — so an absurd value would silently remove the bound it asked
// for. Roughly 292 years; nothing legitimate comes near it.
const maxTimeoutSeconds = int64(math.MaxInt64) / int64(time.Second)

// CheckpointSpec holds settings for the CRIU dump.
type CheckpointSpec struct {
	// A pointer so absent and zero are distinguishable: a config predating this field omits it,
	// as does one still spelling it checkpointTimeoutSeconds, and an agent upgraded ahead of its
	// ConfigMap has to start — with the guard on.
	TimeoutSeconds *int `yaml:"timeoutSeconds"`
}

// CheckpointTimeout bounds one dump, defaulting when the config omits it.
func (c *CheckpointSpec) CheckpointTimeout() time.Duration {
	if c.TimeoutSeconds == nil {
		return DefaultCheckpointTimeoutSeconds * time.Second
	}
	return time.Duration(*c.TimeoutSeconds) * time.Second
}

func (c *CheckpointSpec) Validate() error {
	if c.TimeoutSeconds == nil {
		return nil
	}
	if *c.TimeoutSeconds <= 0 {
		return &ConfigError{Field: "checkpoint.timeoutSeconds", Message: "checkpoint.timeoutSeconds must be greater than zero"}
	}
	if int64(*c.TimeoutSeconds) > maxTimeoutSeconds {
		return &ConfigError{
			Field:   "checkpoint.timeoutSeconds",
			Message: fmt.Sprintf("checkpoint.timeoutSeconds must not exceed %d", maxTimeoutSeconds),
		}
	}
	return nil
}

// DefaultSnapshotWaitSeconds bounds a restore's wait for its snapshot for a config written before
// the field existed.
const DefaultSnapshotWaitSeconds = 60

// RestoreSpec holds settings for the CRIU restore process.
type RestoreSpec struct {
	TimeoutSeconds int `yaml:"timeoutSeconds"`

	// SnapshotWaitSeconds bounds how long a restore Pod waits for its
	// PodSnapshots to be ready to restore, counted from when its first
	// destination container started. Past it the agent ends the restore the
	// way a failed one ends, and the destination containers start without it.
	// A pointer so absent and zero are distinguishable: absent is
	// DefaultSnapshotWaitSeconds, and zero waits for as long as it takes.
	SnapshotWaitSeconds *int `yaml:"snapshotWaitSeconds"`

	// SkipCompatCheck turns the restore compatibility gate off for every
	// restore this agent handles. It is the per-node escape hatch, for a
	// cluster admin who would otherwise be stuck annotating pods one by one.
	SkipCompatCheck bool `yaml:"skipCompatCheck"`
}

func (c *RestoreSpec) RestoreTimeout() time.Duration {
	if c.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// SnapshotWait bounds a restore's wait for its snapshot; zero means no bound.
func (c *RestoreSpec) SnapshotWait() time.Duration {
	if c.SnapshotWaitSeconds == nil {
		return DefaultSnapshotWaitSeconds * time.Second
	}
	return time.Duration(max(*c.SnapshotWaitSeconds, 0)) * time.Second
}

func (c *RestoreSpec) Validate() error {
	if c.TimeoutSeconds <= 0 {
		return &ConfigError{Field: "restore.timeoutSeconds", Message: "restore.timeoutSeconds must be greater than zero"}
	}
	if int64(c.TimeoutSeconds) > maxTimeoutSeconds {
		return &ConfigError{
			Field:   "restore.timeoutSeconds",
			Message: fmt.Sprintf("restore.timeoutSeconds must not exceed %d", maxTimeoutSeconds),
		}
	}
	if wait := c.SnapshotWaitSeconds; wait != nil {
		if *wait < 0 {
			return &ConfigError{Field: "restore.snapshotWaitSeconds", Message: "restore.snapshotWaitSeconds must not be negative"}
		}
		if int64(*wait) > maxTimeoutSeconds {
			return &ConfigError{
				Field:   "restore.snapshotWaitSeconds",
				Message: fmt.Sprintf("restore.snapshotWaitSeconds must not exceed %d", maxTimeoutSeconds),
			}
		}
	}
	return nil
}

// CRIUSettings holds CRIU-specific configuration options.
type CRIUSettings struct {
	GhostLimit        uint32 `yaml:"ghostLimit"`
	LogLevel          int32  `yaml:"logLevel"`
	WorkDir           string `yaml:"workDir"`
	AutoDedup         bool   `yaml:"autoDedup"`
	LazyPages         bool   `yaml:"lazyPages"`
	ShellJob          bool   `yaml:"shellJob"`
	TcpClose          bool   `yaml:"tcpClose"`
	TcpEstablished    bool   `yaml:"tcpEstablished"`
	FileLocks         bool   `yaml:"fileLocks"`
	OrphanPtsMaster   bool   `yaml:"orphanPtsMaster"`
	ExtUnixSk         bool   `yaml:"extUnixSk"`
	LinkRemap         bool   `yaml:"linkRemap"`
	ExtMasters        bool   `yaml:"extMasters"`
	ManageCgroupsMode string `yaml:"manageCgroupsMode"`
	ImageIoMode       string `yaml:"imageIoMode"`
	RstSibling        bool   `yaml:"rstSibling"`
	MntnsCompatMode   bool   `yaml:"mntnsCompatMode"`
	EvasiveDevices    bool   `yaml:"evasiveDevices"`
	ForceIrmap        bool   `yaml:"forceIrmap"`
	BinaryPath        string `yaml:"binaryPath"`
	LibDir            string `yaml:"libDir"`
	AllowUprobes      bool   `yaml:"allowUprobes"`
	SkipInFlight      bool   `yaml:"skipInFlight"`
	// Compress has the dump store memory pages in LZ4-compressed blocks.
	// All-zero blocks take no space, and blocks that do not compress are
	// stored raw. A restore needs no setting to read them.
	Compress bool `yaml:"compress"`
	// CompressBlockBytes is the size of a compression block; zero keeps
	// CRIU's default. It needs Compress.
	CompressBlockBytes uint32 `yaml:"compressBlockBytes"`
	// DecompressThreads bounds the workers a restore decodes compressed
	// blocks and large zero runs with: 0 lets CRIU choose, 1 decodes them
	// serially. The restoring agent's value applies, not the one a snapshot
	// was taken with. Unset keeps CRIU's default, 1.
	DecompressThreads *int `yaml:"decompressThreads"`
}

// MaxDecompressThreads is the largest worker count CRIU accepts for
// decompress_threads.
const MaxDecompressThreads = 1024

// OverlaySettings is the static config for rootfs exclusions.
type OverlaySettings struct {
	Exclusions []string `yaml:"exclusions"`
}

// ConfigError represents a configuration validation error.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("config error: %s: %s", e.Field, e.Message)
}
