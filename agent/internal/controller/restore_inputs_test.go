// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const (
	checkpointSecret = "checkpoint-s3cr3t-value"
	restoreSecret    = "restore-s3cr3t-value"
)

// specRuntime resolves every container to the default placeholder, whose
// runtime spec is spec.
type specRuntime struct {
	*fakeRuntime
	spec *specs.Spec
}

func (r *specRuntime) ResolveContainer(_ context.Context, _ string) (int, *specs.Spec, error) {
	return defaultPlaceholderPID, r.spec, nil
}

// inputsRestore is one restore Pod that asks to match its inputs, driven
// through the real executor up to the inputs check.
type inputsRestore struct {
	controller *NodeController
	pod        *corev1.Pod
	volume     *controlVolume
	logs       *logRecorder
	recorded   *types.InputsManifest
}

func newInputsRestore(t *testing.T, annotation string, checkpointEnv, restoreEnv []string) *inputsRestore {
	t.Helper()
	pod := restorePod(map[string]string{
		podcontract.RestoreFromAnnotation:        "snapshot-a",
		podcontract.RestoreMatchInputsAnnotation: annotation,
	})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.RestoreWithoutProxy()
	logs := &logRecorder{}
	w.log = logr.New(&recordingSink{recorder: logs})
	w.compareFn = func(compat.Gate, compat.Environment, compat.Environment) []compat.Mismatch { return nil }
	w.runtime = &specRuntime{fakeRuntime: &fakeRuntime{}, spec: &specs.Spec{Process: &specs.Process{Env: restoreEnv, Args: []string{"serve"}}}}
	volume := newControlVolume(map[int]string{defaultPlaceholderPID: "main"})
	volume.install(w)

	recorded := types.RecordInputs(&specs.Spec{Process: &specs.Process{Env: checkpointEnv, Args: []string{"serve"}}})
	writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
		Inputs:   recorded,
	})
	return &inputsRestore{controller: w, pod: pod, volume: volume, logs: logs, recorded: recorded}
}

// published is every string the restore put where others can read it: the
// condition, the events, and the log records with all their fields.
func (r *inputsRestore) published(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	condition := appliedRestoredCondition(t, r.controller)
	out.WriteString(condition.Reason + " " + condition.Message + "\n")
	for _, action := range r.controller.clientset.(*fake.Clientset).Actions() {
		if create, ok := action.(clientgotesting.CreateAction); ok {
			if event, ok := create.GetObject().(*corev1.Event); ok {
				out.WriteString(event.Reason + " " + event.Message + "\n")
			}
		}
	}
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	for _, record := range r.logs.records {
		fmt.Fprintf(&out, "%s %v\n", record.message, record.fields)
	}
	return out.String()
}

// A destination container started with a value other than the snapshot's is
// refused after its claim and before any restore step, ended like an
// incompatible one, and reported as RestoreInputsChanged. Nothing the agent
// publishes holds either value or a digest.
func TestARestoreWhoseInputsChangedIsRefusedAndEnded(t *testing.T) {
	r := newInputsRestore(t, `{"main": {"env": ["DB_PASSWORD"]}}`,
		[]string{"DB_PASSWORD=" + checkpointSecret}, []string{"DB_PASSWORD=" + restoreSecret})

	processQueuedRestorePod(t, r.controller, r.pod)

	condition := appliedRestoredCondition(t, r.controller)
	assert.Equal(t, podcontract.RestoreReasonInputsChanged, condition.Reason)
	assert.Equal(t, corev1.ConditionFalse, condition.Status)
	assert.Equal(t, "Refused restore; the destination container was not shown to be started with the snapshot's inputs: "+
		"the destination container was started with inputs that differ from the snapshot's: environment variables DB_PASSWORD",
		condition.Message)
	clientset := r.controller.clientset.(*fake.Clientset)
	assert.Len(t, eventsForReason(clientset, podcontract.RestoreReasonInputsChanged), 1)
	assert.Empty(t, eventsForReason(clientset, podcontract.RestoreReasonFailed))

	// Claimed by the agent, then ended: restore-failed and a kill, so the
	// container starts again without a restore.
	claim, err := podcontract.ParseRestoreClaim([]byte(r.volume.read("main", podcontract.RestoreClaimFile)))
	require.NoError(t, err)
	assert.Equal(t, podcontract.ControlFileOwnerAgent, claim.Owner)
	assert.True(t, r.volume.holds("main", podcontract.RestoreFailedFile))
	assert.Equal(t, []string{"main"}, r.volume.kills())

	published := r.published(t)
	for _, secret := range []string{checkpointSecret, restoreSecret, r.recorded.Salt, r.recorded.Env["DB_PASSWORD"]} {
		assert.NotContains(t, published, secret)
	}
	assert.Contains(t, published, "DB_PASSWORD")
}

