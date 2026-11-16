// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build kamin_claim_race && linux

package controller

// The restore claim end to end, opt in (build tag kamin_claim_race): this
// agent's restore path against a real workload entrypoint, the Kamin
// entrypoint (kamin-entrypoint run, restore mode), which claims its container
// before it starts the workload without a restore. KAMIN_ENTRYPOINT_BIN names
// the binary.
//
// Each test is one destination container of a restore Pod. The entrypoint
// runs as the container's command, with the control directory of the Pod's
// emptyDir; the agent reaches that directory as on a node, through
// /proc/<pid>/root of the container's process (here a directory standing in
// for /host/proc), with its own claim, read, sentinel and kill operations. A
// container restart starts the entrypoint again on the same directory, with a
// new process and runtime ID. Only the restore itself is simulated: it enters
// the container, clears the outcome sentinels as nsrestore does, and starts a
// process that stands in for the restored workload; a restore into a container
// that has ended fails, and the restored process ends with its container, as
// it does with the container's PID namespace. The workload the entrypoint cold
// starts records which container started it.
//
// After every test, no container both had a restore enter it and cold started
// its workload (two engines on one GPU), and the agent killed only containers
// it had claimed.
//
// Run as root, the entrypoint and the workload run as user 65532 and the
// control directory is world-writable, as with an emptyDir and a non-root
// workload. KAMIN_CLAIM_RACE_DIR puts the Pods' directories on a given
// filesystem, such as an emptyDir on a GPU node. KAMIN-PATCHES.md ("Joint
// race test with the entrypoint") says how to run it, on a GPU node too.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	raceEntrypointEnv = "KAMIN_ENTRYPOINT_BIN"
	raceDirEnv        = "KAMIN_CLAIM_RACE_DIR"
	// The helper processes: the workload and the restored process.
	raceRoleEnv  = "KAMIN_CLAIM_RACE_ROLE"
	raceMarksEnv = "KAMIN_CLAIM_RACE_MARKS"
	// The workload's user when the test runs as root.
	raceWorkloadUID = 65532
	raceWait        = 10 * time.Second
)

