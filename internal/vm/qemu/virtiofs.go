//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// virtiofsdCandidates lists where distros install virtiofsd, which is usually not on PATH.
var virtiofsdCandidates = []string{"/usr/lib/virtiofsd", "/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd"}

// VirtiofsdPath finds the virtiofsd binary, on PATH or in a distro's libexec dir.
func VirtiofsdPath() (string, error) {
	if p, err := exec.LookPath("virtiofsd"); err == nil {
		return p, nil
	}
	for _, p := range virtiofsdCandidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("qemu: virtiofsd not found on PATH or in %s (install the virtiofsd package)", strings.Join(virtiofsdCandidates, ", "))
}

// Virtiofsd is one running virtiofsd serving a host directory to one VM.
// It exits by itself once QEMU disconnects from its socket.
type Virtiofsd struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// virtiofsdStartTimeout bounds how long StartVirtiofsd waits for the socket to appear.
const virtiofsdStartTimeout = 5 * time.Second

// StartVirtiofsd serves sharedDir on socketPath. The namespace sandbox is tried
// first; it needs user namespaces, so "none" is the fallback when those are blocked.
func StartVirtiofsd(sharedDir, socketPath string, readOnly bool, logPath string) (*Virtiofsd, error) {
	bin, err := VirtiofsdPath()
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, sandbox := range []string{"namespace", "none"} {
		v, err := startVirtiofsd(bin, sharedDir, socketPath, readOnly, sandbox, logPath)
		if err == nil {
			return v, nil
		}
		errs = append(errs, fmt.Sprintf("sandbox=%s: %v", sandbox, err))
	}
	return nil, fmt.Errorf("qemu: starting virtiofsd for %s: %s", sharedDir, strings.Join(errs, "; "))
}

func startVirtiofsd(bin, sharedDir, socketPath string, readOnly bool, sandbox, logPath string) (*Virtiofsd, error) {
	_ = os.Remove(socketPath)
	args := []string{
		"--socket-path=" + socketPath,
		"--shared-dir=" + sharedDir,
		"--sandbox=" + sandbox,
		"--cache=auto",
		"--announce-submounts",
	}
	if readOnly {
		args = append(args, "--readonly")
	}
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	defer logF.Close() // the child keeps its own copy of the descriptor

	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logF, logF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	v := &Virtiofsd{cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(v.exited)
	}()

	deadline := time.After(virtiofsdStartTimeout)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return v, nil
		}
		select {
		case <-v.exited:
			return nil, fmt.Errorf("virtiofsd exited early (see %s)", logPath)
		case <-deadline:
			v.Kill()
			return nil, fmt.Errorf("socket %s never appeared", socketPath)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Kill stops v if it is still running and waits for it to be gone.
func (v *Virtiofsd) Kill() {
	select {
	case <-v.exited:
		return
	default:
	}
	_ = syscall.Kill(-v.cmd.Process.Pid, syscall.SIGKILL)
	<-v.exited
}

// Exited is closed once v is gone.
func (v *Virtiofsd) Exited() <-chan struct{} { return v.exited }

// WaitExit waits up to timeout for v to exit by itself.
func (v *Virtiofsd) WaitExit(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-v.exited:
		return true
	case <-time.After(timeout):
		return false
	case <-ctx.Done():
		return false
	}
}
