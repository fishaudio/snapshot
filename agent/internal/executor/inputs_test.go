// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	oldSecret = "old-s3cr3t-value"
	newSecret = "new-s3cr3t-value"
)

func inputsSpec(env []string, args ...string) *specs.Spec {
	return &specs.Spec{Process: &specs.Process{Env: env, Args: args}}
}

func TestCheckRestoreInputs(t *testing.T) {
	sourceEnv := []string{"PATH=/usr/bin", "DB_PASSWORD=" + oldSecret, "API_URL=http://api", "EMPTY=", "TWICE=1", "TWICE=1"}
	sourceArgs := []string{"/kamin/kamin-entrypoint", "run", "--", "serve", "--token", oldSecret}
	recorded := types.RecordInputs(inputsSpec(sourceEnv, sourceArgs...))

	for name, tc := range map[string]struct {
		want         *podcontract.RestoreInputs
		recorded     *types.InputsManifest
		noRecord     bool
		spec         *specs.Spec
		withProxy    bool
		wantEnv      []string
		wantArgs     bool
		unverifiable string
	}{
		"nothing asked": {
			spec: inputsSpec([]string{"DB_PASSWORD=" + newSecret}),
		},
		"equal": {
			want: &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD", "API_URL", "EMPTY", "TWICE"}, Args: true},
			spec: inputsSpec(append([]string{"SNAPSHOT_RESTORE_STANDBY=1", "HOSTNAME=other"}, sourceEnv...), sourceArgs...),
		},
		"a variable unset on both sides": {
			want: &podcontract.RestoreInputs{Env: []string{"NEVER_SET"}},
			spec: inputsSpec(sourceEnv),
		},
		"variables that are not asked for may differ": {
			want: &podcontract.RestoreInputs{Env: []string{"API_URL"}},
			spec: inputsSpec([]string{"API_URL=http://api", "DB_PASSWORD=" + newSecret, "HOSTNAME=other"}),
		},
		"changed values": {
			want:    &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD", "API_URL", "PATH"}},
			spec:    inputsSpec([]string{"PATH=/usr/bin", "DB_PASSWORD=" + newSecret, "API_URL=http://other"}),
			wantEnv: []string{"API_URL", "DB_PASSWORD"},
		},
		"set now, unset at checkpoint": {
			want:    &podcontract.RestoreInputs{Env: []string{"NEW_VAR"}},
			spec:    inputsSpec([]string{"NEW_VAR=" + newSecret}),
			wantEnv: []string{"NEW_VAR"},
		},
		"unset now, set at checkpoint": {
			want:    &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			spec:    inputsSpec([]string{"PATH=/usr/bin"}),
			wantEnv: []string{"DB_PASSWORD"},
		},
		"empty now, set at checkpoint": {
			want:    &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			spec:    inputsSpec([]string{"DB_PASSWORD="}),
			wantEnv: []string{"DB_PASSWORD"},
		},
		"set once now, twice at checkpoint": {
			want:    &podcontract.RestoreInputs{Env: []string{"TWICE"}},
			spec:    inputsSpec([]string{"TWICE=1"}),
			wantEnv: []string{"TWICE"},
		},
		"changed arguments": {
			want:     &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}, Args: true},
			spec:     inputsSpec(sourceEnv, "/kamin/kamin-entrypoint", "run", "--", "serve", "--token", newSecret),
			wantArgs: true,
		},
		"both": {
			want:     &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}, Args: true},
			spec:     inputsSpec([]string{"DB_PASSWORD=" + newSecret}, "serve"),
			wantEnv:  []string{"DB_PASSWORD"},
			wantArgs: true,
		},
		"no record": {
			want:         &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			noRecord:     true,
			spec:         inputsSpec(sourceEnv),
			unverifiable: "the snapshot recorded no inputs",
		},
		"a record of another version": {
			want:         &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			recorded:     &types.InputsManifest{Version: 2, Recorded: []string{"env"}, Salt: recorded.Salt},
			spec:         inputsSpec(sourceEnv),
			unverifiable: "version 2",
		},
		"a record without environment variables": {
			want:         &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			recorded:     &types.InputsManifest{Version: 1, Recorded: []string{"args"}, Salt: recorded.Salt, Args: recorded.Args},
			spec:         inputsSpec(sourceEnv),
			unverifiable: "did not record environment variables",
		},
		"a record without arguments": {
			want:         &podcontract.RestoreInputs{Args: true},
			recorded:     &types.InputsManifest{Version: 1, Recorded: []string{"env"}, Salt: recorded.Salt, Env: recorded.Env},
			spec:         inputsSpec(sourceEnv, sourceArgs...),
			unverifiable: "did not record arguments",
		},
		"a spec without a process": {
			want:         &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			spec:         &specs.Spec{},
			unverifiable: "has no process",
		},
		"arguments behind the restore proxy": {
			want:         &podcontract.RestoreInputs{Args: true},
			spec:         inputsSpec(sourceEnv, podcontract.RestoreProxyPath),
			withProxy:    true,
			unverifiable: "the restore proxy replaces",
		},
		"environment variables behind the restore proxy": {
			want:      &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
			spec:      inputsSpec(sourceEnv, podcontract.RestoreProxyPath),
			withProxy: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := recorded
			if tc.noRecord || tc.recorded != nil {
				record = tc.recorded
			}
			err := checkRestoreInputs(tc.want, record, tc.spec, !tc.withProxy)
			if len(tc.wantEnv) == 0 && !tc.wantArgs && tc.unverifiable == "" {
				require.NoError(t, err)
				return
			}
			var refused *RestoreInputsError
			require.ErrorAs(t, err, &refused)
			if tc.unverifiable != "" {
				require.Contains(t, refused.Unverifiable, tc.unverifiable)
			} else {
				require.Empty(t, refused.Unverifiable)
				require.Equal(t, tc.wantEnv, refused.Env)
				require.Equal(t, tc.wantArgs, refused.Args)
			}
			require.NotContains(t, err.Error(), oldSecret)
			require.NotContains(t, err.Error(), newSecret)
		})
	}
}

