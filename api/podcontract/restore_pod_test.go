// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func restorePodFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "restore-worker",
			Namespace:   "inference",
			Annotations: map[string]string{"example.com/team": "inference"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "worker:latest",
					Command: []string{"python3"},
					Args:    []string{"serve.py"},
				},
				{Name: "sidecar", Image: "sidecar:latest"},
			},
		},
	}
}

func restoreRequest(mappings ...ContainerMapping) Request {
	return Request{
		SnapshotName:    "snapshot-a",
		SourceContainer: "main",
		Mappings:        mappings,
	}
}

func TestBuildShapesSingleDestination(t *testing.T) {
	original := restorePodFixture()
	before := original.DeepCopy()
	options := Options{
		SeccompProfile:    DefaultSeccompLocalhostProfile,
		EnableStartupGate: true,
	}

	shaped, err := Build(original, restoreRequest(), options)
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("Build() mutated its input")
	}
	if shaped.Annotations[RestoreFromAnnotation] != "snapshot-a" {
		t.Fatalf("%s = %q", RestoreFromAnnotation, shaped.Annotations[RestoreFromAnnotation])
	}
	if _, found := shaped.Annotations[RestoreContainerMapAnnotation]; found {
		t.Fatalf("same-name restore unexpectedly set %s", RestoreContainerMapAnnotation)
	}
	if shaped.Annotations["example.com/team"] != "inference" {
		t.Fatal("unrelated annotation was not preserved")
	}
	if len(shaped.Spec.Volumes) != 1 || shaped.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("expected one snapshot control emptyDir, got %#v", shaped.Spec.Volumes)
	}

	main := &shaped.Spec.Containers[0]
	if !reflect.DeepEqual(main.Command, []string{"python3"}) || !reflect.DeepEqual(main.Args, []string{"serve.py"}) {
		t.Fatalf("workload command changed: command=%v args=%v", main.Command, main.Args)
	}
	if len(main.VolumeMounts) != 1 || main.VolumeMounts[0].SubPath != "main" {
		t.Fatalf("unexpected snapshot control mount: %#v", main.VolumeMounts)
	}
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if got := restoreEnvValue(main.Env, name); got != SnapshotControlMountPath {
			t.Fatalf("%s = %q, want %q", name, got, SnapshotControlMountPath)
		}
	}
	for _, name := range []string{RestoreStandbyModeEnv, LegacyRestoreStandbyModeEnv} {
		if got := restoreEnvValue(main.Env, name); got != "" {
			t.Fatalf("generic builder injected workload-specific %s value %q", name, got)
		}
	}
	if main.StartupProbe == nil ||
		main.StartupProbe.Exec == nil ||
		!reflect.DeepEqual(
			main.StartupProbe.Exec.Command,
			[]string{"cat", SnapshotControlMountPath + "/" + RestoreCompleteFile},
		) {
		t.Fatalf("unexpected restore startup gate: %#v", main.StartupProbe)
	}
	if shaped.Spec.SecurityContext != nil {
		t.Fatalf("restore profile was applied at Pod scope: %#v", shaped.Spec.SecurityContext)
	}
	if main.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(main.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("destination is missing expected seccomp profile: %#v", main.SecurityContext)
	}
	if !reflect.DeepEqual(shaped.Spec.Containers[1], before.Spec.Containers[1]) {
		t.Fatal("non-destination sidecar was modified")
	}
	if err := Validate(shaped, restoreRequest()); err != nil {
		t.Fatalf("Validate() rejected builder output: %v", err)
	}
}

