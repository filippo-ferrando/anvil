//go:build linux

package firecracker

import (
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

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/proc"
)

// Binary is the firecracker executable, looked up by LookPath.
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

// JailerBinary is the jailer executable, looked up by LookPath.
const JailerBinary = "jailer"

// Jail is how the jailer confines one firecracker: a chroot under ChrootBase, a new mount
// namespace, and UID/GID instead of the caller's privileges.
type Jail struct {
	ID         string // also names the chroot
	ChrootBase string
	UID, GID   int
	Jailer     string // jailer binary, LookPath(JailerBinary) when empty
	ExecFile   string // firecracker binary, symlinks resolved; its name is part of the chroot path
}

// LookPath finds name in config.FirecrackerDir, where the packages put a static build,
// then on PATH.
func LookPath(name string) (string, error) {
	if path, err := exec.LookPath(filepath.Join(config.FirecrackerDir(), name)); err == nil {
		return path, nil
	}
	return exec.LookPath(name)
}

// ResolveBinary finds firecracker with symlinks resolved, the path a Jail's ExecFile wants.
func ResolveBinary() (string, error) {
	path, err := LookPath(Binary)
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
		// Not resolved: Attach recognizes the process by the name it was started with.
		fc, err := LookPath(Binary)
		return fc, args, err
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
		var err error
		if jailer, err = LookPath(JailerBinary); err != nil {
			return "", nil, err
		}
	}
	return jailer, append(jail, args...), nil
}

// Process controls one running or reattached firecracker process.
type Process struct {
	*proc.Handle
	paths Paths
	files []*os.File // console and log, nil when Attach'd

	API *Client
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

	return &Process{
		Handle: proc.Track(cmd), paths: paths, files: []*os.File{console, logF},
		API: NewClient(paths.APISocket),
	}, nil
}

// Attach takes control of a firecracker process an earlier anvild spawned, after
// checking pid still is firecracker serving apiSocket.
func Attach(paths Paths, pid int) (*Process, error) {
	if !looksLikeOurs(pid, paths) {
		return nil, fmt.Errorf("firecracker: pid %d is not firecracker for %s (stale or reused pid)", pid, paths.APISocket)
	}
	return &Process{Handle: proc.Watch(pid), paths: paths, API: NewClient(paths.APISocket)}, nil
}

// looksLikeOurs checks pid is firecracker serving paths. Jailed ones all use the same
// in-jail socket path, so their --id tells them apart.
func looksLikeOurs(pid int, paths Paths) bool {
	args := proc.Args(pid)
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
		case <-p.Exited():
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

// Stop sends Ctrl+Alt+Del and waits up to timeout for the guest to go down, then kills
// the process. Pass timeout=0 for an immediate hard stop.
func (p *Process) Stop(ctx context.Context, timeout time.Duration) error {
	if timeout > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		err := p.API.SendCtrlAltDel(waitCtx)
		exited := err == nil && p.WaitExit(waitCtx)
		cancel()
		if exited {
			return nil
		}
	}
	return p.Kill("firecracker")
}

// Close releases the process's file handles without affecting whether firecracker
// itself is still running.
func (p *Process) Close() error {
	for _, f := range p.files {
		_ = f.Close()
	}
	return nil
}