func TestRestoreInputsErrorNamesInputsOnly(t *testing.T) {
	err := &RestoreInputsError{Env: []string{"API_URL", "DB_PASSWORD"}, Args: true}
	require.Equal(t, "the destination container was started with inputs that differ from the snapshot's: "+
		"environment variables API_URL, DB_PASSWORD; arguments", err.Error())
	err = &RestoreInputsError{Env: []string{"DB_PASSWORD"}}
	require.Equal(t, "the destination container was started with inputs that differ from the snapshot's: "+
		"environment variables DB_PASSWORD", err.Error())
	err = &RestoreInputsError{Unverifiable: "the snapshot recorded no inputs"}
	require.Equal(t, "cannot check that the destination container was started with the snapshot's inputs: "+
		"the snapshot recorded no inputs", err.Error())
}

// A corrupted record is refused with what is wrong, never with a digest: a
// digest and the salt beside it would let anyone with the message test
// guesses offline.
func TestCheckRestoreInputsNeverPrintsADigest(t *testing.T) {
	recorded := types.RecordInputs(inputsSpec([]string{"DB_PASSWORD=" + oldSecret}))
	broken := *recorded
	broken.Env = map[string]string{"DB_PASSWORD": recorded.Env["DB_PASSWORD"][:20]}
	err := checkRestoreInputs(&podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}}, &broken,
		inputsSpec([]string{"DB_PASSWORD=" + newSecret}), true)
	var refused *RestoreInputsError
	require.ErrorAs(t, err, &refused)
	require.NotContains(t, err.Error(), recorded.Env["DB_PASSWORD"][:20])
	require.NotContains(t, err.Error(), recorded.Salt)
}

// inputsRuntime resolves a placeholder whose runtime spec is spec.
type inputsRuntime struct {
	restoreFakeRuntime
	spec *specs.Spec
}

func (r *inputsRuntime) ResolveContainer(_ context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedID = id
	return 123, r.spec, nil
}

// The check runs before anything else inspects the placeholder: before GPU
// discovery, whose nvidia-smi a refused restore never runs, and whether or not
// the compatibility checks are skipped.
func TestInspectRestoreChecksInputsFirstAndAlways(t *testing.T) {
	for _, skipCompatCheck := range []bool{false, true} {
		calls := filepath.Join(t.TempDir(), "calls")
		dir := t.TempDir()
		script := "#!/bin/sh\necho call >> " + calls + "\nprintf '%s\\n' 'GPU 0: NVIDIA B200 (UUID: GPU-a)'\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "nsenter"), []byte(script), 0o755))
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

		manifest := types.NewCheckpointManifest("content-uid-123", "main", types.CRIUDumpManifest{},
			types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "", nil),
			types.OverlayManifest{}, types.HostManifest{})
		manifest.CUDA = types.CUDAManifest{PIDs: []int{1}, SourceGPUUUIDs: []string{"GPU-a"}}
		manifest.Inputs = types.RecordInputs(inputsSpec([]string{"DB_PASSWORD=" + oldSecret}))
		rt := &inputsRuntime{spec: inputsSpec([]string{"NVIDIA_VISIBLE_DEVICES=none", "DB_PASSWORD=" + newSecret})}

		_, _, err := inspectRestore(context.Background(), rt, testr.New(t), RestoreRequest{
			ContentUID:               "content-uid-123",
			ContainerID:              "placeholder-id",
			PodName:                  "restore-pod",
			PodNamespace:             "default",
			ArtifactContainerName:    "main",
			DestinationContainerName: "main",
			WithoutProxy:             true,
			SkipCompatCheck:          skipCompatCheck,
			Inputs:                   &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}},
		}, manifest)

		var refused *RestoreInputsError
		require.ErrorAs(t, err, &refused, "skipCompatCheck=%v", skipCompatCheck)
		require.Equal(t, []string{"DB_PASSWORD"}, refused.Env)
		_, statErr := os.Stat(calls)
		require.True(t, errors.Is(statErr, os.ErrNotExist), "nvidia-smi ran before the inputs were checked")
	}
}

