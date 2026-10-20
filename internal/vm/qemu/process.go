//go:build linux

package qemu

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Process controls one running or reattached qemu-system-* instance.
type Process struct {
	cfg  Config
	pid  int
	cmd  *exec.Cmd // nil when Attach'd rather than Spawn'd
	logF *os.File  // nil when Attach'd, we don't own its log file

	QMP *QMPClient

	// exited is closed exactly once the process is known to be gone.
	exited  chan struct{}
	waitErr error
}

// Spawn starts qemu-system-* for cfg, redirecting its stdout/stderr to
// logPath. It does not dial QMP itself; the caller should retry DialQMP.
func Spawn(ctx context.Context, cfg Config, logPath string) (*Process, error) {
	args, err := BuildArgs(cfg)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return nil, fmt.Errorf("qemu: creating log dir: %w", err)
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("qemu: opening log file: %w", err)
	}

	bin := BinaryName(cfg.Arch)
	cmd := exec.Command(bin, args...)
	cmd.Stdout = logF
	cmd.Stderr = logF
	// New process group so Stop's SIGKILL escalation can target the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		logF.Close()
		return nil, fmt.Errorf("qemu: starting %s: %w", bin, err)
	}

	p := &Process{cfg: cfg, pid: cmd.Process.Pid, cmd: cmd, logF: logF, exited: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Attach re-establishes control over a previously-spawned qemu-system-*
// process, verifying pid looks like a qemu process for diskPath.
func Attach(cfg Config, pid int, diskPath string) (*Process, error) {
	if !looksLikeOurQEMU(pid, diskPath) {
		return nil, fmt.Errorf("qemu: pid %d is not a qemu-system process for %s (stale or reused pid)", pid, diskPath)
	}
	p := &Process{cfg: cfg, pid: pid, exited: make(chan struct{})}
	go p.pollLiveness()
	return p, nil
}

func looksLikeOurQEMU(pid int, diskPath string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	// /proc's cmdline is NUL-separated argv, not space-separated.
	cmdline := string(bytes.ReplaceAll(data, []byte{0}, []byte{' '}))
	return strings.Contains(cmdline, "qemu-system") && strings.Contains(cmdline, diskPath)
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil && !isZombie(pid)
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

func (p *Process) pollLiveness() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if !processAlive(p.pid) {
			close(p.exited)
			return
		}
	}
}

// ExitedCleanly reports whether the exited process ended with status 0, as QEMU does after
// a guest poweroff or a QMP quit. A reattached process's status is unknown and counts as clean.
func (p *Process) ExitedCleanly() bool {
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return true
	}
	return p.cmd.ProcessState.Success()
}

// ExitStatus describes how the process ended, e.g. "exit status 1" or "signal: killed".
func (p *Process) ExitStatus() string {
	if p.cmd == nil || p.cmd.ProcessState == nil {
		return "unknown"
	}
	return p.cmd.ProcessState.String()
}

// Exited is closed once the process is gone.
func (p *Process) Exited() <-chan struct{} {
	return p.exited
}

// Pid returns the qemu-system-* process ID, or 0 if it isn't running.
func (p *Process) Pid() int {
	return p.pid
}

// AttachQMP dials the process's QMP socket, retrying briefly since the
// socket may not be accepting connections yet.
func (p *Process) AttachQMP(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := DialQMP(ctx, p.cfg.QMPSocket)
		if err == nil {
			p.QMP = client
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("qemu: QMP never became reachable at %s: %w", p.cfg.QMPSocket, lastErr)
}

// Stop escalates gracefully: QMP system_powerdown, then QMP quit, then
// SIGKILL. Pass timeout=0 for an immediate hard stop.
func (p *Process) Stop(ctx context.Context, timeout time.Duration) error {
	if p.QMP != nil && timeout > 0 {
		shutdownCtx, cancel := context.WithTimeout(ctx, timeout)
		if err := p.QMP.GracefulShutdown(shutdownCtx); err == nil {
			if p.waitExit(shutdownCtx) {
				cancel()
				return nil
			}
		}
		cancel()
	}

	if p.QMP != nil {
		quitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = p.QMP.Quit(quitCtx)
		exited := p.waitExit(quitCtx)
		cancel()
		if exited {
			return nil
		}
	}

	return p.kill()
}

func (p *Process) kill() error {
	if p.pid == 0 {
		return nil
	}
	// Negative pid targets the whole process group; a process that isn't a group leader
	// would otherwise never get the signal and this would wait forever.
	if err := syscall.Kill(-p.pid, syscall.SIGKILL); err != nil {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
	<-p.exited
	if p.cmd == nil {
		return nil
	}
	// A signaled process reports as a non-nil *exec.ExitError; dying of
	// dying of the SIGKILL sent above counts as a successful kill.
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return nil
	}
	return p.waitErr
}

// waitExit blocks until the process has exited or ctx is done, returning
// whether it exited in time. Safe to call multiple times/concurrently.
func (p *Process) waitExit(ctx context.Context) bool {
	select {
	case <-p.exited:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close releases the process's file handles without affecting whether
// qemu-system-* itself is still running.
func (p *Process) Close() error {
	if p.QMP != nil {
		_ = p.QMP.Close()
	}
	if p.logF != nil {
		return p.logF.Close()
	}
	return nil
}
