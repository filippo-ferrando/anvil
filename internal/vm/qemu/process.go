//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/vm/proc"
)

// Process controls one running or reattached qemu-system-* instance.
type Process struct {
	*proc.Handle
	cfg  Config
	logF *os.File // nil when Attach'd, we don't own its log file

	QMP *QMPClient
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

	return &Process{Handle: proc.Track(cmd), cfg: cfg, logF: logF}, nil
}

// Attach re-establishes control over a previously-spawned qemu-system-*
// process, verifying pid looks like a qemu process for diskPath.
func Attach(cfg Config, pid int, diskPath string) (*Process, error) {
	if !looksLikeOurQEMU(pid, diskPath) {
		return nil, fmt.Errorf("qemu: pid %d is not a qemu-system process for %s (stale or reused pid)", pid, diskPath)
	}
	return &Process{Handle: proc.Watch(pid), cfg: cfg}, nil
}

func looksLikeOurQEMU(pid int, diskPath string) bool {
	cmdline := strings.Join(proc.Args(pid), " ")
	return strings.Contains(cmdline, "qemu-system") && strings.Contains(cmdline, diskPath)
}

// AttachQMP dials the process's QMP socket, retrying briefly since the
// socket may not be accepting connections yet. It gives up once QEMU exits.
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
		case <-p.Exited():
			return fmt.Errorf("qemu: exited (%s) before QMP was reachable at %s: %w", p.ExitStatus(), p.cfg.QMPSocket, lastErr)
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
			if p.WaitExit(shutdownCtx) {
				cancel()
				return nil
			}
		}
		cancel()
	}

	if p.QMP != nil {
		quitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = p.QMP.Quit(quitCtx)
		exited := p.WaitExit(quitCtx)
		cancel()
		if exited {
			return nil
		}
	}

	return p.Kill("qemu")
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
