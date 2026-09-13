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

// Process wraps one qemu-system-* invocation, either one we spawned
// ourselves (Spawn) or one we're re-attaching to after a daemon restart
// (Attach, used by reconciliation — see internal/vm.Backend.Reconcile).
// internal/instance's per-instance supervisor goroutine is the sole owner
// of a Process value; nothing else should touch it concurrently.
type Process struct {
	cfg  Config
	pid  int
	cmd  *exec.Cmd // nil when Attach'd rather than Spawn'd — see kill()'s comment on why that matters
	logF *os.File  // nil when Attach'd, we don't own its log file

	QMP *QMPClient

	// exited is closed exactly once when we've determined the process is
	// gone: via cmd.Wait() for a Spawn'd process, via liveness polling for
	// an Attach'd one (see the field's use in each constructor).
	exited  chan struct{}
	waitErr error
}

// Spawn starts qemu-system-* for cfg, redirecting its stdout/stderr to
// logPath (QEMU's own diagnostics, not the guest console — see args.go's
// `-serial null`; guest console capture is a follow-up). It does not dial
// QMP itself: the QMP socket isn't guaranteed to be accepting connections
// the instant the process starts, so the caller should retry DialQMP with
// a short backoff (a handful of attempts over ~1s is normally enough).
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
	// New process group so Stop's SIGKILL escalation (below) can target the
	// whole group, not just the immediate qemu-system-* pid, in case it
	// ever forks helper processes. This also means the group ID always
	// equals the qemu process's own pid, which Attach's reconciliation
	// path relies on (see kill()).
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

// Attach re-establishes control over a qemu-system-* process a *previous*
// anvild process spawned, found during startup reconciliation (see
// internal/vm.Backend.Reconcile). It refuses to trust a bare pid: pids get
// reused, so it first checks /proc/<pid>/cmdline actually looks like a
// qemu-system process for diskPath before doing anything else.
//
// Unlike a Spawn'd process, this one isn't our child — the anvild that
// spawned it is gone, and only a process's real parent can reap it via
// wait(4). So exit detection here is polling-based (see pollLiveness), not
// cmd.Wait()-based.
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
	proc, err := os.FindProcess(pid) // always succeeds on Unix, doesn't itself check liveness
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
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

// Pid returns the qemu-system-* process ID, or 0 if it isn't running.
func (p *Process) Pid() int {
	return p.pid
}

// AttachQMP dials the process's QMP socket, retrying briefly since the
// socket may not be accepting connections the instant qemu-system-* starts
// (irrelevant for an Attach'd process, where it should already be up, but
// harmless to retry there too).
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

// Stop escalates gracefully: QMP system_powerdown, then (after timeout) QMP
// quit, then (after a further short grace period) SIGKILL to the process
// group. Callers needing an immediate hard stop should pass timeout=0.
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
	// Negative pid targets the whole process group. Safe in both Spawn and
	// Attach modes: every qemu process anvil starts uses Setpgid (see
	// Spawn), so the group ID always equals the process's own pid, whether
	// or not *this* anvild instance is the one that originally spawned it.
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	<-p.exited
	return p.waitErr
}

// waitExit blocks until the process has exited (detected by whichever
// mechanism Spawn or Attach set up) or ctx is done, returning whether it
// exited in time. Safe to call multiple times/concurrently, unlike
// cmd.Wait().
func (p *Process) waitExit(ctx context.Context) bool {
	select {
	case <-p.exited:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close releases the process's file handles (log file, QMP connection)
// without affecting whether qemu-system-* itself is still running — call
// this once you've confirmed the process has actually exited.
func (p *Process) Close() error {
	if p.QMP != nil {
		_ = p.QMP.Close()
	}
	if p.logF != nil {
		return p.logF.Close()
	}
	return nil
}