// Without a request, a restore behaves as before: a snapshot without a
// record, and a destination whose values differ, both restore.
func TestInspectRestoreWithoutARequestComparesNothing(t *testing.T) {
	manifest := types.NewCheckpointManifest("content-uid-123", "main", types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "", nil),
		types.OverlayManifest{}, types.HostManifest{})
	rt := &inputsRuntime{spec: inputsSpec([]string{"DB_PASSWORD=" + newSecret})}
	_, _, err := inspectRestore(context.Background(), rt, testr.New(t), RestoreRequest{
		ContentUID:               "content-uid-123",
		ContainerID:              "placeholder-id",
		ArtifactContainerName:    "main",
		DestinationContainerName: "main",
		WithoutProxy:             true,
	}, manifest)
	require.NoError(t, err)
}

// failOnMountInjector fails the test if the restore gets as far as touching
// the placeholder.
type failOnMountInjector struct {
	t *testing.T
}

func (m failOnMountInjector) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	m.t.Fatal("the restore mounted into the placeholder after its inputs were refused")
	return nil, errors.New("unreachable")
}

func (m failOnMountInjector) MountCuInterpose(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	m.t.Fatal("the restore mounted into the placeholder after its inputs were refused")
	return nil, errors.New("unreachable")
}

func (m failOnMountInjector) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	m.t.Fatal("the restore mounted into the placeholder after its inputs were refused")
	return nil, errors.New("unreachable")
}

func (m failOnMountInjector) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	m.t.Fatal("the restore mounted into the placeholder after its inputs were refused")
	return nil, errors.New("unreachable")
}

// A whole restore refused for its inputs stops before it mounts anything into
// the placeholder, reports the refusal as such, and logs neither value at any
// verbosity.
func TestRestoreRefusedForItsInputsTouchesNothingAndLeaksNothing(t *testing.T) {
	basePath := t.TempDir()
	artifactPath, err := nsmount.ResolveArtifactPath(basePath, "content-uid-123", "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(artifactPath, 0o700))
	manifest := types.NewCheckpointManifest("content-uid-123", "main", types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "", nil),
		types.OverlayManifest{}, types.HostManifest{})
	manifest.Inputs = types.RecordInputs(inputsSpec([]string{"DB_PASSWORD=" + oldSecret}, "serve", oldSecret))
	require.NoError(t, types.WriteManifest(artifactPath, manifest))

	var logs bytes.Buffer
	log := funcr.New(func(prefix, args string) {
		logs.WriteString(prefix + " " + args + "\n")
	}, funcr.Options{Verbosity: 10})

	rt := &inputsRuntime{spec: inputsSpec([]string{"DB_PASSWORD=" + newSecret}, "serve", newSecret)}
	_, err = Restore(context.Background(), rt, log, RestoreRequest{
		ContentUID:               "content-uid-123",
		BasePath:                 basePath,
		ContainerID:              "placeholder-id",
		PodName:                  "restore-pod",
		PodNamespace:             "default",
		ArtifactContainerName:    "main",
		DestinationContainerName: "main",
		WithoutProxy:             true,
		Inputs:                   &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}, Args: true},
	}, failOnMountInjector{t: t})

	var refused *RestoreInputsError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, []string{"DB_PASSWORD"}, refused.Env)
	require.True(t, refused.Args)
	require.Equal(t, StepInspectPlaceholder, StepOf(err))
	for _, secret := range []string{oldSecret, newSecret} {
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, logs.String(), secret)
	}
	for _, digest := range manifest.Inputs.Env {
		require.NotContains(t, logs.String(), digest)
	}
	require.NotContains(t, logs.String(), manifest.Inputs.Salt)
}

// A checkpoint records the target container's inputs in its manifest.
func TestConfigureCheckpointRecordsTheInputs(t *testing.T) {
	spec := inputsSpec([]string{"DB_PASSWORD=" + oldSecret, "PATH=/usr/bin"}, "serve")
	dir := t.TempDir()
	_, data, err := configureCheckpoint(logr.Discard(),
		&types.CheckpointContainerSnapshot{PID: 42, RootFS: "/", NetNSInode: 7, OCISpec: spec},
		CheckpointRequest{ContentUID: "content", ContainerName: "main"}, &types.AgentConfig{}, dir, false)
	require.NoError(t, err)
	require.NotNil(t, data.Inputs)
	require.Equal(t, []string{types.InputEnv, types.InputArgs}, data.Inputs.Recorded)
	require.ElementsMatch(t, []string{"DB_PASSWORD", "PATH"}, mapKeys(data.Inputs.Env))
	written, err := types.ReadManifest(dir)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(data.Inputs, written.Inputs))
	content, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(content), oldSecret)

	// Without a runtime spec, nothing is recorded.
	_, data, err = configureCheckpoint(logr.Discard(),
		&types.CheckpointContainerSnapshot{PID: 42, RootFS: "/", NetNSInode: 7},
		CheckpointRequest{ContentUID: "content", ContainerName: "main"}, &types.AgentConfig{}, t.TempDir(), false)
	require.NoError(t, err)
	require.Nil(t, data.Inputs)
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