// TestClaimRaceHelper is not a test: it is the workload the entrypoint cold
// starts, which records the entrypoint that started it, and the restored
// process. Both run until they are stopped or their parent goes: a process
// does not outlive its container.
func TestClaimRaceHelper(t *testing.T) {
	role := os.Getenv(raceRoleEnv)
	if role == "" {
		t.Skip("the workload and restored process of the claim race tests")
	}
	parent := os.Getppid()
	if role == "workload" {
		mark := filepath.Join(os.Getenv(raceMarksEnv), "cold-"+strconv.Itoa(parent))
		if err := os.WriteFile(mark, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	deadline := time.After(2 * time.Minute)
	for os.Getppid() == parent {
		select {
		case <-stop:
			os.Exit(0)
		case <-deadline:
			os.Exit(0)
		case <-time.After(20 * time.Millisecond):
		}
	}
	os.Exit(0)
}

// raceContainer is one start of the destination container.
type raceContainer struct {
	id      string
	pid     int
	cmd     *exec.Cmd
	log     string
	done    chan struct{}
	code    int
	mu      sync.Mutex
	others  []*exec.Cmd // processes restored into it
	waiting time.Time   // when its entrypoint was seen waiting for the restore
}

// state is "running" or the exit code, for messages.
func (c *raceContainer) state() string {
	if c.ended() {
		return "exit " + strconv.Itoa(c.code)
	}
	return "running"
}

func (c *raceContainer) ended() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// exit waits for the entrypoint to exit and returns its code.
func (c *raceContainer) exit(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case <-c.done:
		return c.code
	case <-time.After(within):
		t.Fatalf("%s did not exit within %v", c.id, within)
		return 0
	}
}

// racePod is a restore Pod with one destination container, its control
// directory, the agent of its node and what the test saw.
type racePod struct {
	t          *testing.T
	h          *claimHarness
	bin        string
	helper     string
	dir        string
	procRoot   string
	rootfs     string
	control    string
	marks      string
	asWorkload bool
	mu         sync.Mutex
	pod        *corev1.Pod
	containers []*raceContainer
	// Containers the agent's restore entered, and those it restored into.
	enteredIDs  map[string]bool
	restoredIDs map[string]bool
	strayKills  []string
	// hold, when set, keeps the restore staging until it is closed.
	hold chan struct{}
	// claimAt, when set, delays the agent's claim until then.
	claimAt time.Time
}

func newRacePod(t *testing.T) *racePod {
	t.Helper()
	return newRacePodWith(t, newClaimHarness(t))
}

// newRacePodWith is newRacePod with the agent and Pod of h.
func newRacePodWith(t *testing.T, h *claimHarness) *racePod {
	t.Helper()
	bin := os.Getenv(raceEntrypointEnv)
	require.NotEmpty(t, bin, "%s must name the kamin-entrypoint binary", raceEntrypointEnv)
	dir, err := os.MkdirTemp(os.Getenv(raceDirEnv), "kamin-claim-race-")
	require.NoError(t, err)
	p := &racePod{
		t:           t,
		h:           h,
		dir:         dir,
		procRoot:    filepath.Join(dir, "proc"),
		rootfs:      filepath.Join(dir, "rootfs"),
		marks:       filepath.Join(dir, "marks"),
		asWorkload:  os.Geteuid() == 0,
		enteredIDs:  map[string]bool{},
		restoredIDs: map[string]bool{},
	}
	p.control = filepath.Join(p.rootfs, strings.TrimPrefix(podcontract.SnapshotControlMountPath, "/"))
	p.pod = p.h.pod
	for _, d := range []string{p.procRoot, p.control, p.marks, filepath.Join(dir, "bin")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	// kubelet makes an emptyDir world-writable; the workload may run as
	// another user than the agent.
	for _, d := range []string{dir, p.rootfs} {
		require.NoError(t, os.Chmod(d, 0o755))
	}
	for _, d := range []string{p.control, p.marks} {
		require.NoError(t, os.Chmod(d, 0o777))
	}
	// Copies the workload's user can run.
	p.bin = p.copyExecutable(bin, "kamin-entrypoint")
	p.helper = p.copyExecutable(os.Args[0], "race-helper")
	p.installAgent()
	t.Cleanup(p.close)
	return p
}

func (p *racePod) copyExecutable(from, name string) string {
	data, err := os.ReadFile(from)
	require.NoError(p.t, err)
	to := filepath.Join(p.dir, "bin", name)
	require.NoError(p.t, os.WriteFile(to, data, 0o755))
	return to
}

// installAgent points the agent at the Pod: its claim, read, sentinel and
// kill operations reach the real control directory, through procRoot.
func (p *racePod) installAgent() {
	p.install(p.h.w)
}

// newAgent replaces the node's agent with a new one, as an agent restart
// does; the old one is left where it stopped.
func (p *racePod) newAgent() {
	p.h = newClaimHarness(p.t)
	p.install(p.h.w)
}

func (p *racePod) install(w *NodeController) {
	w.RestoreWithoutProxy()
	w.runtime = &raceRuntime{fakeRuntime: &fakeRuntime{}, pod: p}
	w.claimControlFileFn = func(pid int, name string, contents []byte) (bool, error) {
		p.mu.Lock()
		at := p.claimAt
		p.mu.Unlock()
		time.Sleep(time.Until(at))
		return snapshotruntime.ClaimControlFileUnder(p.procRoot, pid, name, contents)
	}
	w.readControlFileFn = func(pid int, name string) ([]byte, bool, error) {
		return snapshotruntime.ReadControlFileUnder(p.procRoot, pid, name)
	}
	w.controlSentinelExistsFn = func(pid int, name string) (bool, error) {
		return snapshotruntime.ControlSentinelExistsUnder(p.procRoot, pid, name)
	}
	w.writeControlSentinelFn = func(pid int, name string, contents []byte) error {
		return snapshotruntime.WriteControlSentinelUnder(p.procRoot, pid, name, contents)
	}
	// The node's path of the control volume is the same directory.
	w.writePodControlSentinelFn = func(_, _, name string, contents []byte) error {
		dir, err := os.Open(p.control)
		if err != nil {
			return err
		}
		defer dir.Close()
		return snapshotruntime.WriteFileInDir(dir, name, 0o644, func(out io.Writer) error {
			_, err := out.Write(contents)
			return err
		})
	}
	w.sendSignalFn = func(log logr.Logger, pid int, sig syscall.Signal, reason string) error {
		p.recordKill(pid)
		return snapshotruntime.SendSignalToPID(log, pid, sig, reason)
	}
	w.restoreFn = p.restore
}

// raceRuntime resolves a container to its entrypoint while it runs.
type raceRuntime struct {
	*fakeRuntime
	pod *racePod
}

func (r *raceRuntime) ResolveContainer(_ context.Context, id string) (int, *specs.Spec, error) {
	if c := r.pod.running(snapshotruntime.StripCRIScheme(id)); c != nil {
		return c.pid, &specs.Spec{}, nil
	}
	return 0, nil, fmt.Errorf("container %s is not running", id)
}

func (p *racePod) running(id string) *raceContainer {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.containers {
		if c.id == id && !c.ended() {
			return c
		}
	}
	return nil
}

func (p *racePod) credential(cmd *exec.Cmd) {
	if p.asWorkload {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: raceWorkloadUID, Gid: raceWorkloadUID},
		}
	}
}