func TestBuildShapesFanoutIdempotently(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
	request := restoreRequest(
		ContainerMapping{Source: "main", Destination: "engine-1"},
		ContainerMapping{Source: "main", Destination: "engine-0"},
	)
	options := Options{SeccompProfile: DefaultSeccompLocalhostProfile}

	first, err := Build(pod, request, options)
	if err != nil {
		t.Fatalf("first Build() failed: %v", err)
	}
	second, err := Build(first, request, options)
	if err != nil {
		t.Fatalf("second Build() failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Build() is not idempotent")
	}
	if got := first.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
		t.Fatalf("%s = %q", RestoreContainerMapAnnotation, got)
	}
	if len(first.Spec.Volumes) != 1 {
		t.Fatalf("expected one shared volume, got %d", len(first.Spec.Volumes))
	}
	for i, name := range []string{"engine-0", "engine-1"} {
		container := &first.Spec.Containers[i]
		if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != name {
			t.Fatalf("container %q mount = %#v", name, container.VolumeMounts)
		}
		if container.SecurityContext == nil ||
			!matchesLocalhostSeccompProfile(container.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
			t.Fatalf("container %q is missing expected seccomp profile: %#v", name, container.SecurityContext)
		}
	}
}

func TestBuildStartupGateIsOptIn(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/live"}},
	}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}},
	}
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/startup"}},
		PeriodSeconds: 3,
	}
	beforeLiveness := pod.Spec.Containers[0].LivenessProbe.DeepCopy()
	beforeReadiness := pod.Spec.Containers[0].ReadinessProbe.DeepCopy()
	beforeStartup := pod.Spec.Containers[0].StartupProbe.DeepCopy()

	ungated, err := Build(pod, restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("ungated Build() failed: %v", err)
	}
	if !reflect.DeepEqual(ungated.Spec.Containers[0].StartupProbe, beforeStartup) {
		t.Fatal("zero-value options changed the workload startup probe")
	}

	gated, err := Build(pod, restoreRequest(), Options{EnableStartupGate: true})
	if err != nil {
		t.Fatalf("gated Build() failed: %v", err)
	}
	main := &gated.Spec.Containers[0]
	if !reflect.DeepEqual(main.LivenessProbe, beforeLiveness) || !reflect.DeepEqual(main.ReadinessProbe, beforeReadiness) {
		t.Fatal("workload liveness or readiness probe was modified")
	}
	if reflect.DeepEqual(main.StartupProbe, beforeStartup) {
		t.Fatal("workload startup probe was not replaced by the restore gate")
	}
	if !reflect.DeepEqual(pod.Spec.Containers[0].StartupProbe, beforeStartup) {
		t.Fatal("Build() mutated the input startup probe")
	}
	if !reflect.DeepEqual(main.StartupProbe, canonicalRestoreStartupProbe()) {
		t.Fatalf("unexpected restore startup gate: %#v", main.StartupProbe)
	}
}

func TestBuildCanonicalizesEquivalentMappings(t *testing.T) {
	t.Run("fanout", func(t *testing.T) {
		pod := restorePodFixture()
		pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
		pod.Annotations[RestoreContainerMapAnnotation] = "main=engine-1, main=engine-0"
		request := restoreRequest(
			ContainerMapping{Source: "main", Destination: "engine-0"},
			ContainerMapping{Source: "main", Destination: "engine-1"},
		)

		shaped, err := Build(pod, request, Options{})
		if err != nil {
			t.Fatalf("Build() failed: %v", err)
		}
		if got := shaped.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
			t.Fatalf("canonical mapping = %q", got)
		}
	})

	t.Run("same name", func(t *testing.T) {
		pod := restorePodFixture()
		pod.Annotations[RestoreContainerMapAnnotation] = "main=main"
		shaped, err := Build(pod, restoreRequest(), Options{})
		if err != nil {
			t.Fatalf("Build() failed: %v", err)
		}
		if _, found := shaped.Annotations[RestoreContainerMapAnnotation]; found {
			t.Fatalf("canonical same-name restore retained %s", RestoreContainerMapAnnotation)
		}
	})
}

