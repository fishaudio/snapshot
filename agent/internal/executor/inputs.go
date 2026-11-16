// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// RestoreInputsError refuses a restore because its destination container was
// not shown to be started with the inputs the snapshot recorded
// (podcontract.RestoreMatchInputsAnnotation). The restore is then ended like
// an incompatible one, and the Pod's outcome is
// podcontract.RestoreReasonInputsChanged.
//
// Its message names inputs, never their values or digests: the values are
// often credentials, and the message reaches the Pod's condition, events and
// logs.
type RestoreInputsError struct {
	// Env names, sorted, the environment variables whose values differ from
	// the snapshot's, including one set on only one side.
	Env []string
	// Args reports that the arguments differ from the snapshot's.
	Args bool
	// Unverifiable says why the inputs could not be compared at all. It is
	// empty when they were compared and differ.
	Unverifiable string
}

func (e *RestoreInputsError) Error() string {
	if e.Unverifiable != "" {
		return "cannot check that the destination container was started with the snapshot's inputs: " + e.Unverifiable
	}
	var differ []string
	if len(e.Env) != 0 {
		differ = append(differ, "environment variables "+strings.Join(e.Env, ", "))
	}
	if e.Args {
		differ = append(differ, "arguments")
	}
	return "the destination container was started with inputs that differ from the snapshot's: " + strings.Join(differ, "; ")
}

func unverifiableInputs(why string) *RestoreInputsError {
	return &RestoreInputsError{Unverifiable: why}
}

// checkRestoreInputs compares the inputs want names between the destination
// container's runtime spec and the checkpoint's record. A nil want asks for
// nothing. Anything short of a comparison that shows them equal refuses the
// restore: a missing or unreadable record, a spec without a process, or
// arguments on a node whose restore proxy replaced them.
//
// It runs whether or not compatibility checks are skipped: those trade safety
// for reach across nodes, while this check guards which values the restored
// process holds.
func checkRestoreInputs(want *podcontract.RestoreInputs, recorded *types.InputsManifest, spec *specs.Spec, withoutProxy bool) error {
	if want == nil {
		return nil
	}
	if recorded == nil {
		return unverifiableInputs("the snapshot recorded no inputs; it was taken by a node agent that does not record them")
	}
	if err := recorded.Validate(); err != nil {
		return unverifiableInputs("the snapshot's record of its inputs cannot be read: " + err.Error())
	}
	if spec == nil || spec.Process == nil {
		return unverifiableInputs("the destination container's runtime spec has no process")
	}

	differs := &RestoreInputsError{}
	if len(want.Env) != 0 {
		if !recorded.Records(types.InputEnv) {
			return unverifiableInputs("the snapshot did not record environment variables")
		}
		groups := types.GroupEnv(spec.Process.Env)
		for _, name := range want.Env {
			entries, set := groups[name]
			digest, had := recorded.Env[name]
			if !set && !had {
				continue
			}
			if set != had {
				differs.Env = append(differs.Env, name)
				continue
			}
			got, err := recorded.EnvDigest(name, entries)
			if err != nil {
				return unverifiableInputs("the snapshot's record of its inputs cannot be read: " + err.Error())
			}
			if got != digest {
				differs.Env = append(differs.Env, name)
			}
		}
		slices.Sort(differs.Env)
	}
	if want.Args {
		if !withoutProxy {
			return unverifiableInputs("the restore proxy replaces the destination container's arguments, so they cannot be compared")
		}
		if !recorded.Records(types.InputArgs) {
			return unverifiableInputs("the snapshot did not record arguments")
		}
		got, err := recorded.ArgsDigest(spec.Process.Args)
		if err != nil {
			return unverifiableInputs("the snapshot's record of its inputs cannot be read: " + err.Error())
		}
		differs.Args = got != recorded.Args
	}
	if len(differs.Env) == 0 && !differs.Args {
		return nil
	}
	return differs
}
