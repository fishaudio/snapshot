// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build kamin_claim_race

package runtime

// The agent's control-file operations, reaching a container's control
// directory through procRoot/<pid>/root instead of HostProcPath. Only the
// opt-in restore-claim race test builds this file (build tag
// kamin_claim_race, agent/internal/controller/restore_claim_race_test.go): it
// runs these operations against a real workload process on a machine without
// the agent's /host/proc mount.

// ClaimControlFileUnder is ClaimControlFile through procRoot.
func ClaimControlFileUnder(procRoot string, hostPID int, name string, contents []byte) (bool, error) {
	return claimControlFile(procRoot, hostPID, name, contents)
}

// ReadControlFileUnder is ReadControlFile through procRoot.
func ReadControlFileUnder(procRoot string, hostPID int, name string) ([]byte, bool, error) {
	return readControlFile(procRoot, hostPID, name)
}

// ControlSentinelExistsUnder is ControlSentinelExists through procRoot.
func ControlSentinelExistsUnder(procRoot string, hostPID int, name string) (bool, error) {
	return controlSentinelExists(procRoot, hostPID, name)
}

// WriteControlSentinelUnder is WriteControlSentinel through procRoot.
func WriteControlSentinelUnder(procRoot string, hostPID int, name string, contents []byte) error {
	return writeControlSentinel(procRoot, hostPID, name, contents)
}