// start starts the destination container (again): the entrypoint in restore
// mode, waiting up to restoreTimeout for the restore.
func (p *racePod) start(restoreTimeout time.Duration) *raceContainer {
	p.t.Helper()
	p.mu.Lock()
	n := len(p.containers) + 1
	p.mu.Unlock()
	c := &raceContainer{
		id:   "race-container-" + strconv.Itoa(n),
		done: make(chan struct{}),
	}
	c.log = filepath.Join(p.dir, c.id+".log")
	logFile, err := os.Create(c.log)
	require.NoError(p.t, err)
	c.cmd = exec.Command(p.bin, "run", "--", p.helper, "-test.run=^TestClaimRaceHelper$")
	c.cmd.Env = []string{
		"SNAPSHOT_RESTORE_STANDBY=1",
		"SNAPSHOT_CONTROL_DIR=" + p.control,
		"KAMIN_RESTORE_TIMEOUT=" + strconv.FormatInt(restoreTimeout.Milliseconds(), 10) + "ms",
		"KAMIN_POLL_INTERVAL=1ms",
		"KAMIN_STOP_TIMEOUT=2s",
		raceRoleEnv + "=workload",
		raceMarksEnv + "=" + p.marks,
	}
	c.cmd.Stdout = logFile
	c.cmd.Stderr = logFile
	p.credential(c.cmd)
	require.NoError(p.t, c.cmd.Start())
	c.pid = c.cmd.Process.Pid
	// Any process of the container reaches its control directory through
	// /proc/<pid>/root.
	procDir := filepath.Join(p.procRoot, strconv.Itoa(c.pid))
	require.NoError(p.t, os.MkdirAll(procDir, 0o755))
	require.NoError(p.t, os.Symlink(p.rootfs, filepath.Join(procDir, "root")))
	p.mu.Lock()
	p.containers = append(p.containers, c)
	status := &p.pod.Status.ContainerStatuses[0]
	status.ContainerID = "containerd://" + c.id
	status.RestartCount = int32(n - 1)
	status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now())}}
	p.mu.Unlock()
	go func() {
		_ = c.cmd.Wait()
		_ = logFile.Close()
		c.code = c.cmd.ProcessState.ExitCode()
		if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			c.code = 128 + int(ws.Signal())
		}
		// The container's processes, its workload and restored ones, end
		// with its PID namespace, and its /proc entry goes.
		_ = os.RemoveAll(procDir)
		if data, err := os.ReadFile(p.coldMark(c)); err == nil {
			if workload, err := strconv.Atoi(string(data)); err == nil {
				_ = syscall.Kill(workload, syscall.SIGKILL)
			}
		}
		c.mu.Lock()
		for _, other := range c.others {
			_ = other.Process.Kill()
		}
		c.mu.Unlock()
		close(c.done)
	}()
	return c
}

