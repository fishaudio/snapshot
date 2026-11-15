// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// workloadBinEnv names a workload entrypoint that claims its restore container
// before it starts without a restore, such as kamin-entrypoint. The test runs
// only when it is set.
const workloadBinEnv = "KAMIN_ENTRYPOINT_BIN"

// The agent's claim races a real workload entrypoint that gives up waiting for
// its restore, at random moments around the entrypoint's deadline. Whoever
// claims first owns the container: when the agent does, the workload never
// starts and the entrypoint exits 114 after its second wait; when the
// entrypoint does, the workload starts and the agent finds a whole
// owner=workload claim, never an empty or cut-short one.
func TestClaimRacesAWorkloadThatGivesUpWaiting(t *testing.T) {
	bin := os.Getenv(workloadBinEnv)
	if bin == "" {
		t.Skipf("%s is not set", workloadBinEnv)
	}
	const rounds = 300
	var agentWon, workloadWon int
	for round := range rounds {
		dir := t.TempDir()
		cmd := exec.Command(bin, "run", "--mode", "restore", "--control-dir", dir,
			"--restore-timeout", "20ms", "--poll-interval", "1ms", "--stop-timeout", "1s",
			"--", "/bin/sh", "-c", `echo ran > "$0/app-ran"`, dir)
		cmd.Env = []string{}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// The entrypoint marks the container once it starts waiting; claim
		// somewhere between 10 ms before and 10 ms after its deadline.
		waitFor(t, filepath.Join(dir, ".kamin-restore-attempted"))
		time.Sleep(time.Duration(10000+rand.IntN(20000)) * time.Microsecond)

		control, err := os.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		created, err := createFileInDir(control, podcontract.RestoreClaimFile,
			podcontract.FormatRestoreClaim(podcontract.RestoreClaim{Owner: podcontract.ControlFileOwnerAgent, ContainerID: "cid"}))
		if err != nil {
			t.Fatal(err)
		}
		if !created {
			data, _, err := readFileInDir(control, podcontract.RestoreClaimFile)
			claim, parseErr := podcontract.ParseRestoreClaim(data)
			if err != nil || parseErr != nil || claim.Owner != podcontract.ControlFileOwnerWorkload {
				t.Fatalf("round %d: the agent lost the claim and read it as %q (%v, %v)", round, data, err, parseErr)
			}
		}
		_ = control.Close()

		code := 0
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatal(err)
			}
			code = exitErr.ExitCode()
		}
		_, ranErr := os.Stat(filepath.Join(dir, "app-ran"))
		ran := ranErr == nil
		if created {
			agentWon++
			if ran || code != 114 {
				t.Fatalf("round %d: the agent claimed the container, but the workload started=%v, exit %d", round, ran, code)
			}
		} else {
			workloadWon++
			if !ran || code != 0 {
				t.Fatalf("round %d: the workload claimed the container, but started=%v, exit %d", round, ran, code)
			}
		}
	}
	t.Logf("rounds=%d agentWon=%d workloadWon=%d", rounds, agentWon, workloadWon)
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", path)
		}
		time.Sleep(100 * time.Microsecond)
	}
}
