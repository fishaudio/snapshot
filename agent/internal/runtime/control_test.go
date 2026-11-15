// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeContainer lays out procRoot/<pid>/root/snapshot-control, as the agent
// sees a container through /host/proc, and returns procRoot, the PID and the
// control directory. The PID is arbitrary: nothing reads the real /proc.
func fakeContainer(t *testing.T) (procRoot string, pid int, controlDir string) {
	t.Helper()
	procRoot = t.TempDir()
	pid = 4242
	controlDir = filepath.Join(procRoot, "4242", "root", "snapshot-control")
	if err := os.MkdirAll(controlDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return procRoot, pid, controlDir
}

func readSentinel(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("sentinel not found: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("sentinel %s is %v, not a regular file", path, info.Mode())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestWriteControlSentinelCreatesAndOverwrites(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)

	for _, contents := range []string{"pid=1\n", "pid=2\n"} {
		if err := writeControlSentinel(procRoot, pid, "restore-complete", []byte(contents)); err != nil {
			t.Fatalf("writeControlSentinel: %v", err)
		}
		if got := readSentinel(t, filepath.Join(controlDir, "restore-complete")); got != contents {
			t.Errorf("sentinel = %q, want %q", got, contents)
		}
	}
	entries, err := os.ReadDir(controlDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("control dir has %d entries, want only the sentinel", len(entries))
	}
}

// Existing entries in the control directory are replaced, not written into.
func TestWriteControlSentinelReplacesExistingEntries(t *testing.T) {
	for name, entry := range map[string]string{
		"a temporary name": ".restore-complete.tmp",
		"the final name":   "restore-complete",
	} {
		t.Run(name, func(t *testing.T) {
			procRoot, pid, controlDir := fakeContainer(t)
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(controlDir, entry)); err != nil {
				t.Fatal(err)
			}

			if err := writeControlSentinel(procRoot, pid, "restore-complete", []byte("pid=42\n")); err != nil {
				t.Fatalf("writeControlSentinel: %v", err)
			}

			if got, _ := os.ReadFile(target); string(got) != "original\n" {
				t.Errorf("link target = %q, want it unchanged", got)
			}
			if got := readSentinel(t, filepath.Join(controlDir, "restore-complete")); got != "pid=42\n" {
				t.Errorf("sentinel = %q, want %q", got, "pid=42\n")
			}
		})
	}
}

// The control directory's path resolves inside the container's root.
func TestWriteControlSentinelResolvesInContainerRoot(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	root := filepath.Dir(controlDir)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	inRoot := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(inRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// An absolute link resolves against the container's root.
	if err := os.Symlink("/elsewhere", controlDir); err != nil {
		t.Fatal(err)
	}

	if err := writeControlSentinel(procRoot, pid, "restore-failed", []byte("x\n")); err != nil {
		t.Fatalf("writeControlSentinel: %v", err)
	}

	if got := readSentinel(t, filepath.Join(inRoot, "restore-failed")); got != "x\n" {
		t.Errorf("sentinel = %q, want %q", got, "x\n")
	}
}

func TestWriteControlSentinelErrors(t *testing.T) {
	procRoot, pid, _ := fakeContainer(t)
	for name, tc := range map[string]struct {
		pid  int
		name string
	}{
		"PID 0":              {pid: 0, name: "restore-complete"},
		"negative PID":       {pid: -1, name: "restore-complete"},
		"no such container":  {pid: pid + 1, name: "restore-complete"},
		"a path, not a name": {pid: pid, name: "../restore-complete"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := writeControlSentinel(procRoot, tc.pid, tc.name, nil); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestWriteControlSentinelWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	if err := writeControlSentinel(procRoot, pid, "restore-complete", nil); err == nil {
		t.Fatal("expected an error for a missing control mount")
	}
}

func TestControlSentinelExists(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	check := func(wantExists, wantErr bool) {
		t.Helper()
		exists, err := controlSentinelExists(procRoot, pid, "restore-complete")
		if (err != nil) != wantErr {
			t.Fatalf("err = %v, want error %v", err, wantErr)
		}
		if exists != wantExists {
			t.Fatalf("exists = %v, want %v", exists, wantExists)
		}
	}

	check(false, false) // nothing there

	path := filepath.Join(controlDir, "restore-complete")
	if err := os.Symlink(filepath.Join(t.TempDir(), "anything"), path); err != nil {
		t.Fatal(err)
	}
	check(false, true) // not a regular file

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	check(true, false)
}

func TestControlSentinelExistsWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	if _, err := controlSentinelExists(procRoot, pid, "restore-complete"); err == nil {
		t.Fatal("expected an error for a missing control mount")
	}
}

func TestClaimControlFile(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	path := filepath.Join(controlDir, "restore-claim")

	created, err := claimControlFile(procRoot, pid, "restore-claim", []byte("owner=agent\n"))
	if err != nil || !created {
		t.Fatalf("first claim: created %v, err %v", created, err)
	}
	if got := readSentinel(t, path); got != "owner=agent\n" {
		t.Fatalf("claim holds %q", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("claim mode %v, err %v; the workload must be able to read it", info.Mode(), err)
	}

	// A second claim, by the agent or by the workload, finds the first.
	created, err = claimControlFile(procRoot, pid, "restore-claim", []byte("owner=other\n"))
	if err != nil || created {
		t.Fatalf("second claim: created %v, err %v", created, err)
	}
	if _, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("the workload's exclusive create after the agent's: %v", err)
	}
	if got := readSentinel(t, path); got != "owner=agent\n" {
		t.Fatalf("a lost claim changed the claim to %q", got)
	}
	assertOnlyEntries(t, controlDir, "restore-claim")
}

// assertOnlyEntries checks that dir holds exactly names: a claim leaves no
// temporary file behind, won or lost.
func assertOnlyEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	if !slices.Equal(got, names) {
		t.Fatalf("%s holds %q, want %q", dir, got, names)
	}
}

// claimByLink claims path the way ClaimControlFile does, as a workload's
// command would: the content in a temporary file, then a hard link.
func claimByLink(path, contents string) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".workload-claim.*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(contents); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// The workload claims first: the agent's claim finds it and changes nothing.
func TestClaimControlFileAfterTheWorkload(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	path := filepath.Join(controlDir, "restore-claim")
	if err := os.WriteFile(path, []byte("owner=workload\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := claimControlFile(procRoot, pid, "restore-claim", []byte("owner=agent\n"))

	if err != nil || created {
		t.Fatalf("created %v, err %v", created, err)
	}
	if got := readSentinel(t, path); got != "owner=workload\n" {
		t.Fatalf("claim holds %q", got)
	}
	assertOnlyEntries(t, controlDir, "restore-claim")
}

// Whatever the workload put under the name is a claim the agent did not make,
// and is never followed.
func TestClaimControlFileNeverFollowsWhatIsThere(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, filepath.Join(controlDir, "restore-claim")); err != nil {
		t.Fatal(err)
	}

	created, err := claimControlFile(procRoot, pid, "restore-claim", []byte("owner=agent\n"))

	if err != nil || created {
		t.Fatalf("created %v, err %v", created, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the claim followed the symlink: %v", err)
	}
}

// The agent and the workload race for the claim many times over: each time
// exactly one of them creates it, and nobody ever reads it empty or cut short.
func TestClaimControlFileHasOneWinner(t *testing.T) {
	const agentClaim, workloadClaim = "owner=agent\ncontainer=abc123\n", "owner=workload\n"
	for range 200 {
		procRoot, pid, controlDir := fakeContainer(t)
		path := filepath.Join(controlDir, "restore-claim")
		var agentWon, workloadWon atomic.Int32
		var racers, readers sync.WaitGroup
		start, done := make(chan struct{}), make(chan struct{})
		var seen sync.Map
		for range 2 {
			readers.Go(func() {
				<-start
				for {
					select {
					case <-done:
						return
					default:
					}
					if data, err := os.ReadFile(path); err == nil {
						seen.Store(string(data), true)
					}
				}
			})
		}
		for range 4 {
			racers.Go(func() {
				<-start
				if created, err := claimControlFile(procRoot, pid, "restore-claim", []byte(agentClaim)); err == nil && created {
					agentWon.Add(1)
				}
			})
			racers.Go(func() {
				<-start
				if created, err := claimByLink(path, workloadClaim); err == nil && created {
					workloadWon.Add(1)
				}
			})
		}
		close(start)
		racers.Wait()
		close(done)
		readers.Wait()
		if winners := agentWon.Load() + workloadWon.Load(); winners != 1 {
			t.Fatalf("%d racers created the claim, want 1", winners)
		}
		seen.Range(func(data, _ any) bool {
			if data != agentClaim && data != workloadClaim {
				t.Fatalf("a reader saw the claim as %q", data)
			}
			return true
		})
		assertOnlyEntries(t, controlDir, "restore-claim")
	}
}

func TestClaimControlFileWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	created, err := claimControlFile(procRoot, pid, "restore-claim", nil)
	if err == nil || created {
		t.Fatalf("created %v, err %v; want an error for a missing control mount", created, err)
	}
	if _, err := claimControlFile(procRoot, pid, "../restore-claim", nil); err == nil {
		t.Fatal("expected an error for a name outside the control directory")
	}
}

func TestReadControlFile(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	path := filepath.Join(controlDir, "restore-failed")

	data, exists, err := readControlFile(procRoot, pid, "restore-failed")
	if err != nil || exists || data != nil {
		t.Fatalf("nothing there: got %q, %v, %v", data, exists, err)
	}

	if err := os.WriteFile(path, []byte("owner=agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, exists, err = readControlFile(procRoot, pid, "restore-failed")
	if err != nil || !exists || string(data) != "owner=agent\n" {
		t.Fatalf("regular file: got %q, %v, %v", data, exists, err)
	}

	// The workload can write anything there; the agent reads a bounded prefix.
	if err := os.WriteFile(path, make([]byte, 3*maxControlFileBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	data, exists, err = readControlFile(procRoot, pid, "restore-failed")
	if err != nil || !exists || len(data) != maxControlFileBytes {
		t.Fatalf("large file: got %d bytes, %v, %v", len(data), exists, err)
	}
}

func TestReadControlFileRefusesWhatIsNotARegularFile(t *testing.T) {
	for name, plant := range map[string]func(path string) error{
		"symlink": func(path string) error {
			return os.Symlink(filepath.Join(os.TempDir(), "anything"), path)
		},
		"directory": func(path string) error { return os.Mkdir(path, 0o755) },
		// O_NONBLOCK keeps the open from waiting for a writer.
		"fifo": func(path string) error { return unix.Mkfifo(path, 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			procRoot, pid, controlDir := fakeContainer(t)
			if err := plant(filepath.Join(controlDir, "restore-claim")); err != nil {
				t.Fatal(err)
			}
			data, exists, err := readControlFile(procRoot, pid, "restore-claim")
			if err == nil || !exists || data != nil {
				t.Fatalf("got %q, %v, %v; want an error for something that exists", data, exists, err)
			}
		})
	}
}

func TestReadControlFileWithoutControlDir(t *testing.T) {
	procRoot, pid, controlDir := fakeContainer(t)
	if err := os.Remove(controlDir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readControlFile(procRoot, pid, "restore-failed"); err == nil {
		t.Fatal("expected an error for a missing control mount")
	}
	if _, _, err := readControlFile(procRoot, pid, "../restore-failed"); err == nil {
		t.Fatal("expected an error for a name outside the control directory")
	}
}

func TestRemoveControlSentinel_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveControlSentinel(dir, "restore-complete"); err != nil {
		t.Fatalf("missing sentinel should be already removed: %v", err)
	}
}

func TestRemoveControlSentinel_RemovesExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "restore-complete"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RemoveControlSentinel(dir, "restore-complete"); err != nil {
		t.Fatalf("RemoveControlSentinel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "restore-complete")); !os.IsNotExist(err) {
		t.Fatalf("sentinel should be gone, stat error: %v", err)
	}
}

func TestRemoveControlSentinel_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := RemoveControlSentinel(missing, "restore-complete"); err == nil {
		t.Fatal("expected error for missing control mount")
	}
}

func TestPodControlVolumeDir(t *testing.T) {
	dir, err := podControlVolumeDir(KubeletPodsDir, "0f6c2c1e-uid", "engine-0")
	if err != nil {
		t.Fatalf("podControlVolumeDir: %v", err)
	}
	want := "/host/proc/1/root/var/lib/kubelet/pods/0f6c2c1e-uid/volumes/kubernetes.io~empty-dir/snapshot-control"
	if dir != want {
		t.Errorf("podControlVolumeDir = %q, want %q", dir, want)
	}

	for _, tc := range []struct{ uid, container string }{
		{"", "main"},
		{"uid", ""},
		{"..", "main"},
		{"uid", "../other"},
		{"a/b", "main"},
	} {
		if _, err := podControlVolumeDir(KubeletPodsDir, tc.uid, tc.container); err == nil {
			t.Errorf("podControlVolumeDir(%q, %q) succeeded, want error", tc.uid, tc.container)
		}
	}
}

// podsDirWithControlVolume returns a stand-in for KubeletPodsDir holding the
// snapshot-control volume of Pod "uid", as kubelet sets it up.
func podsDirWithControlVolume(t *testing.T) (podsDir, volume string) {
	t.Helper()
	podsDir = t.TempDir()
	volume, err := podControlVolumeDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(volume, 0o755); err != nil {
		t.Fatal(err)
	}
	return podsDir, volume
}

func writeString(dir *os.File, name, contents string, perm os.FileMode) error {
	return WriteFileInDir(dir, name, perm, func(w io.Writer) error {
		_, err := io.WriteString(w, contents)
		return err
	})
}

func TestOpenPodControlDirCreatesContainerDir(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)

	dir, err := openPodControlDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatalf("openPodControlDir: %v", err)
	}
	defer dir.Close()
	if err := writeString(dir, "restore-failed", "failed\n", 0o644); err != nil {
		t.Fatalf("WriteFileInDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(volume, "main", "restore-failed"))
	if err != nil || string(data) != "failed\n" {
		t.Fatalf("restore-failed = %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(volume, "main"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries = %v, %v; want only restore-failed", entries, err)
	}
}

func TestOpenPodControlDirRequiresVolume(t *testing.T) {
	if _, err := openPodControlDir(t.TempDir(), "uid", "main"); err == nil {
		t.Fatal("expected an error when kubelet has not set up the volume")
	}
}

// The Pod can change everything inside its control volume. A symlink there
// must not lead a write outside it.
func TestOpenPodControlDirRefusesSymlinkedContainerDir(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(volume, "main")); err != nil {
		t.Fatal(err)
	}

	if dir, err := openPodControlDir(podsDir, "uid", "main"); err == nil {
		dir.Close()
		t.Fatal("expected an error for a symlinked container directory")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("wrote outside the volume: %v", entries)
	}
}

func TestWriteFileInDirReplacesSymlinkWithoutFollowing(t *testing.T) {
	podsDir, volume := podsDirWithControlVolume(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(volume, "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(volume, "main", "snapshot-restore-proxy")); err != nil {
		t.Fatal(err)
	}
	dir, err := openPodControlDir(podsDir, "uid", "main")
	if err != nil {
		t.Fatalf("openPodControlDir: %v", err)
	}
	defer dir.Close()

	if err := writeString(dir, "snapshot-restore-proxy", "proxy", 0o755); err != nil {
		t.Fatalf("WriteFileInDir: %v", err)
	}

	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("symlink target changed to %q", data)
	}
	info, err := os.Lstat(filepath.Join(volume, "main", "snapshot-restore-proxy"))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed file = %v, %v; want a regular 0755 file", info, err)
	}
}

func TestWriteFileInDirRejectsPaths(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for _, name := range []string{"", "..", "a/b", "../x"} {
		if err := writeString(dir, name, "x", 0o644); err == nil {
			t.Errorf("WriteFileInDir(%q) succeeded, want error", name)
		}
	}
}