// waitWaiting waits until the first start of the container waits for the
// restore: it has written its restart marker.
func (p *racePod) waitWaiting(c *raceContainer) {
	p.t.Helper()
	p.waitFile(filepath.Join(p.control, ".kamin-restore-attempted"), c)
	c.waiting = time.Now()
}

// waitCold waits until c's entrypoint has started the workload.
func (p *racePod) waitCold(c *raceContainer) {
	p.t.Helper()
	p.waitFile(p.coldMark(c), c)
}

func (p *racePod) coldMark(c *raceContainer) string {
	return filepath.Join(p.marks, "cold-"+strconv.Itoa(c.pid))
}

func (p *racePod) cold(c *raceContainer) bool {
	_, err := os.Stat(p.coldMark(c))
	return err == nil
}

func (p *racePod) waitFile(path string, c *raceContainer) {
	p.t.Helper()
	deadline := time.Now().Add(raceWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if c.ended() {
			p.t.Fatalf("%s exited %d before %s appeared", c.id, c.code, filepath.Base(path))
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("%s did not appear", path)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// kill kills c's entrypoint, as an OOM kill or a failed probe does, and
// waits until the container has ended.
func (p *racePod) kill(c *raceContainer) {
	p.t.Helper()
	require.NoError(p.t, c.cmd.Process.Kill())
	c.exit(p.t, raceWait)
}

// endRestored ends the processes restored into c: the restored workload
// exits.
func (p *racePod) endRestored(c *raceContainer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, other := range c.others {
		_ = other.Process.Kill()
	}
}

// restore stands in for executor.Restore. The agent calls it after it
// claimed the container: entering the container is what must never meet a
// cold start in it.
func (p *racePod) restore(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
	id := snapshotruntime.StripCRIScheme(req.ContainerID)
	p.mu.Lock()
	p.enteredIDs[id] = true
	hold := p.hold
	p.mu.Unlock()
	c := p.running(id)
	if c == nil {
		return executor.RestoreResult{}, fmt.Errorf("destination container %s is not running", id)
	}
	// Staging the snapshot, however long it takes. Staging does not watch
	// the container: the restore finds out it ended only when it enters it.
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	if c.ended() {
		return executor.RestoreResult{}, fmt.Errorf("destination container %s ended during the restore", id)
	}
	// nsrestore, inside the container: the outcome of an earlier attempt goes,
	// the claim stays (executor.clearRestoreOutcome).
	for _, name := range []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile} {
		if err := snapshotruntime.RemoveControlSentinel(p.control, name); err != nil {
			return executor.RestoreResult{}, err
		}
	}
	restored := exec.Command(p.helper, "-test.run=^TestClaimRaceHelper$")
	restored.Env = []string{raceRoleEnv + "=restored"}
	p.credential(restored)
	if restored.SysProcAttr == nil {
		restored.SysProcAttr = &syscall.SysProcAttr{}
	}
	restored.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if err := restored.Start(); err != nil {
		return executor.RestoreResult{}, err
	}
	go func() { _ = restored.Wait() }()
	c.mu.Lock()
	c.others = append(c.others, restored)
	c.mu.Unlock()
	if c.ended() {
		_ = restored.Process.Kill()
		return executor.RestoreResult{}, fmt.Errorf("destination container %s ended during the restore", id)
	}
	p.mu.Lock()
	p.restoredIDs[id] = true
	p.mu.Unlock()
	return executor.RestoreResult{PlaceholderHostPID: c.pid, RestoredPID: restored.Process.Pid}, nil
}

func (p *racePod) entered(c *raceContainer) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enteredIDs[c.id]
}

func (p *racePod) restoredInto(c *raceContainer) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restoredIDs[c.id]
}

