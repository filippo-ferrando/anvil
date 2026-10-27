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

// TestProcessAliveTreatsAZombieAsGone covers a killed process nothing has reaped yet.
func TestProcessAliveTreatsAZombieAsGone(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	defer func() { _ = cmd.Wait() }()
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for !isZombie(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("killed child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(cmd.Process.Pid) {
		t.Error("expected an unreaped zombie to count as gone")
	}
}

// TestKillReachesProcessOutsideItsOwnGroup covers a reattached QEMU that isn't a
// process group leader: signalling only the group would never reach it.
func TestKillReachesProcessOutsideItsOwnGroup(t *testing.T) {
	cmd := exec.Command("sleep", "60") // stays in the test's process group
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	p := &Process{pid: cmd.Process.Pid, exited: make(chan struct{})}
	go p.pollLiveness()
	done := make(chan error, 1)
	go func() { done <- p.kill() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("kill never reached a process outside its own group")
	}
}

// TestKillSkipsExitedProcess checks that a process already seen exiting is not signalled,
// since its pid may have been reused.
func TestKillSkipsExitedProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	exited := make(chan struct{})
	close(exited)
	p := &Process{pid: cmd.Process.Pid, exited: exited}
	if err := p.kill(); err != nil {
		t.Fatal(err)
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("kill signalled a pid whose process had already exited")
	}
}

// TestKillTimesOut checks that kill gives up when the exit is never observed.
func TestKillTimesOut(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	old := killTimeout
	killTimeout = 50 * time.Millisecond
	defer func() { killTimeout = old }()
	p := &Process{pid: cmd.Process.Pid, exited: make(chan struct{})}
	if err := p.kill(); err == nil {
		t.Fatal("kill returned nil although the exit was never seen")
	}
}

// AttachQMP gives up as soon as QEMU is gone instead of retrying for 5s.
func TestAttachQMPStopsWhenProcessExits(t *testing.T) {
	p := &Process{cfg: Config{QMPSocket: filepath.Join(t.TempDir(), "qmp.sock")}, exited: make(chan struct{})}
	close(p.exited)
	start := time.Now()
	if err := p.AttachQMP(t.Context()); err == nil {
		t.Fatal("AttachQMP succeeded without a socket")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("AttachQMP took %v after the process exited", d)
	}
}
