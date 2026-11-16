// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const testSecretValue = "s3cr3t-value-that-must-not-leak"

func testSpec(env []string, args ...string) *specs.Spec {
	return &specs.Spec{Process: &specs.Process{Env: env, Args: args}}
}

func TestRecordInputsNeedsAProcess(t *testing.T) {
	if got := RecordInputs(nil); got != nil {
		t.Fatalf("RecordInputs(nil) = %#v, want nil", got)
	}
	if got := RecordInputs(&specs.Spec{}); got != nil {
		t.Fatalf("RecordInputs(no process) = %#v, want nil", got)
	}
}

func TestRecordInputsRecordsEveryVariableAndTheArguments(t *testing.T) {
	spec := testSpec([]string{"PATH=/usr/bin", "DB_PASSWORD=" + testSecretValue, "EMPTY="},
		"/kamin/kamin-entrypoint", "run", "--", "serve")
	m := RecordInputs(spec)
	if m == nil {
		t.Fatal("RecordInputs() = nil")
	}
	if m.Version != InputsVersion || !reflect.DeepEqual(m.Recorded, []string{InputEnv, InputArgs}) {
		t.Fatalf("version %d, recorded %v", m.Version, m.Recorded)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if got := len(m.Env); got != 3 {
		t.Fatalf("recorded %d variables, want 3", got)
	}
	for name, digest := range m.Env {
		if !isDigest(digest) {
			t.Errorf("digest of %s = %q, not a SHA-256 in hex", name, digest)
		}
	}
	want, err := m.EnvDigest("DB_PASSWORD", []string{"DB_PASSWORD=" + testSecretValue})
	if err != nil || want != m.Env["DB_PASSWORD"] {
		t.Fatalf("EnvDigest() = %q, %v; want the recorded %q", want, err, m.Env["DB_PASSWORD"])
	}
	args, err := m.ArgsDigest(spec.Process.Args)
	if err != nil || args != m.Args {
		t.Fatalf("ArgsDigest() = %q, %v; want the recorded %q", args, err, m.Args)
	}
}

// Each checkpoint gets its own salt, so equal values in two snapshots do not
// show up as equal digests.
func TestRecordInputsSaltsEveryRecord(t *testing.T) {
	spec := testSpec([]string{"DB_PASSWORD=" + testSecretValue})
	first, second := RecordInputs(spec), RecordInputs(spec)
	if first.Salt == second.Salt {
		t.Fatal("two records share a salt")
	}
	if first.Env["DB_PASSWORD"] == second.Env["DB_PASSWORD"] {
		t.Fatal("the same value has the same digest in two records")
	}
}

// The digest tells apart every way a variable can be set, so none of them can
// stand in for another.
func TestEnvDigestTellsEntriesApart(t *testing.T) {
	m := RecordInputs(testSpec(nil))
	cases := map[string][]string{
		"unset":          nil,
		"empty":          {"A="},
		"no equals sign": {"A"},
		"value":          {"A=x"},
		"other value":    {"A=y"},
		"set twice":      {"A=x", "A=x"},
		"value with =":   {"A=x=y"},
		"split value":    {"A=x", "A=y"},
		"joined value":   {"A=xA=y"},
	}
	seen := map[string]string{}
	for name, entries := range cases {
		digest, err := m.EnvDigest("A", entries)
		if err != nil {
			t.Fatalf("EnvDigest(%s): %v", name, err)
		}
		if other, ok := seen[digest]; ok {
			t.Fatalf("%s and %s have the same digest", name, other)
		}
		seen[digest] = name
	}
	envDigest, _ := m.EnvDigest("", []string{"x"})
	argsDigest, _ := m.ArgsDigest([]string{"x"})
	if envDigest == argsDigest {
		t.Fatal("an environment entry and an argument have the same digest")
	}
}

func TestGroupEnvKeepsWholeEntriesInOrder(t *testing.T) {
	got := GroupEnv([]string{"A=1", "B=2", "A=3", "C", "D=x=y"})
	want := map[string][]string{
		"A": {"A=1", "A=3"},
		"B": {"B=2"},
		"C": {"C"},
		"D": {"D=x=y"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GroupEnv() = %#v, want %#v", got, want)
	}
}

func TestValidateRefusesARecordItCannotRead(t *testing.T) {
	valid := func() *InputsManifest { return RecordInputs(testSpec([]string{"A=1"}, "serve")) }
	for name, tc := range map[string]struct {
		edit    func(*InputsManifest)
		wantErr string
	}{
		"another version":       {edit: func(m *InputsManifest) { m.Version = 2 }, wantErr: "version 2"},
		"no version":            {edit: func(m *InputsManifest) { m.Version = 0 }, wantErr: "version 0"},
		"short salt":            {edit: func(m *InputsManifest) { m.Salt = m.Salt[:10] }, wantErr: "salt"},
		"salt not hex":          {edit: func(m *InputsManifest) { m.Salt = strings.Repeat("z", 64) }, wantErr: "salt"},
		"malformed env digest":  {edit: func(m *InputsManifest) { m.Env["A"] = "abc" }, wantErr: `variable "A"`},
		"malformed args digest": {edit: func(m *InputsManifest) { m.Args = "" }, wantErr: "arguments"},
	} {
		t.Run(name, func(t *testing.T) {
			m := valid()
			tc.edit(m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
	// A kind this version does not know, recorded by a later one, does not
	// stop it from comparing the kinds it does.
	m := valid()
	m.Recorded = append(m.Recorded, "files")
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate() with an unknown kind: %v", err)
	}
}

// The manifest carries the record across the checkpoint and the restore,
// and it holds digests only: the value never reaches the file.
func TestInputsSurviveTheManifestWithoutTheirValues(t *testing.T) {
	dir := t.TempDir()
	original := NewCheckpointManifest("content-uid-123", "main", CRIUDumpManifest{},
		NewSourcePodManifest("ctr", 42, "node-1", "pod", "default", "", nil), OverlayManifest{}, HostManifest{})
	original.Inputs = RecordInputs(testSpec([]string{"DB_PASSWORD=" + testSecretValue}, "serve", "--token", testSecretValue))
	if err := WriteManifest(dir, original); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, manifestFilename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(content), testSecretValue) {
		t.Fatal("the manifest holds a recorded value")
	}
	loaded, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if !reflect.DeepEqual(loaded.Inputs, original.Inputs) {
		t.Fatalf("inputs = %#v, want %#v", loaded.Inputs, original.Inputs)
	}
}

// A checkpoint taken before the record existed reads back without one, which
// a restore that asks for the check refuses.
func TestManifestWithoutInputsReadsAsNotRecorded(t *testing.T) {
	dir := t.TempDir()
	original := NewCheckpointManifest("content-uid-123", "main", CRIUDumpManifest{},
		NewSourcePodManifest("ctr", 42, "node-1", "pod", "default", "", nil), OverlayManifest{}, HostManifest{})
	if err := WriteManifest(dir, original); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, manifestFilename))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(content), "inputs:") {
		t.Fatalf("a manifest without a record writes an inputs key:\n%s", content)
	}
	loaded, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if loaded.Inputs != nil {
		t.Fatalf("inputs = %#v, want nil", loaded.Inputs)
	}
}