func (p *racePod) waitEntered(c *raceContainer) {
	p.t.Helper()
	deadline := time.Now().Add(raceWait)
	for !p.entered(c) {
		if time.Now().After(deadline) {
			p.t.Fatalf("the agent's restore did not enter %s", c.id)
		}
		time.Sleep(time.Millisecond)
	}
}

// recordKill notes a kill of a container the agent had not claimed.
func (p *racePod) recordKill(pid int) {
	p.mu.Lock()
	var target *raceContainer
	for _, c := range p.containers {
		if c.pid == pid {
			target = c
		}
	}
	stray := ""
	if target == nil {
		stray = fmt.Sprintf("pid %d of no container", pid)
	} else if claim := p.claim(); !claim.HeldByAgentFor(target.id) {
		stray = fmt.Sprintf("%s, claimed %+v", target.id, claim)
	}
	if stray != "" {
		p.strayKills = append(p.strayKills, stray)
	}
	p.mu.Unlock()
}

func (p *racePod) claim() podcontract.RestoreClaim {
	data, err := os.ReadFile(filepath.Join(p.control, podcontract.RestoreClaimFile))
	if err != nil {
		return podcontract.RestoreClaim{}
	}
	claim, _ := podcontract.ParseRestoreClaim(data)
	return claim
}

// agentPass runs one pass of the agent's restore queue over the Pod and
// returns the Restored condition it recorded, if any.
func (p *racePod) agentPass() *corev1.PodCondition {
	p.mu.Lock()
	pod := p.pod.DeepCopy()
	p.mu.Unlock()
	before := len(p.h.w.clientset.(*fake.Clientset).Actions())
	processQueuedRestorePod(p.t, p.h.w, pod)
	condition := p.conditionSince(before)
	if condition != nil {
		p.mu.Lock()
		setPodCondition(&p.pod.Status, *condition)
		p.mu.Unlock()
	}
	return condition
}

// conditionSince is the Restored condition of the last status apply after the
// first before actions, if any.
func (p *racePod) conditionSince(before int) *corev1.PodCondition {
	actions := p.h.w.clientset.(*fake.Clientset).Actions()
	for i := len(actions) - 1; i >= before; i-- {
		patch, ok := actions[i].(clientgotesting.PatchAction)
		if !ok || patch.GetSubresource() != "status" {
			continue
		}
		var applied struct {
			Status struct {
				Conditions []corev1.PodCondition `json:"conditions"`
			} `json:"status"`
		}
		require.NoError(p.t, json.Unmarshal(patch.GetPatch(), &applied))
		for _, condition := range applied.Status.Conditions {
			if condition.Type == corev1.PodConditionType(podcontract.RestoredCondition) {
				return &condition
			}
		}
	}
	return nil
}

func (p *racePod) agentPassAsync() <-chan *corev1.PodCondition {
	out := make(chan *corev1.PodCondition, 1)
	go func() { out <- p.agentPass() }()
	return out
}

// agentRestart forgets what this agent handled, as a restarted agent does,
// with the Pod's outcome not recorded: the restore looks in progress.
func (p *racePod) agentRestart() {
	p.h.w.handledRestores.Range(func(key, _ any) bool {
		p.h.w.handledRestores.Delete(key)
		return true
	})
	p.mu.Lock()
	setPodCondition(&p.pod.Status, corev1.PodCondition{
		Type:   corev1.PodConditionType(podcontract.RestoredCondition),
		Status: corev1.ConditionFalse,
		Reason: podcontract.RestoreReasonInProgress,
	})
	p.mu.Unlock()
}

// check holds the claim's promises over everything the test did.
func (p *racePod) check() {
	p.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.containers {
		if p.enteredIDs[c.id] && p.cold(c) {
			p.t.Errorf("two engines: the agent's restore entered %s, and its entrypoint started the workload", c.id)
		}
	}
	assert.Empty(p.t, p.strayKills, "the agent killed containers it had not claimed")
}

