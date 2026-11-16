// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// RestoreMatchInputsAnnotation asks the node agent to restore a destination
// container only if it was started with the same inputs as the container the
// snapshot was taken from. Its value is a JSON object keyed by destination
// container name; each entry is a RestoreInputs:
//
//	{"main": {"env": ["DB_PASSWORD", "API_URL"], "args": true}}
//
// Kubernetes resolves environment variables from Secrets and ConfigMaps, and
// expands $(VAR) in arguments, when a container starts. A restored process
// keeps the values it was started with at checkpoint time, so a container
// started after such a value changed would run the old one. The agent compares
// what the destination container's runtime spec holds against the digests the
// checkpoint recorded, after it claims the container and before it restores
// into it. A difference, a checkpoint without a record, or an entry the agent
// cannot read refuses the restore with RestoreReasonInputsChanged; the
// destination container then starts without a restore. Destinations the
// annotation does not name are not checked, and a Pod without it is restored
// as before.
const RestoreMatchInputsAnnotation = "nvidia.com/restore-match-inputs"

// MaxRestoreInputEnvNames bounds the environment variables one entry of
// RestoreMatchInputsAnnotation may name.
const MaxRestoreInputEnvNames = 1024

// RestoreInputs is one destination container's entry in
// RestoreMatchInputsAnnotation: which of its inputs must equal the snapshot's.
// An entry names at least one input.
type RestoreInputs struct {
	// Env names environment variables whose values must equal the
	// snapshot's. A variable the snapshot's container did not have must be
	// absent too. Only names: never put a value here.
	Env []string `json:"env,omitempty"`
	// Args asks that the container's arguments, as the runtime starts them
	// after Kubernetes expanded them, equal the snapshot's. It can be checked
	// only on a node without the restore proxy, which replaces the arguments.
	Args bool `json:"args,omitempty"`
}

// RestoreInputsFromAnnotations parses RestoreMatchInputsAnnotation. It
// returns nil without the annotation. Anything it cannot read in full is an
// error: an unknown field may ask for a check this version cannot make, and
// an input skipped in silence would let a changed one through.
func RestoreInputsFromAnnotations(annotations map[string]string) (map[string]RestoreInputs, error) {
	raw, ok := annotations[RestoreMatchInputsAnnotation]
	if !ok {
		return nil, nil
	}
	entries, err := decodeRestoreInputs(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RestoreMatchInputsAnnotation, err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s names no destination container", RestoreMatchInputsAnnotation)
	}
	for destination, inputs := range entries {
		if errs := validation.IsDNS1123Label(destination); len(errs) != 0 {
			return nil, fmt.Errorf("%s names invalid destination container %q: %s",
				RestoreMatchInputsAnnotation, destination, strings.Join(errs, "; "))
		}
		if err := validateRestoreInputs(inputs); err != nil {
			return nil, fmt.Errorf("%s entry %q: %w", RestoreMatchInputsAnnotation, destination, err)
		}
	}
	return entries, nil
}

// decodeRestoreInputs reads the annotation's JSON exactly: one object of
// objects, each with only the keys "env" and "args", spelled that way, and no
// key twice at any level. encoding/json would otherwise match keys without
// regard to case and let the last of two equal keys win, so a value could
// mean something other than what its writer saw.
func decodeRestoreInputs(raw string) (map[string]RestoreInputs, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, err
	}
	var objects map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &objects); err != nil {
		return nil, fmt.Errorf("not a JSON object of destination containers: %w", err)
	}
	if objects == nil {
		// JSON null decodes into a nil map.
		return nil, errors.New("not a JSON object of destination containers")
	}
	entries := make(map[string]RestoreInputs, len(objects))
	for destination, fields := range objects {
		if fields == nil {
			return nil, fmt.Errorf("entry %q is not a JSON object", destination)
		}
		var inputs RestoreInputs
		for key, value := range fields {
			var err error
			switch key {
			case "env":
				err = decodeStrict(value, &inputs.Env)
				if err == nil && inputs.Env == nil {
					err = errors.New("is null")
				}
			case "args":
				err = decodeStrict(value, &inputs.Args)
			default:
				err = errors.New("is not a known input")
			}
			if err != nil {
				return nil, fmt.Errorf("entry %q, key %q: %w", destination, key, err)
			}
		}
		entries[destination] = inputs
	}
	return entries, nil
}

// decodeStrict decodes one JSON value into target, refusing null for a
// scalar, which encoding/json would leave at its zero value.
func decodeStrict(value json.RawMessage, target any) error {
	if strings.TrimSpace(string(value)) == "null" {
		return errors.New("is null")
	}
	return json.Unmarshal(value, target)
}

// rejectDuplicateKeys walks the JSON tokens and fails on a key repeated in one
// object. Malformed JSON and trailing values are left to json.Unmarshal.
func rejectDuplicateKeys(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	type frame struct {
		object  bool
		keys    map[string]struct{}
		wantKey bool
	}
	var stack []*frame
	for {
		token, err := decoder.Token()
		if err != nil {
			// io.EOF after the first value, or a syntax error that
			// json.Unmarshal reports in full.
			return nil
		}
		var top *frame
		if len(stack) != 0 {
			top = stack[len(stack)-1]
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			// A nested object or array is the value of the key before it.
			if top != nil && top.object {
				top.wantKey = true
			}
			stack = append(stack, &frame{object: token == json.Delim('{'), keys: map[string]struct{}{}, wantKey: true})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
			continue
		}
		if top == nil || !top.object {
			continue
		}
		if top.wantKey {
			key, _ := token.(string)
			if _, duplicate := top.keys[key]; duplicate {
				return fmt.Errorf("key %q appears twice in one object", key)
			}
			top.keys[key] = struct{}{}
			top.wantKey = false
			continue
		}
		top.wantKey = true
	}
}

func validateRestoreInputs(inputs RestoreInputs) error {
	if len(inputs.Env) == 0 && !inputs.Args {
		return errors.New("names no input to compare")
	}
	if len(inputs.Env) > MaxRestoreInputEnvNames {
		return fmt.Errorf("names %d environment variables; the limit is %d", len(inputs.Env), MaxRestoreInputEnvNames)
	}
	seen := make(map[string]struct{}, len(inputs.Env))
	for _, name := range inputs.Env {
		// A runtime spec entry is NAME=VALUE, so a name holding "=" or a NUL
		// byte could never match one.
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("names environment variable %q twice", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// ValidateRestoreInputsDestinations checks that every container
// RestoreMatchInputsAnnotation names is one of the Pod's restore destinations.
// An entry for any other container is a request that would never be met.
func ValidateRestoreInputsDestinations(entries map[string]RestoreInputs, destinations []string) error {
	for destination := range entries {
		if !slices.Contains(destinations, destination) {
			return fmt.Errorf("%s names container %q, which is not a restore destination of this Pod",
				RestoreMatchInputsAnnotation, destination)
		}
	}
	return nil
}