func TestBuildRejectsConflictsAtomically(t *testing.T) {
	runtimeDefault := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	tests := []struct {
		name     string
		mutate   func(*corev1.Pod)
		mappings []ContainerMapping
	}{
		{
			name: "restore annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreFromAnnotation] = "snapshot-b"
			},
		},
		{
			name: "mapping annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreContainerMapAnnotation] = "main=sidecar"
			},
		},
		{
			name: "control volume",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name: SnapshotControlVolumeName,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "wrong",
					}},
				}}
			},
		},
		{
			name: "control mount name",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: SnapshotControlVolumeName, MountPath: "/wrong", SubPath: "main",
				}}
			},
		},
		{
			name: "control mount path",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: "other", MountPath: SnapshotControlMountPath,
				}}
			},
		},
		{
			name: "control environment",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: SnapshotControlDirEnv, Value: "/wrong"}}
			},
		},
		{
			name: "container seccomp",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
			},
		},
		{
			name:     "missing destination",
			mappings: []ContainerMapping{{Source: "main", Destination: "missing"}},
		},
		{
			name: "duplicate destination",
			mappings: []ContainerMapping{
				{Source: "main", Destination: "main"},
				{Source: "main", Destination: "main"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := restorePodFixture()
			if test.mutate != nil {
				test.mutate(pod)
			}
			before := pod.DeepCopy()

			if _, err := Build(
				pod,
				restoreRequest(test.mappings...),
				Options{SeccompProfile: DefaultSeccompLocalhostProfile},
			); err == nil {
				t.Fatal("Build() unexpectedly succeeded")
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("Build() mutated input after failure")
			}
		})
	}
}

func TestBuildScopesSeccompToRestoreDestinations(t *testing.T) {
	runtimeDefault := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	pod := restorePodFixture()
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
	beforeSidecar := pod.Spec.Containers[1].DeepCopy()

	shaped, err := Build(
		pod,
		restoreRequest(),
		Options{SeccompProfile: DefaultSeccompLocalhostProfile},
	)
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if !reflect.DeepEqual(shaped.Spec.SecurityContext.SeccompProfile, runtimeDefault) {
		t.Fatalf("Pod seccomp profile changed: %#v", shaped.Spec.SecurityContext.SeccompProfile)
	}
	main := &shaped.Spec.Containers[0]
	if main.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(main.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("destination is missing expected seccomp profile: %#v", main.SecurityContext)
	}
	if !reflect.DeepEqual(&shaped.Spec.Containers[1], beforeSidecar) {
		t.Fatalf("non-destination sidecar changed: %#v", shaped.Spec.Containers[1])
	}
}

func TestValidateMinimumContract(t *testing.T) {
	pod, err := Build(restorePodFixture(), restoreRequest(), Options{
		SeccompProfile:    DefaultSeccompLocalhostProfile,
		EnableStartupGate: true,
	})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Legacy environment compatibility, startup gating, and seccomp selection
	// are producer policy rather than agent-facing minimum protocol.
	pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, LegacySnapshotControlDirEnv)
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
	}
	pod.Spec.Containers[0].SecurityContext.SeccompProfile = nil
	if err := Validate(pod, restoreRequest()); err != nil {
		t.Fatalf("Validate() rejected minimum restore protocol: %v", err)
	}

	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  LegacySnapshotControlDirEnv,
		Value: "/wrong",
	})
	if err := Validate(pod, restoreRequest()); err == nil {
		t.Fatal("Validate() accepted conflicting deprecated environment alias")
	}
}

func TestValidateRejectsMinimumContractDrift(t *testing.T) {
	valid, err := Build(restorePodFixture(), restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	tests := map[string]func(*corev1.Pod){
		"annotation": func(pod *corev1.Pod) {
			delete(pod.Annotations, RestoreFromAnnotation)
		},
		"volume": func(pod *corev1.Pod) {
			pod.Spec.Volumes = nil
		},
		"mount": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts[0].SubPath = "other"
		},
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, SnapshotControlDirEnv)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pod := valid.DeepCopy()
			mutate(pod)
			if err := Validate(pod, restoreRequest()); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestBuildAllowsUnmanagedSeccomp(t *testing.T) {
	pod := restorePodFixture()
	shaped, err := Build(pod, restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if shaped.Spec.SecurityContext != nil {
		t.Fatalf("empty option unexpectedly changed security context: %#v", shaped.Spec.SecurityContext)
	}
	if shaped.Spec.Containers[0].SecurityContext != nil {
		t.Fatalf(
			"empty option unexpectedly changed destination security context: %#v",
			shaped.Spec.Containers[0].SecurityContext,
		)
	}
}

func restoreEnvValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func removeRestoreEnv(env []corev1.EnvVar, name string) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(env))
	for _, item := range env {
		if item.Name != name {
			result = append(result, item)
		}
	}
	return result
}