// A snapshot taken by an agent that did not record inputs cannot be shown to
// match, so a Pod that asks for the check is refused the same way.
func TestARestoreFromASnapshotWithoutARecordIsRefused(t *testing.T) {
	r := newInputsRestore(t, `{"main": {"env": ["DB_PASSWORD"]}}`, nil, []string{"DB_PASSWORD=" + restoreSecret})
	_, content := readySnapshotObjects()
	writeTestArtifact(t, r.controller.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
	})

	processQueuedRestorePod(t, r.controller, r.pod)

	condition := appliedRestoredCondition(t, r.controller)
	assert.Equal(t, podcontract.RestoreReasonInputsChanged, condition.Reason)
	assert.Contains(t, condition.Message, "the snapshot recorded no inputs")
	assert.Equal(t, []string{"main"}, r.volume.kills())
	assert.NotContains(t, r.published(t), restoreSecret)
}

// A request the agent cannot read in full, or one naming a container that is
// not a destination, ends the restore in preflight: nothing is restored,
// every destination is ended, and the outcome is RestoreInputsChanged.
func TestAnUnreadableInputsRequestEndsTheRestore(t *testing.T) {
	for name, tc := range map[string]struct {
		annotation string
		wantError  string
	}{
		"not JSON":          {annotation: `main=DB_PASSWORD`, wantError: "not a JSON object"},
		"an unknown input":  {annotation: `{"main": {"files": ["/etc/app"]}}`, wantError: `key "files": is not a known input`},
		"asks for nothing":  {annotation: `{"main": {}}`, wantError: "names no input to compare"},
		"not a destination": {annotation: `{"router": {"env": ["A"]}}`, wantError: `"router", which is not a restore destination`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newInputsRestore(t, tc.annotation, []string{"A=1"}, []string{"A=1"})
			r.controller.restoreFn = noRestoreExpected(t)

			processQueuedRestorePod(t, r.controller, r.pod)

			condition := appliedRestoredCondition(t, r.controller)
			assert.Equal(t, podcontract.RestoreReasonInputsChanged, condition.Reason)
			assert.Contains(t, condition.Message, tc.wantError)
			assert.Contains(t, condition.Message, podcontract.RestoreMatchInputsAnnotation)
			assert.Equal(t, []string{"main"}, r.volume.kills())
			assert.True(t, r.volume.holds("main", podcontract.RestoreFailedFile))
		})
	}
}

// requestRecorder records what each destination's restore was asked to
// check, then stops it.
type requestRecorder struct {
	mu     sync.Mutex
	inputs map[string]*podcontract.RestoreInputs
	skips  map[string]bool
}

func (r *requestRecorder) restore(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs[req.DestinationContainerName] = req.Inputs
	r.skips[req.DestinationContainerName] = req.SkipCompatCheck
	return executor.RestoreResult{}, errors.New("test restore stopped")
}

// Each destination is asked to check what the annotation names for it, and a
// destination it does not name checks nothing.
func TestEachDestinationChecksItsOwnInputs(t *testing.T) {
	pod := multiRestorePod()
	pod.Annotations[podcontract.RestoreMatchInputsAnnotation] = `{"engine-0": {"env": ["DB_PASSWORD"], "args": true}}`
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.compareFn = func(compat.Gate, compat.Environment, compat.Environment) []compat.Mismatch { return nil }
	w.runtime = &containerPIDRuntime{fakeRuntime: &fakeRuntime{}, pids: map[string]int{"engine-0-id": 100, "engine-1-id": 200}}
	newControlVolume(map[int]string{100: "engine-0", 200: "engine-1"}).install(w)
	writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
	})
	recorder := &requestRecorder{inputs: map[string]*podcontract.RestoreInputs{}, skips: map[string]bool{}}
	w.restoreFn = recorder.restore

	processQueuedRestorePod(t, w, pod)

	require.Contains(t, recorder.inputs, "engine-0")
	require.Contains(t, recorder.inputs, "engine-1")
	assert.Equal(t, &podcontract.RestoreInputs{Env: []string{"DB_PASSWORD"}, Args: true}, recorder.inputs["engine-0"])
	assert.Nil(t, recorder.inputs["engine-1"])
}

// A Pod without the annotation asks for no check: the restore request is the
// same as before the check existed.
func TestARestoreWithoutTheAnnotationChecksNothing(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.compareFn = func(compat.Gate, compat.Environment, compat.Environment) []compat.Mismatch { return nil }
	writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: gatedRestoreContainer},
	})
	recorder := &requestRecorder{inputs: map[string]*podcontract.RestoreInputs{}, skips: map[string]bool{}}
	w.restoreFn = recorder.restore

	processQueuedRestorePod(t, w, pod)

	require.Contains(t, recorder.inputs, "main")
	assert.Nil(t, recorder.inputs["main"])
}

