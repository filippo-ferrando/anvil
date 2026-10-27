//go:build linux

package proc

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func startSleep(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", args...)
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	return cmd
}

func TestAlive(t *testing.T) {
	cmd := startSleep(t, "5")
	if !Alive(cmd.Process.Pid) {
		t.Error("expected the freshly spawned process to be alive")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if Alive(cmd.Process.Pid) {
		t.Error("expected the killed process to no longer be alive")
	}
}

// An unreaped zombie counts as gone.
func TestAliveTreatsAZombieAsGone(t *testing.T) {
	cmd := startSleep(t, "60")
	defer func() { _ = cmd.Wait() }()
	_ = cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for !isZombie(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("killed child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(cmd.Process.Pid) {
		t.Error("expected an unreaped zombie to count as gone")
	}
}

func TestArgs(t *testing.T) {
	cmd := startSleep(t, "5")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var args []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if args = Args(cmd.Process.Pid); len(args) == 2 && strings.HasSuffix(args[0], "sleep") {
			break
		}
	}
	if len(args) != 2 || args[1] != "5" {
		t.Fatalf("Args = %q, want [sleep 5]", args)
	}
	if Args(0x7fffffff) != nil {
		t.Error("expected nil args for a missing pid")
	}
}

func TestTrackReportsExitStatus(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	h := Track(cmd)
	if !h.WaitExit(t.Context()) {
		t.Fatal("WaitExit returned false without a deadline")
	}
	if h.ExitedCleanly() || h.ExitStatus() != "exit status 3" {
		t.Fatalf("ExitedCleanly=%v ExitStatus=%q", h.ExitedCleanly(), h.ExitStatus())
	}
}

func TestWatchedExitStatusIsUnknown(t *testing.T) {
	h := &Handle{pid: 1, exited: make(chan struct{})}
	if !h.ExitedCleanly() || h.ExitStatus() != "unknown" {
		t.Fatalf("ExitedCleanly=%v ExitStatus=%q", h.ExitedCleanly(), h.ExitStatus())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if h.WaitExit(ctx) {
		t.Fatal("WaitExit reported an exit that never happened")
	}
}

func TestKillSpawned(t *testing.T) {
	cmd := startSleep(t, "60")
	h := Track(cmd)
	if err := h.Kill("test"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if Alive(h.Pid()) {
		t.Fatal("process still alive after Kill")
	}
}

// A reattached process that isn't a group leader only gets the signal through its pid.
func TestKillReachesProcessOutsideItsOwnGroup(t *testing.T) {
	cmd := startSleep(t, "60") // stays in the test's process group
	go func() { _ = cmd.Wait() }()
	h := Watch(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- h.Kill("test") }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("kill never reached a process outside its own group")
	}
}

// A process already seen exiting is not signalled, since its pid may have been reused.
func TestKillSkipsExitedProcess(t *testing.T) {
	cmd := startSleep(t, "30")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	exited := make(chan struct{})
	close(exited)
	h := &Handle{pid: cmd.Process.Pid, exited: exited}
	if err := h.Kill("test"); err != nil {
		t.Fatal(err)
	}
	if !Alive(cmd.Process.Pid) {
		t.Fatal("kill signalled a pid whose process had already exited")
	}
}

// Kill gives up when the exit is never observed.
func TestKillTimesOut(t *testing.T) {
	cmd := startSleep(t, "30")
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	old := killTimeout
	killTimeout = 50 * time.Millisecond
	defer func() { killTimeout = old }()
	h := &Handle{pid: cmd.Process.Pid, exited: make(chan struct{})}
	if err := h.Kill("test"); err == nil || !strings.HasPrefix(err.Error(), "test: ") {
		t.Fatalf("Kill = %v, want a timeout error prefixed with the name", err)
	}
}
