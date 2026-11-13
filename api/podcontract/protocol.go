// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Snapshot workload Pod annotations and control protocol.
const (
	// RestoreFromAnnotation names the PodSnapshot to restore in the Pod's
	// namespace.
	RestoreFromAnnotation = "nvidia.com/restore-from"

	// RestoreContainerMapAnnotation optionally maps the single captured source
	// container to one or more restore destinations. Its value is a
	// comma-separated list of source=destination pairs. When absent, restore
	// uses the captured container name as the destination.
	RestoreContainerMapAnnotation = "nvidia.com/restore-container-map"

	// RestoreFromMapAnnotation restores each destination container from its
	// own PodSnapshot in the Pod's namespace. Its value is a comma-separated
	// list of destination=snapshot pairs: the single captured container of
	// each named PodSnapshot restores into that destination. A Pod sets it
	// instead of RestoreFromAnnotation and RestoreContainerMapAnnotation, never
	// together with either.
	RestoreFromMapAnnotation = "nvidia.com/restore-from-map"

	// MaxSnapshotMappings is the most destinations one RestoreFromMapAnnotation
	// names. Each one is a restore that the node agent runs in parallel with
	// the others.
	MaxSnapshotMappings = 8

	// SkipCompatCheckAnnotation disables compatibility checks for one restore.
	SkipCompatCheckAnnotation = "nvidia.com/snapshot-skip-compat-check"

	// CuInterposeAnnotation enables multi-GPU support, which runs checkpoint
	// targets under the CUDA interposer.
	CuInterposeAnnotation = "nvidia.com/snapshot-multi-gpu"

	// DefaultSeccompLocalhostProfile is the kubelet-local profile installed by
	// the Snapshot Helm chart to block io_uring for CRIU.
	DefaultSeccompLocalhostProfile = "profiles/block-iouring.json"

	// SnapshotControlVolumeName is the per-Pod emptyDir used to carry capture and
	// restore lifecycle files between Snapshot and the workload.
	SnapshotControlVolumeName = "snapshot-control"

	// SnapshotControlMountPath is where the control volume is mounted inside a
	// Snapshot-managed workload container.
	SnapshotControlMountPath = "/snapshot-control"

	// SnapshotControlDirEnv is the canonical environment variable exposing the
	// control mount path to the workload.
	SnapshotControlDirEnv = "SNAPSHOT_CONTROL_DIR"

	// LegacySnapshotControlDirEnv is the deprecated environment variable
	// exposing the control mount path to the workload.
	//
	// Deprecated: use SnapshotControlDirEnv instead.
	LegacySnapshotControlDirEnv = "DYN_SNAPSHOT_CONTROL_DIR"

	// ReadyForSnapshotFile is written by the workload when the model is loaded
	// and the workload is ready for capture. The source Pod's kubelet readiness
	// probe observes it through the control volume.
	ReadyForSnapshotFile = "ready-for-snapshot"

	// CUDAJobFileName is the stable name under which the
	// cuda-checkpoint-helper launch-job wrapper persists the CUDA checkpoint job
	// file inside the control volume.
	CUDAJobFileName = "cuda-checkpoint-job"

	// CUDAJobFilePath is the full stable path to the persisted CUDA checkpoint
	// job file.
	CUDAJobFilePath = SnapshotControlMountPath + "/" + CUDAJobFileName

	// RestoreStandbyModeEnv asked standby-aware workload entrypoints to remain
	// inert until Snapshot replaced them with restored processes. Snapshot
	// published the name but never injected it.
	//
	// Deprecated: Snapshot now replaces a restore container's command with
	// RestoreProxyPath, so the entrypoint never runs and nothing needs to stay
	// inert. Restore Pods can keep the source's command.
	RestoreStandbyModeEnv = "SNAPSHOT_RESTORE_STANDBY"

	// LegacyRestoreStandbyModeEnv is the previous name of RestoreStandbyModeEnv,
	// kept for existing workload integrations that still read it. Snapshot
	// publishes the name but does not inject it.
	//
	// Deprecated: see RestoreStandbyModeEnv. This alias is removed in the next
	// release.
	LegacyRestoreStandbyModeEnv = "DYN_SNAPSHOT_RESTORE_STANDBY"

	// RestoreCompleteFile is written by the Snapshot agent when restore has
	// completed and the workload may resume. Its contents are key=value lines
	// built by FormatRestoreComplete; workloads that only gate on the file's
	// existence may ignore them.
	RestoreCompleteFile = "restore-complete"

	// RestoreFailedFile is written by the Snapshot agent before it kills a
	// destination container whose restore failed, or whose restore it ended
	// without running: refused, or its snapshot not ready in time. The
	// failing container need not act on it; a later container in the same Pod
	// reads it as proof that the Pod's one restore was already used. The agent
	// never restores into a container whose control directory holds it,
	// whoever wrote it.
	RestoreFailedFile = "restore-failed"

	// RestoreCompletePIDKey names the restored process's PID, relative to the
	// destination container's PID namespace, in RestoreCompleteFile.
	RestoreCompletePIDKey = "pid"

	// RestoreProxyBinaryName is the PID 1 process Snapshot runs in restore
	// destination containers. It waits for RestoreCompleteFile, then follows
	// the restored process and exits with its status.
	RestoreProxyBinaryName = "snapshot-restore-proxy"

	// RestoreProxyPath is the proxy as the container sees it, in the control
	// volume. It is the whole command of a restore destination container.
	RestoreProxyPath = SnapshotControlMountPath + "/" + RestoreProxyBinaryName
)