func TestRestoreVerdictWithDestinationsRefusedForTheirInputs(t *testing.T) {
	changed := func(destination string) restoreResult {
		return restoreResult{destination: destination, state: restoreResultInputsChanged,
			reason: "the destination container was started with inputs that differ from the snapshot's: environment variables A"}
	}
	for name, tc := range map[string]struct {
		results     []restoreResult
		wantReason  string
		wantMessage string
	}{
		"alone": {
			results:    []restoreResult{changed("main")},
			wantReason: podcontract.RestoreReasonInputsChanged,
			wantMessage: "Refused restore; the destination container was not shown to be started with the snapshot's inputs: " +
				"the destination container was started with inputs that differ from the snapshot's: environment variables A",
		},
		"every destination": {
			results:    []restoreResult{changed("prefill"), changed("decode")},
			wantReason: podcontract.RestoreReasonInputsChanged,
			wantMessage: "Refused restore; the destination container was not shown to be started with the snapshot's inputs: " +
				"prefill: the destination container was started with inputs that differ from the snapshot's: environment variables A; " +
				"decode: the destination container was started with inputs that differ from the snapshot's: environment variables A",
		},
		"still in progress": {
			results:     []restoreResult{changed("prefill"), {destination: "decode", state: restoreResultPending}},
			wantReason:  podcontract.RestoreReasonInProgress,
			wantMessage: "Restore from PodSnapshot snap remains in progress: 0 succeeded, 0 failed, 1 refused for changed inputs, 1 pending (decode)",
		},
		"beside a success": {
			results:    []restoreResult{changed("prefill"), {destination: "decode", state: restoreResultSucceeded}},
			wantReason: podcontract.RestoreReasonPartiallySucceeded,
			wantMessage: "Restored 1 of 2 destination containers from PodSnapshot snap; not restored: " +
				"prefill: the destination container was started with inputs that differ from the snapshot's: environment variables A",
		},
		"beside an incompatible destination": {
			results: []restoreResult{changed("prefill"), {destination: "decode", state: restoreResultIncompatible,
				reason: "gpu-count: source 1, target 0"}},
			wantReason: podcontract.RestoreReasonIncompatible,
			wantMessage: "Refused restore; this node cannot run the checkpoint: decode: gpu-count: source 1, target 0; " +
				"1 destination container(s) refused because they were not started with the snapshot's inputs: " +
				"prefill: the destination container was started with inputs that differ from the snapshot's: environment variables A",
		},
		"beside a failure": {
			results:    []restoreResult{changed("prefill"), {destination: "decode", state: restoreResultFailed, reason: "CRIU restore failed: boom"}},
			wantReason: podcontract.RestoreReasonFailed,
			wantMessage: "Restore failed for 1 destination container(s) and refused 1 other destination(s) from PodSnapshot snap: " +
				"decode: CRIU restore failed: boom; " +
				"1 destination container(s) refused because they were not started with the snapshot's inputs: " +
				"prefill: the destination container was started with inputs that differ from the snapshot's: environment variables A",
		},
		"beside a workload that started without a restore": {
			results: []restoreResult{changed("prefill"), {destination: "decode", state: restoreResultDeclined,
				reason: "it started without a restore", declinedReason: podcontract.RestoreReasonEndedByWorkload}},
			wantReason: podcontract.RestoreReasonInputsChanged,
			wantMessage: "Refused restore; the destination container was not shown to be started with the snapshot's inputs: " +
				"prefill: the destination container was started with inputs that differ from the snapshot's: environment variables A; " +
				"1 destination container(s) started without a restore first: decode: it started without a restore",
		},
	} {
		t.Run(name, func(t *testing.T) {
			verdict := tallyRestoreResults(tc.results).verdict("snap")

			assert.Equal(t, tc.wantReason, verdict.reason)
			assert.Equal(t, tc.wantMessage, verdict.message)
		})
	}
}

// The content's status says which inputs the checkpoint recorded, so whoever
// builds restore Pods can tell whether asking to match them can succeed.
func TestCheckpointSourcePublishesTheRecordedInputs(t *testing.T) {
	base := func() *types.CheckpointManifest {
		return &types.CheckpointManifest{Artifact: types.ArtifactManifest{ContentUID: "uid", ContainerName: "main"}}
	}

	m := base()
	m.Inputs = types.RecordInputs(&specs.Spec{Process: &specs.Process{Env: []string{"A=" + checkpointSecret}}})
	source := checkpointSourceFromManifest(m)
	require.NotNil(t, source)
	require.NotNil(t, source.Pod)
	assert.Equal(t, []snapshotv1alpha1.CheckpointInput{snapshotv1alpha1.CheckpointInputEnv, snapshotv1alpha1.CheckpointInputArgs},
		source.Pod.RecordedInputs)
	assert.NotContains(t, fmt.Sprintf("%+v", *source.Pod), checkpointSecret)

	assert.Nil(t, checkpointSourceFromManifest(base()), "a manifest without a record publishes no inputs")

	unreadable := base()
	unreadable.Inputs = types.RecordInputs(&specs.Spec{Process: &specs.Process{}})
	unreadable.Inputs.Version = 2
	assert.Nil(t, checkpointSourceFromManifest(unreadable), "a record this agent cannot read publishes no inputs")
}