func (p *racePod) close() {
	p.mu.Lock()
	containers := slices.Clone(p.containers)
	p.mu.Unlock()
	for _, c := range containers {
		if !c.ended() {
			_ = c.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-c.done:
			case <-time.After(5 * time.Second):
				_ = c.cmd.Process.Kill()
				<-c.done
			}
		}
		p.endRestored(c)
		if p.t.Failed() {
			log, _ := os.ReadFile(c.log)
			p.t.Logf("%s (pid %d, exit %d):\n%s", c.id, c.pid, c.code, log)
		}
	}
	_ = os.RemoveAll(p.dir)
}

// assertRestoreReason asserts the Restored condition a pass recorded.
func assertRestoreReason(t *testing.T, condition *corev1.PodCondition, reasons ...string) {
	t.Helper()
	require.NotNil(t, condition, "the agent recorded no outcome")
	assert.Contains(t, reasons, condition.Reason, "outcome: %s", condition.Message)
}

// The agent comes first: it claims the container and restores into it; the
// entrypoint never starts the workload and follows the restored process.
func TestClaimRaceAgentFirst(t *testing.T) {
	p := newRacePod(t)
	c1 := p.start(5 * time.Second)
	p.waitWaiting(c1)

	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonSucceeded)
	assert.True(t, p.restoredInto(c1))
	assert.True(t, p.claim().HeldByAgentFor(c1.id), "claim: %+v", p.claim())

	// The restored workload exits. The stand-in is not the entrypoint's child,
	// so it follows it through /proc and exits 111; in a container the
	// restored process is PID 1's child and its status is the exit code.
	time.Sleep(50 * time.Millisecond)
	assert.False(t, c1.ended(), "the entrypoint ended (%s) after the restore", c1.state())
	p.endRestored(c1)
	assert.Equal(t, 111, c1.exit(t, raceWait))
	assert.False(t, p.cold(c1), "the entrypoint started the workload after the restore")
	p.check()
}

// The entrypoint comes first: its wait ran out, it claimed the container,
// wrote restore-failed and started the workload. The agent leaves the
// container alone.
func TestClaimRaceWorkloadFirst(t *testing.T) {
	p := newRacePod(t)
	c1 := p.start(100 * time.Millisecond)
	p.waitCold(c1)

	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonEndedByWorkload)
	assert.Equal(t, podcontract.ControlFileOwnerWorkload, p.claim().Owner)
	assert.False(t, p.entered(c1))
	assert.False(t, c1.ended(), "the workload's container ended (%s)", c1.state())
	p.check()
}

// Both claim at about the same moment: the agent's claim is made at offsets
// around the end of the entrypoint's wait, where the entrypoint makes its
// own, 250 µs apart. Exactly one of them gets the container, and each round
// has the outcome that goes with the winner.
func TestClaimRaceSimultaneous(t *testing.T) {
	const wait = 60 * time.Millisecond
	var agentWon, workloadWon int
	for offset := -10 * time.Millisecond; offset <= 10*time.Millisecond; offset += 250 * time.Microsecond {
		t.Run(fmt.Sprintf("offset %dus", offset.Microseconds()), func(t *testing.T) {
			p := newRacePod(t)
			c1 := p.start(wait)
			p.waitWaiting(c1)
			p.claimAt = c1.waiting.Add(wait + offset)
			condition := p.agentPass()

			switch claim := p.claim(); {
			case claim.HeldByAgentFor(c1.id):
				agentWon++
				assertRestoreReason(t, condition, podcontract.RestoreReasonSucceeded)
				assert.True(t, p.restoredInto(c1))
				time.Sleep(20 * time.Millisecond)
				p.endRestored(c1)
				assert.Equal(t, 111, c1.exit(t, raceWait))
			case claim.Owner == podcontract.ControlFileOwnerWorkload:
				workloadWon++
				assertRestoreReason(t, condition,
					podcontract.RestoreReasonClaimedByWorkload, podcontract.RestoreReasonEndedByWorkload)
				p.waitCold(c1)
				assert.False(t, p.entered(c1))
			default:
				t.Fatalf("claim after both tried: %+v", claim)
			}
			p.check()
		})
	}
	t.Logf("the agent won %d rounds, the workload %d", agentWon, workloadWon)
	assert.Positive(t, agentWon, "the agent never won: the race was not run")
	assert.Positive(t, workloadWon, "the workload never won: the race was not run")
}

