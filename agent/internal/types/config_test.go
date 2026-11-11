// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func validAgentConfig() *AgentConfig {
	return &AgentConfig{
		Storage: StorageSpec{
			Type:     "pvc",
			BasePath: "/checkpoints",
		},
		Restore: RestoreSpec{
			TimeoutSeconds: 60,
		},
		PageBroker: PageBrokerSpec{ControlSocketPath: "/pagebroker/control/pagebroker.sock"},
	}
}

// The key an admin flips in the ConfigMap has to be the key the agent reads,
// and an agent whose ConfigMap predates it has to keep the gate on.
func TestRestoreSpecParsesSkipCompatCheck(t *testing.T) {
	cases := map[string]bool{
		"restore:\n  skipCompatCheck: true\n":  true,
		"restore:\n  skipCompatCheck: false\n": false,
		"restore:\n  timeoutSeconds: 60\n":     false,
	}
	for document, want := range cases {
		cfg := &AgentConfig{}
		if err := yaml.Unmarshal([]byte(document), cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", document, err)
		}
		if cfg.Restore.SkipCompatCheck != want {
			t.Errorf("%q parsed skipCompatCheck = %v, want %v", document, cfg.Restore.SkipCompatCheck, want)
		}
	}
}

func TestAgentConfigValidateRequiresFixedStorageBasePath(t *testing.T) {
	for _, basePath := range []string{"checkpoints", " /checkpoints ", "/checkpoints/../other", "/other"} {
		cfg := validAgentConfig()
		cfg.Storage.BasePath = basePath
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted storage base path %q", basePath)
		}
	}
}

func TestAgentConfigValidateRequiresPageBrokerControlSocket(t *testing.T) {
	cfg := validAgentConfig()
	cfg.PageBroker.ControlSocketPath = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing PageBroker control socket")
	}
}

func TestPageBrokerRestoreMode(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"", "direct"}, {"  ", "direct"}, {"staged", "staged"}, {"direct", "direct"}, {" DIRECT ", "direct"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			cfg := validAgentConfig()
			cfg.PageBroker.RestoreMode = tc.input
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.PageBroker.RestoreMode != tc.want {
				t.Fatalf("mode = %q, want %q", cfg.PageBroker.RestoreMode, tc.want)
			}
		})
	}
	cfg := validAgentConfig()
	cfg.PageBroker.RestoreMode = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted invalid restore mode")
	}
}

// A config written before checkpoint.timeoutSeconds existed must still load, and must come up with
// the guard on rather than unbounded.
func TestCheckpointSpecDefaultsWhenAbsent(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("{}"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, time.Duration(DefaultCheckpointTimeoutSeconds)*time.Second, spec.CheckpointTimeout())
}

func TestCheckpointSpecRejectsAnExplicitNonPositiveTimeout(t *testing.T) {
	for _, raw := range []string{"timeoutSeconds: 0\n", "timeoutSeconds: -1\n"} {
		var spec CheckpointSpec
		require.NoError(t, yaml.Unmarshal([]byte(raw), &spec))
		require.Error(t, spec.Validate(), raw)
	}
}

func TestCheckpointSpecHonoursAnExplicitTimeout(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("timeoutSeconds: 45\n"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, 45*time.Second, spec.CheckpointTimeout())
}

// time.Duration counts nanoseconds in an int64, so a large enough seconds value wraps negative and
// every call site reads that as "no timeout" — the bound silently disappears. Reject it instead.
func TestTimeoutsRejectValuesThatOverflowDuration(t *testing.T) {
	overflow := int(maxTimeoutSeconds) + 1
	require.Less(t, time.Duration(overflow)*time.Second, time.Duration(0),
		"the test value must actually overflow, or this proves nothing")

	checkpoint := CheckpointSpec{TimeoutSeconds: &overflow}
	require.Error(t, checkpoint.Validate())

	restore := RestoreSpec{TimeoutSeconds: overflow}
	require.Error(t, restore.Validate())
}

func TestTimeoutsAcceptTheLargestRepresentableValue(t *testing.T) {
	limit := int(maxTimeoutSeconds)
	require.Positive(t, time.Duration(limit)*time.Second)

	checkpoint := CheckpointSpec{TimeoutSeconds: &limit}
	require.NoError(t, checkpoint.Validate())

	restore := RestoreSpec{TimeoutSeconds: limit}
	require.NoError(t, restore.Validate())
}

// A ConfigMap that predates the compression keys leaves them unset, so the
// agent keeps CRIU's own defaults.
func TestCRIUCompressionSettingsParse(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		cfg := &AgentConfig{}
		require.NoError(t, yaml.Unmarshal([]byte("criu:\n  imageIoMode: direct\n"), cfg))

		assert.False(t, cfg.CRIU.Compress)
		assert.Zero(t, cfg.CRIU.CompressBlockBytes)
		assert.Nil(t, cfg.CRIU.DecompressThreads)
	})

	t.Run("set", func(t *testing.T) {
		cfg := &AgentConfig{}
		document := "criu:\n  compress: true\n  compressBlockBytes: 1048576\n  decompressThreads: 0\n"
		require.NoError(t, yaml.Unmarshal([]byte(document), cfg))

		assert.True(t, cfg.CRIU.Compress)
		assert.Equal(t, uint32(1048576), cfg.CRIU.CompressBlockBytes)
		require.NotNil(t, cfg.CRIU.DecompressThreads, "0 asks CRIU to choose, unlike an absent key")
		assert.Equal(t, 0, *cfg.CRIU.DecompressThreads)
	})
}

func TestAgentConfigValidateCompressionSettings(t *testing.T) {
	threads := func(n int) *int { return &n }
	for name, tc := range map[string]struct {
		criu    CRIUSettings
		wantErr string
	}{
		"defaults":                         {},
		"compression":                      {criu: CRIUSettings{Compress: true, CompressBlockBytes: 65536}},
		"CRIU chooses the restore workers": {criu: CRIUSettings{DecompressThreads: threads(0)}},
		"the most restore workers":         {criu: CRIUSettings{DecompressThreads: threads(MaxDecompressThreads)}},
		"a block size without compression": {criu: CRIUSettings{CompressBlockBytes: 65536}, wantErr: "criu.compressBlockBytes"},
		"negative restore workers":         {criu: CRIUSettings{DecompressThreads: threads(-1)}, wantErr: "criu.decompressThreads"},
		"too many restore workers": {
			criu:    CRIUSettings{DecompressThreads: threads(MaxDecompressThreads + 1)},
			wantErr: "criu.decompressThreads",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validAgentConfig()
			cfg.CRIU = tc.criu

			err := cfg.Validate()

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
