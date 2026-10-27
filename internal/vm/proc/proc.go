//go:build linux

// Package proc tracks the hypervisor processes anvild spawns or reattaches to.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Handle follows one process until it is gone.
type Handle struct {
	pid int
	cmd *exec.Cmd // nil when reattached rather than spawned

	// exited is closed exactly once the process is known to be gone.
	exited  chan struct{}
	waitErr error
}

// Track follows a started cmd, reaping it when it exits.
func Track(cmd *exec.Cmd) *Handle {
	h := &Handle{pid: cmd.Process.Pid, cmd: cmd, exited: make(chan struct{})}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.exited)
	}()
	return h
}

// Watch follows pid, a process this anvild did not start, by polling it.
func Watch(pid int) *Handle {
	h := &Handle{pid: pid, exited: make(chan struct{})}
	go h.pollLiveness()
	return h
}

// Pid returns the process ID.
func (h *Handle) Pid() int { return h.pid }

// Exited is closed once the process is gone.
func (h *Handle) Exited() <-chan struct{} { return h.exited }

// ExitedCleanly reports whether the exited process ended with status 0.
// A reattached process's status is unknown and counts as clean.
func (h *Handle) ExitedCleanly() bool {
	if h.cmd == nil || h.cmd.ProcessState == nil {
		return true
	}
	return h.cmd.ProcessState.Success()
}

// ExitStatus describes how the process ended, e.g. "exit status 1" or "signal: killed".
func (h *Handle) ExitStatus() string {
	if h.cmd == nil || h.cmd.ProcessState == nil {
		return "unknown"
	}
	return h.cmd.ProcessState.String()
}

// WaitExit blocks until the process has exited or ctx is done, and reports whether it exited.
func (h *Handle) WaitExit(ctx context.Context) bool {
	select {
	case <-h.exited:
		return true
	case <-ctx.Done():
		return false
	}
}

// killTimeout bounds the wait for a process to go after SIGKILL.
var killTimeout = 10 * time.Second

// Kill sends SIGKILL to the process group, or to the pid alone when that fails, and waits
// for the exit. name prefixes errors. A process already seen gone is not signalled.
func (h *Handle) Kill(name string) error {
	if h.pid == 0 {
		return nil
	}
	select {
	case <-h.exited:
		// Already gone: its pid may belong to another process by now.
	default:
		// A process that isn't a group leader only gets the signal through its own pid.
		if err := syscall.Kill(-h.pid, syscall.SIGKILL); err != nil {
			_ = syscall.Kill(h.pid, syscall.SIGKILL)
		}
		select {
		case <-h.exited:
		case <-time.After(killTimeout):
			// A reattached process that nothing reaps stays a zombie and never looks gone.
			return fmt.Errorf("%s: pid %d still there %s after SIGKILL", name, h.pid, killTimeout)
		}
	}
	if h.cmd == nil {
		return nil
	}
	// Dying of the SIGKILL sent above counts as a successful kill.
	if ws, ok := h.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return nil
	}
	return h.waitErr
}

func (h *Handle) pollLiveness() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !Alive(h.pid) {
			close(h.exited)
			return
		}
	}
}

// Alive reports whether pid runs and is not a zombie.
func Alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !isZombie(pid)
}

// isZombie reports whether pid has exited and only waits for its parent to reap it.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The state follows the command name, which is in parentheses and may hold spaces.
	i := bytes.LastIndexByte(data, ')')
	return i >= 0 && i+2 < len(data) && data[i+2] == 'Z'
}

// Args returns pid's argv from /proc, or nil when it can't be read.
func Args(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		return nil
	}
	return strings.Split(string(bytes.TrimRight(data, "\x00")), "\x00")
}