// The entrypoint restarts: the container is killed while it waits, before
// anyone claimed it. The next start does not wait again: it claims the
// container and starts the workload. The agent, coming later, leaves it.
func TestClaimRaceRestartBeforeAnyClaim(t *testing.T) {
	p := newRacePod(t)
	c1 := p.start(time.Second)
	p.waitWaiting(c1)
	p.kill(c1)
	c2 := p.start(time.Second)
	p.waitCold(c2)

	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonEndedByWorkload)
	assert.False(t, p.entered(c1))
	assert.False(t, p.entered(c2))
	assert.False(t, c2.ended(), "the workload's container ended (%s)", c2.state())
	p.check()
}

// The entrypoint restarts while the agent restores into the container it
// claimed. The next start finds the agent's claim and does not start the
// workload; the restore into the ended container fails, the agent says so
// with restore-failed, and the start after that starts the workload.
func TestClaimRaceRestartDuringTheAgentsRestore(t *testing.T) {
	p := newRacePod(t)
	p.hold = make(chan struct{})
	c1 := p.start(500 * time.Millisecond)
	p.waitWaiting(c1)
	pass := p.agentPassAsync()
	p.waitEntered(c1)
	assert.True(t, p.claim().HeldByAgentFor(c1.id), "claim: %+v", p.claim())

	p.kill(c1)
	c2 := p.start(500 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.False(t, p.cold(c2), "the restarted entrypoint started the workload under the agent's claim")
	close(p.hold)
	assertRestoreReason(t, <-pass, podcontract.RestoreReasonFailed)
	assert.Equal(t, 112, c2.exit(t, raceWait), "the agent's restore-failed ends the wait")
	assert.False(t, p.cold(c2))

	c3 := p.start(500 * time.Millisecond)
	p.waitCold(c3)
	assert.False(t, p.entered(c2))
	assert.False(t, p.entered(c3))
	p.check()
}

// The restore succeeded, then the restored workload exited and the container
// restarted: it starts the workload, the Pod's one restore being spent. An
// agent that restarted before it recorded the outcome comes back to the Pod:
// its claim is for the earlier container, so it neither restores into nor
// kills the new one.
func TestClaimRaceRestartAfterARestoreWithAnAgentRestart(t *testing.T) {
	p := newRacePod(t)
	c1 := p.start(5 * time.Second)
	p.waitWaiting(c1)
	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonSucceeded)
	time.Sleep(20 * time.Millisecond)
	p.endRestored(c1)
	assert.Equal(t, 111, c1.exit(t, raceWait))

	c2 := p.start(5 * time.Second)
	p.waitCold(c2)
	p.agentRestart()
	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonFailed)
	assert.False(t, p.entered(c2))
	assert.False(t, c2.ended(), "the workload's container ended (%s)", c2.state())
	assert.True(t, p.claim().HeldByAgentFor(c1.id), "claim: %+v", p.claim())
	p.check()
}

// The agent claimed the container and its restore does not finish. The
// entrypoint waits one more restore timeout after its own, then writes
// restore-failed and exits 114 without starting the workload; the next start
// does. The restore, done at last, finds its container ended.
func TestClaimRaceAgentClaimsAndNeverFinishes(t *testing.T) {
	const wait = 300 * time.Millisecond
	p := newRacePod(t)
	p.hold = make(chan struct{})
	c1 := p.start(wait)
	p.waitWaiting(c1)
	pass := p.agentPassAsync()
	p.waitEntered(c1)

	assert.Equal(t, 114, c1.exit(t, raceWait))
	assert.GreaterOrEqual(t, time.Since(c1.waiting), 2*wait-20*time.Millisecond, "exit 114 before two restore timeouts")
	assert.False(t, p.cold(c1), "the entrypoint started the workload under the agent's claim")
	failed, err := os.ReadFile(filepath.Join(p.control, podcontract.RestoreFailedFile))
	require.NoError(t, err, "no restore-failed before exit 114")
	assert.False(t, podcontract.RestoreFailedByAgent(failed), "restore-failed is the entrypoint's: %q", failed)

	c2 := p.start(wait)
	p.waitCold(c2)
	close(p.hold)
	assertRestoreReason(t, <-pass, podcontract.RestoreReasonFailed)
	assert.False(t, p.entered(c2))
	assert.False(t, c2.ended(), "the workload's container ended (%s)", c2.state())
	p.check()
}

