//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Binary is the firecracker executable, looked up on PATH.
const Binary = "firecracker"

// Paths are the files one Firecracker process uses, as host paths.
type Paths struct {
	APISocket  string // REST API
	ConfigFile string // --config-file, written by the caller
	ConsoleLog string // the guest's serial console (Firecracker's stdout)
	LogFile    string // Firecracker's own log and stderr

	// Jail, when set, runs firecracker under the jailer. Every path but ConsoleLog must
	// then be inside Jail.Root(), and firecracker sees them relative to it (InVMM).
	Jail *Jail
}

// JailerBinary is the jailer executable, looked up on PATH.
const JailerBinary = "jailer"

// Jail is how the jailer confines one firecracker: a chroot under ChrootBase, a new mount
// namespace, and UID/GID instead of the caller's privileges.
type Jail struct {
	ID         string // also names the chroot
	ChrootBase string
	UID, GID   int
	Jailer     string // jailer binary, JailerBinary on PATH when empty
	ExecFile   string // firecracker binary, symlinks resolved; its name is part of the chroot path
}

// ResolveBinary finds firecracker on PATH with symlinks resolved, the path a Jail's ExecFile wants.
func ResolveBinary() (string, error) {
	path, err := exec.LookPath(Binary)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func (j *Jail) execName() string {
	if j.ExecFile == "" {
		return Binary
	}
	return filepath.Base(j.ExecFile)
}

// Root is the chroot the jailer creates for j: <base>/<firecracker file name>/<id>/root.
func (j *Jail) Root() string { return filepath.Join(j.ChrootBase, j.execName(), j.ID, "root") }

// InVMM returns host path as firecracker sees it: unchanged, or relative to the jail root.
func (p Paths) InVMM(path string) string {
	if p.Jail == nil {
		return path
	}
	return "/" + strings.TrimPrefix(strings.TrimPrefix(path, p.Jail.Root()), "/")
}

// command builds the firecracker invocation, wrapped in the jailer when p.Jail is set.
// With cgroup v2 and no --cgroup the jailer creates no cgroup, so it needs no cgroup rights.
func (p Paths) command() (string, []string, error) {
	args := []string{"--api-sock", p.InVMM(p.APISocket), "--config-file", p.InVMM(p.ConfigFile), "--log-path", p.InVMM(p.LogFile)}
	if p.Jail == nil {
		return Binary, args, nil
	}
	fc := p.Jail.ExecFile
	if fc == "" {
		var err error
		if fc, err = ResolveBinary(); err != nil {
			return "", nil, err
		}
	}
	jail := []string{
		"--id", p.Jail.ID, "--exec-file", fc,
		"--uid", strconv.Itoa(p.Jail.UID), "--gid", strconv.Itoa(p.Jail.GID),
		"--chroot-base-dir", p.Jail.ChrootBase, "--cgroup-version", "2", "--",
	}
	jailer := p.Jail.Jailer
	if jailer == "" {
		jailer = JailerBinary
	}
	return jailer, append(jail, args...), nil
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

	bin, args, err := paths.command()
	if err != nil {
		console.Close()
		logF.Close()
		return nil, fmt.Errorf("firecracker: %w", err)
	}
	// Without --daemonize the jailer execs firecracker in its own process, so the pid stays.
	cmd := exec.Command(bin, args...)
	cmd.Stdout = console
	cmd.Stderr = logF
	// New process group so a kill reaches anything firecracker started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		console.Close()
		logF.Close()
		return nil, fmt.Errorf("firecracker: starting %s: %w", bin, err)
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
	if !looksLikeOurs(pid, paths) {
		return nil, fmt.Errorf("firecracker: pid %d is not firecracker for %s (stale or reused pid)", pid, paths.APISocket)
	}
	p := &Process{paths: paths, pid: pid, API: NewClient(paths.APISocket), exited: make(chan struct{})}
	go p.pollLiveness()
	return p, nil
}

// looksLikeOurs checks pid is firecracker serving paths. Jailed ones all use the same
// in-jail socket path, so their --id tells them apart.
func looksLikeOurs(pid int, paths Paths) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(bytes.TrimRight(data, "\x00")), "\x00")
	if len(args) == 0 || !containsArg(args, paths.InVMM(paths.APISocket)) {
		return false
	}
	if name := filepath.Base(args[0]); name != Binary && (paths.Jail == nil || name != paths.Jail.execName()) {
		return false
	}
	return paths.Jail == nil || hasID(args, paths.Jail.ID)
}

// hasID reports whether args name the jail id, as "--id=<id>" or "--id <id>".
func hasID(args []string, id string) bool {
	for i, a := range args {
		if a == "--id="+id || (a == "--id" && i+1 < len(args) && args[i+1] == id) {
			return true
		}
	}
	return false
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
			// The log may be gone by the time anyone reads it, so its tail goes in the error.
			return fmt.Errorf("firecracker: exited during startup (%s): %s", p.ExitStatus(), logTail(p.paths.LogFile))
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker: API never became reachable at %s: %w", p.paths.APISocket, lastErr)
}

// logTail returns the last few lines of firecracker's log, joined on one line.
func logTail(path string) string {
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, "; ")
}

// IsDynamic reports whether the ELF binary at path needs a dynamic loader.
func IsDynamic(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
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
	select {
	case <-p.exited:
		// Already gone: its pid may belong to another process by now.
	default:
		// Negative pid targets the whole process group.
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			// An Attach'd process that nothing reaps stays a zombie and never looks gone.
			return fmt.Errorf("firecracker: pid %d still there 10s after SIGKILL", p.pid)
		}
	}
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
