// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// InputsVersion is the version of InputsManifest this agent writes and reads:
// the digest construction in inputDigest. A record of another version is
// refused, never guessed at. Adding a kind of input (files, for example) to
// Recorded does not change it; changing how a digest is computed does.
const InputsVersion = 1

// Kinds of input an InputsManifest can hold, as listed in Recorded.
const (
	// InputEnv is the container process's environment variables.
	InputEnv = "env"
	// InputArgs is the container process's arguments.
	InputArgs = "args"
)

const (
	inputsSaltBytes    = 32
	inputsDigestDomain = "snapshot-restore-inputs/v1\n"
)

// InputsManifest records how a checkpointed container was started, so that a
// restore can check that its destination container was started the same way
// (podcontract.RestoreMatchInputsAnnotation).
//
// It holds salted SHA-256 digests, never values. The values themselves are in
// the checkpoint anyway, in the memory of the process it saved, so the
// digests disclose nothing to whoever can read the artifact; they only keep
// the values out of a small file that is more likely to be copied around.
// The salt is random per checkpoint, so equal values in two checkpoints do
// not have equal digests.
type InputsManifest struct {
	// Version is InputsVersion when written by this agent.
	Version int `yaml:"version"`
	// Recorded lists the kinds of input recorded, InputEnv and InputArgs. A
	// kind that is not listed was not recorded, which is not the same as
	// recorded empty.
	Recorded []string `yaml:"recorded"`
	// Salt is 32 random bytes, in hex, that start every digest of this record.
	Salt string `yaml:"salt"`
	// Env maps each environment variable the container had to the digest of
	// its entries in the runtime spec. A variable it did not have is absent.
	Env map[string]string `yaml:"env,omitempty"`
	// Args is the digest of the container process's arguments.
	Args string `yaml:"args,omitempty"`
}

// RecordInputs records the process of a container's runtime spec: every
// environment variable and the arguments. It returns nil for a spec without a
// process, from which nothing can be recorded.
func RecordInputs(spec *specs.Spec) *InputsManifest {
	if spec == nil || spec.Process == nil {
		return nil
	}
	salt := make([]byte, inputsSaltBytes)
	// crypto/rand.Read never returns an error: it crashes the program if the
	// system cannot supply randomness.
	_, _ = rand.Read(salt)
	m := &InputsManifest{
		Version:  InputsVersion,
		Recorded: []string{InputEnv, InputArgs},
		Salt:     hex.EncodeToString(salt),
	}
	groups := GroupEnv(spec.Process.Env)
	if len(groups) != 0 {
		m.Env = make(map[string]string, len(groups))
	}
	for name, entries := range groups {
		m.Env[name] = inputDigest(salt, InputEnv, name, entries)
	}
	m.Args = inputDigest(salt, InputArgs, "", spec.Process.Args)
	return m
}

// GroupEnv groups the environment entries of a runtime spec by variable name.
// Each entry is kept whole, NAME=VALUE, in order, so a variable set twice
// differs from one set once and an entry without "=" (its own name) differs
// from one with an empty value.
func GroupEnv(env []string) map[string][]string {
	groups := make(map[string][]string, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		groups[name] = append(groups[name], entry)
	}
	return groups
}

// Records reports whether the record holds the input kind.
func (m *InputsManifest) Records(kind string) bool {
	return slices.Contains(m.Recorded, kind)
}

// Validate reports a record this agent cannot compare against. Its errors
// name what is wrong, never a digest.
func (m *InputsManifest) Validate() error {
	if m.Version != InputsVersion {
		return fmt.Errorf("version %d, which this node agent does not read (it reads version %d)", m.Version, InputsVersion)
	}
	if _, err := m.salt(); err != nil {
		return err
	}
	for name, digest := range m.Env {
		if !isDigest(digest) {
			return fmt.Errorf("the digest of environment variable %q is malformed", name)
		}
	}
	if m.Records(InputArgs) && !isDigest(m.Args) {
		return errors.New("the digest of the arguments is malformed")
	}
	return nil
}

// EnvDigest is the digest of one variable's entries under this record's salt.
func (m *InputsManifest) EnvDigest(name string, entries []string) (string, error) {
	salt, err := m.salt()
	if err != nil {
		return "", err
	}
	return inputDigest(salt, InputEnv, name, entries), nil
}

// ArgsDigest is the digest of a process's arguments under this record's salt.
func (m *InputsManifest) ArgsDigest(args []string) (string, error) {
	salt, err := m.salt()
	if err != nil {
		return "", err
	}
	return inputDigest(salt, InputArgs, "", args), nil
}

func (m *InputsManifest) salt() ([]byte, error) {
	salt, err := hex.DecodeString(m.Salt)
	if err != nil || len(salt) != inputsSaltBytes {
		return nil, fmt.Errorf("the salt is not %d bytes of hex", inputsSaltBytes)
	}
	return salt, nil
}

func isDigest(s string) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == sha256.Size
}

// inputDigest is version 1 of the digest: SHA-256 over a fixed domain string,
// the salt, the kind, the name and each value, every variable-length part
// prefixed with its length so no two inputs encode alike.
func inputDigest(salt []byte, kind, name string, values []string) string {
	h := sha256.New()
	h.Write([]byte(inputsDigestDomain))
	h.Write(salt)
	writeLengthPrefixed(h, kind)
	writeLengthPrefixed(h, name)
	writeLength(h, len(values))
	for _, value := range values {
		writeLengthPrefixed(h, value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeLengthPrefixed(h hash.Hash, s string) {
	writeLength(h, len(s))
	h.Write([]byte(s))
}

func writeLength(h hash.Hash, n int) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	h.Write(buf[:])
}
