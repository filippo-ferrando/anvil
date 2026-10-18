//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Binary is the firecracker executable, looked up on PATH.
const Binary = "firecracker"

// Paths are the files one Firecracker process uses.
type Paths struct {
	APISocket  string // REST API
	ConfigFile string // --config-file, written by the caller
	ConsoleLog string // the guest's serial console (Firecracker's stdout)
	LogFile    string // Firecracker's own log and stderr
}

// Process controls one running or reattached firecracker process.
type Process struct {
	paths Paths
	pid   int
	cmd   *exec.Cmd  // nil when Attach'd rather than Spawn'd
	files []*os.File // console and log, nil when Attach'd

	API *Client

	// exited is closed exactly once the process is known to be gone.
	exited  chan struct{}
	waitErr error
}

// Spawn starts firecracker, which boots the VM described by paths.ConfigFile right away.
func Spawn(ctx context.Context, paths Paths) (*Process, error) {
	if err := os.MkdirAll(filepath.Dir(paths.ConsoleLog), 0o750); err != nil {
		return nil, fmt.Errorf("firecracker: creating log dir: %w", err)
	}
	// A socket left by an earlier run makes firecracker refuse to start.
	if err := os.Remove(paths.APISocket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("firecracker: removing stale API socket: %w", err)
	}
	console, err := os.OpenFile(paths.ConsoleLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("firecracker: opening console log: %w", err)
	}
	logF, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		console.Close()
		return nil, fmt.Errorf("firecracker: opening log file: %w", err)
	}

	cmd := exec.Command(Binary,
		"--api-sock", paths.APISocket,
		"--config-file", paths.ConfigFile,
		"--log-path", paths.LogFile,
	)
	cmd.Stdout = console
	cmd.Stderr = logF
	// New process group so a kill reaches anything firecracker started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		console.Close()
		logF.Close()
		return nil, fmt.Errorf("firecracker: starting %s: %w", Binary, err)
	}

	p := &Process{
		paths: paths, pid: cmd.Process.Pid, cmd: cmd, files: []*os.File{console, logF},
		API: NewClient(paths.APISocket), exited: make(chan struct{}),
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// Attach takes control of a firecracker process an earlier anvild spawned, after
// checking pid still is firecracker serving apiSocket.
func Attach(paths Paths, pid int) (*Process, error) {
	if !looksLikeOurs(pid, paths.APISocket) {
		return nil, fmt.Errorf("firecracker: pid %d is not firecracker for %s (stale or reused pid)", pid, paths.APISocket)
	}
	p := &Process{paths: paths, pid: pid, API: NewClient(paths.APISocket), exited: make(chan struct{})}
	go p.pollLiveness()
	return p, nil
}

func looksLikeOurs(pid int, apiSocket string) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(bytes.TrimRight(data, "\x00")), "\x00")
	return len(args) > 0 && filepath.Base(args[0]) == Binary && containsArg(args, apiSocket)
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
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

// WaitAPI waits until the API socket answers, which also means the config was accepted.
func (p *Process) WaitAPI(ctx context.Context) error {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := p.API.Info(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.exited:
			return fmt.Errorf("firecracker: exited during startup (%s), see %s", p.ExitStatus(), p.paths.LogFile)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker: API never became reachable at %s: %w", p.paths.APISocket, lastErr)
}

// ExitedCleanly reports whether the process ended with status 0, as firecracker does
// after a guest reboot. A reattached process's status is unknown and counts as clean.
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
func (p *Process) Exited() <-chan struct{} { return p.exited }

func (p *Process) Pid() int { return p.pid }

// Stop sends Ctrl+Alt+Del and waits up to timeout for the guest to go down, then kills
// the process. Pass timeout=0 for an immediate hard stop.
func (p *Process) Stop(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		err := p.API.SendCtrlAltDel(waitCtx)
		exited := err == nil && p.waitExit(waitCtx)
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
	// Negative pid targets the whole process group.
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	<-p.exited
	if p.cmd == nil {
		return nil
	}
	// Dying of the SIGKILL sent above counts as a successful kill.
	if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return nil
	}
	return p.waitErr
}

func (p *Process) waitExit(ctx context.Context) bool {
	select {
	case <-p.exited:
		return true
	case <-ctx.Done():
		return false
	}
}

// Close releases the process's file handles without affecting whether firecracker
// itself is still running.
func (p *Process) Close() error {
	for _, f := range p.files {
		_ = f.Close()
	}
	return nil
}