// IsReservedControlFile reports whether name is a file that Snapshot or its
// helpers write in the snapshot-control volume. A workload's post-restore file
// action must not name one: writing it would act as Snapshot's own signal.
func IsReservedControlFile(name string) bool {
	switch name {
	case RestoreCompleteFile, RestoreFailedFile, RestoreProxyBinaryName, CUDAJobFileName:
		return true
	}
	return false
}

// Exit codes reserved by the restore proxy. Every other code is the restored
// workload's own, or 128 plus the signal number that killed it.
const (
	// RestoreNotRepeatableExitCode means this Pod's one restore is spent: the
	// proxy found RestoreCompleteFile or RestoreFailedFile at start, left by an
	// earlier container, or RestoreFailedFile appeared while it waited.
	RestoreNotRepeatableExitCode = 112

	// RestoreProxyErrorExitCode means the proxy could not follow the restored
	// process, for example because RestoreCompleteFile was malformed or the
	// restored PID was not its child.
	RestoreProxyErrorExitCode = 113
)

// RestoreComplete is the content of RestoreCompleteFile.
type RestoreComplete struct {
	// PID is the restored process, relative to the destination container's
	// PID namespace.
	PID int
}

// FormatRestoreComplete renders the content of RestoreCompleteFile.
func FormatRestoreComplete(rc RestoreComplete) []byte {
	return []byte(RestoreCompletePIDKey + "=" + strconv.Itoa(rc.PID) + "\n")
}

// ParseRestoreComplete reads the content of RestoreCompleteFile. Unknown keys
// are ignored so the agent can add fields without breaking older readers.
func ParseRestoreComplete(data []byte) (RestoreComplete, error) {
	var rc RestoreComplete
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return RestoreComplete{}, fmt.Errorf("invalid %s line %q: expected key=value", RestoreCompleteFile, line)
		}
		if key != RestoreCompletePIDKey {
			continue
		}
		if rc.PID != 0 {
			return RestoreComplete{}, fmt.Errorf("duplicate %s key %q", RestoreCompleteFile, key)
		}
		pid, err := strconv.Atoi(value)
		if err != nil || pid <= 0 {
			return RestoreComplete{}, fmt.Errorf("invalid %s %s %q", RestoreCompleteFile, key, value)
		}
		rc.PID = pid
	}
	if rc.PID == 0 {
		return RestoreComplete{}, fmt.Errorf("%s has no %s", RestoreCompleteFile, RestoreCompletePIDKey)
	}
	return rc, nil
}

// ContainerMapping maps the one captured source container to a restore
// destination in the target Pod.
type ContainerMapping struct {
	Source      string
	Destination string
}

// SnapshotMapping names the PodSnapshot that one restore destination
// container restores from.
type SnapshotMapping struct {
	Destination  string
	SnapshotName string
}

// RestoreRequested reports whether annotations ask for a restore, with either
// the restore-from or the restore-from-map annotation.
func RestoreRequested(annotations map[string]string) bool {
	_, single := annotations[RestoreFromAnnotation]
	_, mapped := annotations[RestoreFromMapAnnotation]
	return single || mapped
}

// GetRestoreFromSnapshotName returns the same-namespace PodSnapshot named by
// the restore-from annotation. It fails when the Pod also sets restore-from-map.
func GetRestoreFromSnapshotName(annotations map[string]string) (string, error) {
	if err := rejectMixedRestoreAnnotations(annotations); err != nil {
		return "", err
	}
	return validateRestoreFromSnapshotName(annotations[RestoreFromAnnotation])
}

func validateRestoreFromSnapshotName(value string) (string, error) {
	snapshotName := strings.TrimSpace(value)
	if snapshotName == "" {
		return "", fmt.Errorf("%s must name a PodSnapshot", RestoreFromAnnotation)
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return "", fmt.Errorf(
			"%s value %q is not a valid PodSnapshot name: %s",
			RestoreFromAnnotation,
			snapshotName,
			strings.Join(errs, "; "),
		)
	}
	return snapshotName, nil
}

// ContainerMappingsFromAnnotations parses the optional flat restore mapping.
// Absence keeps the existing same-name restore behavior.
func ContainerMappingsFromAnnotations(
	annotations map[string]string,
	capturedSource string,
) ([]ContainerMapping, error) {
	raw, ok := annotations[RestoreContainerMapAnnotation]
	if !ok {
		capturedSource = strings.TrimSpace(capturedSource)
		return []ContainerMapping{{Source: capturedSource, Destination: capturedSource}}, nil
	}
	parts := strings.Split(strings.TrimSpace(raw), ",")
	mappings := make([]ContainerMapping, 0, len(parts))
	for _, part := range parts {
		pair := strings.Split(part, "=")
		if len(pair) != 2 {
			return nil, fmt.Errorf(
				"invalid %s entry %q: expected source=destination",
				RestoreContainerMapAnnotation,
				strings.TrimSpace(part),
			)
		}
		mappings = append(mappings, ContainerMapping{
			Source:      strings.TrimSpace(pair[0]),
			Destination: strings.TrimSpace(pair[1]),
		})
	}
	return mappings, nil
}

