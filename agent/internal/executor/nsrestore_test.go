// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The restore clears a previous outcome from the control directory, but the
// agent's claim on the container stays: it is what keeps the workload from
// starting without a restore while this one runs.
func TestClearRestoreOutcomeKeepsTheClaim(t *testing.T) {
	dir := t.TempDir()
	claim := "owner=agent\ncontainer=abc123\n"
	for name, contents := range map[string]string{
		podcontract.RestoreCompleteFile: "pid=7\n",
		podcontract.RestoreFailedFile:   "owner=agent\n",
		podcontract.RestoreClaimFile:    claim,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := clearRestoreOutcome(dir); err != nil {
		t.Fatalf("clearRestoreOutcome: %v", err)
	}

	for _, name := range []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat error: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, podcontract.RestoreClaimFile))
	if err != nil || string(data) != claim {
		t.Fatalf("the claim is %q, %v; want it unchanged", data, err)
	}
}

func TestClearRestoreOutcome(t *testing.T) {
	if err := clearRestoreOutcome(t.TempDir()); err != nil {
		t.Fatalf("an empty control directory is already clear: %v", err)
	}
	if err := clearRestoreOutcome(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing control mount must fail the restore")
	}
}