func fanInPodFixture() *corev1.Pod {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{
		{Name: "prefill", Image: "worker:latest", Command: []string{"python3"}, Args: []string{"prefill.py"}},
		{Name: "decode", Image: "worker:latest", Command: []string{"python3"}, Args: []string{"decode.py"}},
		{Name: "router", Image: "router:latest"},
	}
	return pod
}

func fanInRequest() Request {
	return Request{SnapshotMappings: []SnapshotMapping{
		{Destination: "prefill", SnapshotName: "snap-prefill"},
		{Destination: "decode", SnapshotName: "snap-decode"},
	}}
}

func TestBuildShapesSnapshotMappingsIdempotently(t *testing.T) {
	original := fanInPodFixture()
	before := original.DeepCopy()
	options := Options{SeccompProfile: DefaultSeccompLocalhostProfile, EnableStartupGate: true}

	first, err := Build(original, fanInRequest(), options)
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("Build() mutated its input")
	}
	second, err := Build(first, fanInRequest(), options)
	if err != nil {
		t.Fatalf("second Build() failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Build() is not idempotent")
	}
	if got := first.Annotations[RestoreFromMapAnnotation]; got != "decode=snap-decode,prefill=snap-prefill" {
		t.Fatalf("%s = %q", RestoreFromMapAnnotation, got)
	}
	for _, name := range []string{RestoreFromAnnotation, RestoreContainerMapAnnotation} {
		if _, found := first.Annotations[name]; found {
			t.Fatalf("Build() set %s for a restore-from-map request", name)
		}
	}
	if first.Annotations["example.com/team"] != "inference" {
		t.Fatal("unrelated annotation was not preserved")
	}
	if len(first.Spec.Volumes) != 1 {
		t.Fatalf("expected one shared volume, got %d", len(first.Spec.Volumes))
	}
	for i, name := range []string{"prefill", "decode"} {
		container := &first.Spec.Containers[i]
		if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != name {
			t.Fatalf("container %q mount = %#v", name, container.VolumeMounts)
		}
		if got := restoreEnvValue(container.Env, SnapshotControlDirEnv); got != SnapshotControlMountPath {
			t.Fatalf("container %q %s = %q", name, SnapshotControlDirEnv, got)
		}
		if !reflect.DeepEqual(container.StartupProbe, canonicalRestoreStartupProbe()) {
			t.Fatalf("container %q startup gate = %#v", name, container.StartupProbe)
		}
		if !reflect.DeepEqual(container.Args, before.Spec.Containers[i].Args) {
			t.Fatalf("container %q workload args changed: %v", name, container.Args)
		}
	}
	if !reflect.DeepEqual(first.Spec.Containers[2], before.Spec.Containers[2]) {
		t.Fatal("non-destination container was modified")
	}

	reordered := Request{SnapshotMappings: []SnapshotMapping{
		{Destination: "decode", SnapshotName: " snap-decode "},
		{Destination: "prefill", SnapshotName: "snap-prefill"},
	}}
	if err := Validate(first, reordered); err != nil {
		t.Fatalf("Validate() rejected builder output: %v", err)
	}
}