// ValidateContainerMappings enforces the one-source-to-many-destinations
// contract after parsing.
func ValidateContainerMappings(mappings []ContainerMapping, capturedSource string) error {
	capturedSource = strings.TrimSpace(capturedSource)
	if errs := validation.IsDNS1123Label(capturedSource); len(errs) != 0 {
		return fmt.Errorf("captured source container %q is invalid: %s", capturedSource, strings.Join(errs, "; "))
	}
	if len(mappings) == 0 {
		return fmt.Errorf("restore container mapping must contain at least one destination")
	}
	destinations := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		source := mapping.Source
		destination := mapping.Destination
		if errs := validation.IsDNS1123Label(source); len(errs) != 0 {
			return fmt.Errorf("invalid restore source container %q: %s", source, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Label(destination); len(errs) != 0 {
			return fmt.Errorf("invalid restore destination container %q: %s", destination, strings.Join(errs, "; "))
		}
		if source != capturedSource {
			return fmt.Errorf("restore source container %q does not match captured container %q", source, capturedSource)
		}
		if _, duplicate := destinations[destination]; duplicate {
			return fmt.Errorf("duplicate restore destination container %q", destination)
		}
		destinations[destination] = struct{}{}
	}
	return nil
}

// SnapshotMappingsFromAnnotations parses and validates the restore-from-map
// annotation. It returns nil without the annotation. A Pod that sets it
// together with restore-from or restore-container-map is rejected.
func SnapshotMappingsFromAnnotations(annotations map[string]string) ([]SnapshotMapping, error) {
	raw, ok := annotations[RestoreFromMapAnnotation]
	if !ok {
		return nil, nil
	}
	if err := rejectMixedRestoreAnnotations(annotations); err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimSpace(raw), ",")
	mappings := make([]SnapshotMapping, 0, len(parts))
	for _, part := range parts {
		pair := strings.Split(part, "=")
		if len(pair) != 2 {
			return nil, fmt.Errorf(
				"invalid %s entry %q: expected destination=snapshot",
				RestoreFromMapAnnotation,
				strings.TrimSpace(part),
			)
		}
		mappings = append(mappings, SnapshotMapping{
			Destination:  strings.TrimSpace(pair[0]),
			SnapshotName: strings.TrimSpace(pair[1]),
		})
	}
	if err := ValidateSnapshotMappings(mappings); err != nil {
		return nil, err
	}
	return mappings, nil
}

// ValidateSnapshotMappings enforces the restore-from-map contract: one to
// MaxSnapshotMappings destinations, each named once. Several destinations may
// restore from the same PodSnapshot.
func ValidateSnapshotMappings(mappings []SnapshotMapping) error {
	if len(mappings) == 0 {
		return fmt.Errorf("%s must map at least one destination container", RestoreFromMapAnnotation)
	}
	if len(mappings) > MaxSnapshotMappings {
		return fmt.Errorf("%s maps %d destination containers; the limit is %d",
			RestoreFromMapAnnotation, len(mappings), MaxSnapshotMappings)
	}
	destinations := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		if errs := validation.IsDNS1123Label(mapping.Destination); len(errs) != 0 {
			return fmt.Errorf("invalid restore destination container %q: %s", mapping.Destination, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Subdomain(mapping.SnapshotName); len(errs) != 0 {
			return fmt.Errorf(
				"restore destination container %q names invalid PodSnapshot %q: %s",
				mapping.Destination,
				mapping.SnapshotName,
				strings.Join(errs, "; "),
			)
		}
		if _, duplicate := destinations[mapping.Destination]; duplicate {
			return fmt.Errorf("duplicate restore destination container %q", mapping.Destination)
		}
		destinations[mapping.Destination] = struct{}{}
	}
	return nil
}

// rejectMixedRestoreAnnotations refuses a Pod that names its PodSnapshots both
// ways, so no reader has to choose between them.
func rejectMixedRestoreAnnotations(annotations map[string]string) error {
	if _, mapped := annotations[RestoreFromMapAnnotation]; !mapped {
		return nil
	}
	for _, name := range []string{RestoreFromAnnotation, RestoreContainerMapAnnotation} {
		if _, found := annotations[name]; found {
			return fmt.Errorf("%s cannot be combined with %s", RestoreFromMapAnnotation, name)
		}
	}
	return nil
}

// SkipCompatCheckFromAnnotations reports whether compatibility checks are disabled.
func SkipCompatCheckFromAnnotations(annotations map[string]string) bool {
	skip, err := strconv.ParseBool(strings.TrimSpace(annotations[SkipCompatCheckAnnotation]))
	return err == nil && skip
}