// The agent claimed the container and its restore finishes during the
// entrypoint's second wait: no exit 114, the restored process is followed.
func TestClaimRaceAgentFinishesDuringTheSecondWait(t *testing.T) {
	const wait = 400 * time.Millisecond
	p := newRacePod(t)
	p.hold = make(chan struct{})
	c1 := p.start(wait)
	p.waitWaiting(c1)
	pass := p.agentPassAsync()
	p.waitEntered(c1)
	time.Sleep(time.Until(c1.waiting.Add(wait + wait/2)))
	close(p.hold)

	assertRestoreReason(t, <-pass, podcontract.RestoreReasonSucceeded)
	time.Sleep(wait)
	assert.False(t, c1.ended(), "the entrypoint ended (%s) although the restore completed", c1.state())
	assert.False(t, p.cold(c1))
	p.endRestored(c1)
	assert.Equal(t, 111, c1.exit(t, raceWait))
	p.check()
}

// The agent stops (an agent restart) while it restores into the container
// it claimed. The new agent finds its claim for that same container and
// restores; the entrypoint, waiting under the claim, follows the restored
// process.
func TestClaimRaceAgentRestartsDuringItsRestore(t *testing.T) {
	const wait = time.Second
	p := newRacePod(t)
	p.hold = make(chan struct{}) // never closed: the first agent is gone
	c1 := p.start(wait)
	p.waitWaiting(c1)
	_ = p.agentPassAsync()
	p.waitEntered(c1)
	require.True(t, p.claim().HeldByAgentFor(c1.id), "claim: %+v", p.claim())

	// The first agent stays stopped inside its restore for the rest of the
	// test; only the new one goes on.
	p.mu.Lock()
	p.hold = nil
	p.mu.Unlock()
	p.newAgent()
	p.agentRestart()
	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonSucceeded)
	assert.True(t, p.restoredInto(c1))
	time.Sleep(time.Until(c1.waiting.Add(2*wait + 200*time.Millisecond)))
	assert.False(t, c1.ended(), "the entrypoint ended (%s) although the restore completed", c1.state())
	assert.False(t, p.cold(c1))
	p.endRestored(c1)
	assert.Equal(t, 111, c1.exit(t, raceWait))
	p.check()
}

// The agent ends a restore that will not run (its PodSnapshot never became
// ready) while the entrypoint waits: it claims the container, writes
// restore-failed and kills it; the next start starts the workload.
func TestClaimRaceAgentEndsARestoreThatWillNotRun(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod) // the PodSnapshot never appears
	p := newRacePodWith(t, &claimHarness{w: w, pod: pod})
	c1 := p.start(10 * time.Second)
	p.waitWaiting(c1)
	w.nowFn = func() time.Time { return time.Now().Add(w.config.Restore.SnapshotWait() + time.Second) }

	assertRestoreReason(t, p.agentPass(), podcontract.RestoreReasonSnapshotNotReady)
	assert.True(t, p.claim().HeldByAgentFor(c1.id), "claim: %+v", p.claim())
	assert.Contains(t, []int{112, 137}, c1.exit(t, raceWait), "restore-failed, then the kill")
	assert.False(t, p.cold(c1))

	c2 := p.start(10 * time.Second)
	p.waitCold(c2)
	assert.False(t, p.entered(c1))
	assert.False(t, p.entered(c2))
	p.check()
}