func TestBuildShapesSnapshotMappingsThatShareAPodSnapshot(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
	request := Request{SnapshotMappings: []SnapshotMapping{
		{Destination: "engine-1", SnapshotName: "snapshot-a"},
		{Destination: "engine-0", SnapshotName: "snapshot-a"},
	}}

	shaped, err := Build(pod, request, Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if got := shaped.Annotations[RestoreFromMapAnnotation]; got != "engine-0=snapshot-a,engine-1=snapshot-a" {
		t.Fatalf("%s = %q", RestoreFromMapAnnotation, got)
	}
	if err := Validate(shaped, request); err != nil {
		t.Fatalf("Validate() rejected builder output: %v", err)
	}
}

func TestBuildRejectsSnapshotMappingConflictsAtomically(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*corev1.Pod)
		request Request
	}{
		{
			name:   "restore-from on the Pod",
			mutate: func(pod *corev1.Pod) { pod.Annotations[RestoreFromAnnotation] = "snap-prefill" },
		},
		{
			name:   "container map on the Pod",
			mutate: func(pod *corev1.Pod) { pod.Annotations[RestoreContainerMapAnnotation] = "main=prefill" },
		},
		{
			name:   "different restore-from-map on the Pod",
			mutate: func(pod *corev1.Pod) { pod.Annotations[RestoreFromMapAnnotation] = "prefill=snap-other" },
		},
		{
			name: "SnapshotName with SnapshotMappings",
			request: Request{SnapshotName: "snap-prefill", SnapshotMappings: []SnapshotMapping{
				{Destination: "prefill", SnapshotName: "snap-prefill"},
			}},
		},
		{
			name: "Mappings with SnapshotMappings",
			request: Request{
				Mappings:         []ContainerMapping{{Source: "main", Destination: "prefill"}},
				SnapshotMappings: []SnapshotMapping{{Destination: "prefill", SnapshotName: "snap-prefill"}},
			},
		},
		{
			name: "missing destination",
			request: Request{SnapshotMappings: []SnapshotMapping{
				{Destination: "missing", SnapshotName: "snap-prefill"},
			}},
		},
		{
			name: "duplicate destination",
			request: Request{SnapshotMappings: []SnapshotMapping{
				{Destination: "prefill", SnapshotName: "snap-prefill"},
				{Destination: "prefill", SnapshotName: "snap-decode"},
			}},
		},
		{
			name: "invalid PodSnapshot name",
			request: Request{SnapshotMappings: []SnapshotMapping{
				{Destination: "prefill", SnapshotName: "Bad_Name"},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := fanInPodFixture()
			if test.mutate != nil {
				test.mutate(pod)
			}
			request := test.request
			if request.SnapshotMappings == nil {
				request = fanInRequest()
			}
			before := pod.DeepCopy()

			if _, err := Build(pod, request, Options{}); err == nil {
				t.Fatal("Build() unexpectedly succeeded")
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("Build() mutated input after failure")
			}
		})
	}
}

func TestBuildSingleSnapshotRejectsRestoreFromMap(t *testing.T) {
	pod := restorePodFixture()
	pod.Annotations[RestoreFromMapAnnotation] = "main=snapshot-a"

	if _, err := Build(pod, restoreRequest(), Options{}); err == nil {
		t.Fatal("Build() accepted a restore-from request for a Pod that sets restore-from-map")
	}
}

func TestValidateRejectsSnapshotMappingDrift(t *testing.T) {
	valid, err := Build(fanInPodFixture(), fanInRequest(), Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if err := Validate(valid, fanInRequest()); err != nil {
		t.Fatalf("Validate() rejected builder output: %v", err)
	}
	tests := map[string]func(*corev1.Pod){
		"annotation missing": func(pod *corev1.Pod) {
			delete(pod.Annotations, RestoreFromMapAnnotation)
		},
		"annotation names another PodSnapshot": func(pod *corev1.Pod) {
			pod.Annotations[RestoreFromMapAnnotation] = "decode=snap-other,prefill=snap-prefill"
		},
		"annotation drops a destination": func(pod *corev1.Pod) {
			pod.Annotations[RestoreFromMapAnnotation] = "prefill=snap-prefill"
		},
		"restore-from added": func(pod *corev1.Pod) {
			pod.Annotations[RestoreFromAnnotation] = "snap-prefill"
		},
		"shared control directory": func(pod *corev1.Pod) {
			pod.Spec.Containers[1].VolumeMounts[0].SubPath = "prefill"
		},
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[1].Env = removeRestoreEnv(pod.Spec.Containers[1].Env, SnapshotControlDirEnv)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pod := valid.DeepCopy()
			mutate(pod)
			if err := Validate(pod, fanInRequest()); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}

	// A restore-from request does not match a restore-from-map Pod.
	if err := Validate(valid, restoreRequest()); err == nil {
		t.Fatal("Validate() accepted a restore-from-map Pod for a restore-from request")
	}
}
