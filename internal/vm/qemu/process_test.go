//go:build linux

package qemu

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// spawnFakeQEMU starts a real `sleep` process with argv[0] overridden to
// contain "qemu-system" and diskPath, for looksLikeOurQEMU to match against.
func spawnFakeQEMU(t *testing.T, diskPath string) (pid int, kill func()) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no `sleep` binary on PATH, skipping")
	}
	cmd := &exec.Cmd{
		Path: sleep,
		Args: []string{"qemu-system-x86_64-disk-" + diskPath, "5"},
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning fake qemu process: %v", err)
	}
	// Give the child a moment to finish its execve.
	time.Sleep(50 * time.Millisecond)
	return cmd.Process.Pid, func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
}

func TestLooksLikeOurQEMU(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.qcow2")
	pid, kill := spawnFakeQEMU(t, diskPath)
	defer kill()

	if !looksLikeOurQEMU(pid, diskPath) {
		t.Error("expected a process with qemu-system and the disk path in argv to match")
	}
	if looksLikeOurQEMU(pid, "/some/other/disk.qcow2") {
		t.Error("expected a mismatched disk path to not match")
	}
}

func TestLooksLikeOurQEMURejectsNonQEMUProcess(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no `sleep` binary on PATH, skipping")
	}
	cmd := exec.Command(sleep, "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawning process: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	if looksLikeOurQEMU(cmd.Process.Pid, "/whatever") {
		t.Error("a plain `sleep` process should never look like qemu-system")
	}
}

func TestLooksLikeOurQEMURejectsDeadPid(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.qcow2")
	pid, kill := spawnFakeQEMU(t, diskPath)
	kill()
	time.Sleep(50 * time.Millisecond) // give the kernel a moment to reap it

	if looksLikeOurQEMU(pid, diskPath) {
		t.Error("expected a dead pid to not match, even if it once looked like qemu-system")
	}
}

func TestProcessAlive(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.qcow2")
	pid, kill := spawnFakeQEMU(t, diskPath)

	if !processAlive(pid) {
		t.Error("expected the freshly spawned process to be alive")
	}
	kill()
	time.Sleep(50 * time.Millisecond)
	if processAlive(pid) {
		t.Error("expected the killed process to no longer be alive")
	}
}

func TestAttachRejectsStaleOrReusedPid(t *testing.T) {
	if _, err := Attach(Config{}, 0, "/no/such/disk"); err == nil {
		t.Error("expected Attach to reject a pid that doesn't look like our qemu process")
	}
}

func TestAttachSucceedsForARealMatchingProcess(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk.qcow2")
	pid, kill := spawnFakeQEMU(t, diskPath)
	defer kill()

	p, err := Attach(Config{}, pid, diskPath)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if p.Pid() != pid {
		t.Errorf("expected Pid() to return %d, got %d", pid, p.Pid())
	}

	select {
	case <-p.exited:
		t.Error("expected exited to not be closed while the process is still alive")
	default:
	}
}
