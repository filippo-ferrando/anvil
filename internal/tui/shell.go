package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
)

// shellDoneMsg reports the outcome of a suspended shell/exec session (see
// shellInto/execInto) back into the update loop, so the instances screen
// can show a status line instead of silently swallowing a failure.
type shellDoneMsg struct{ err error }

// ttyCompatSSHArgs are extra -o flags that keep a local terminal's own
// quirks from leaking into the guest session: SetEnv=TERM=... overrides
// whatever $TERM the local terminal itself would otherwise send (kitty's
// own "xterm-kitty" being the real case this fixes — its shell
// integration queries terminal capabilities in a way that produced
// literal escape-sequence garbage after a suspended shell/exec session
// resumed the TUI, once the reply arrived late). IgnoreUnknown paired
// with WarnWeakCrypto means an ssh client too old to recognize that
// specific keyword just skips it instead of refusing to start at all.
// Same small copy internal/cli/commands/ssh.go has, for the same reason
// as knownHostsPath: this package only reuses pkg/client, not
// internal/cli/commands.
var ttyCompatSSHArgs = []string{
	"-o", "SetEnv=TERM=xterm-256color",
	"-o", "IgnoreUnknown=WarnWeakCrypto",
	"-o", "WarnWeakCrypto=no-pq-kex",
}

// shellInto is the M8 checklist's "shell/SSH handoff": tea.ExecProcess is
// Bubble Tea's own supported way to suspend the program, hand the real
// terminal to an external process, and resume automatically when it
// exits — simpler and more robust than tview's manual Suspend/resume
// pairing, since the framework itself owns putting the terminal back the
// way it found it. user overrides the image's own default user
// (blank keeps that default) — VM only, same as `anvil shell --user`.
func (m model) shellInto(inst *anvilv1.Instance, user string) (tea.Model, tea.Cmd) {
	cmd, err := buildShellCommand(inst, user, nil)
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// execInto is `anvil exec`'s TUI equivalent: runs command inside inst (SSH
// for a VM, `docker`/`podman exec` for a container) via the same
// suspend/resume handoff as shellInto, instead of an interactive login.
func (m model) execInto(inst *anvilv1.Instance, user, command string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		m.setStatus("exec: a command is required", true)
		return m, nil
	}

	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = buildContainerExecCommand(inst, fields)
	} else {
		cmd, err = buildShellCommand(inst, user, fields)
	}
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// buildShellCommand builds the real `ssh` invocation for inst — an
// interactive login when command is nil/empty, a one-off remote command
// (`anvil exec`'s shape) otherwise. user overrides the image's own
// default user; blank keeps that default.
func buildShellCommand(inst *anvilv1.Instance, user string, command []string) (*exec.Cmd, error) {
	vm := inst.GetVm()
	if vm == nil {
		return nil, fmt.Errorf("%q isn't a VM", inst.GetName())
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return nil, fmt.Errorf("%q isn't running", inst.GetName())
	}

	if user == "" {
		user = vm.GetDefaultUser()
	}
	if user == "" {
		user = "root"
	}

	host, port := "localhost", int(vm.GetSshPort())
	if vm.GetNetworkMode() == "bridge" {
		ip, _, _ := strings.Cut(vm.GetStaticIp(), "/")
		if ip == "" {
			return nil, fmt.Errorf("%q has no known bridge address yet", inst.GetName())
		}
		host, port = ip, 22
	} else if port == 0 {
		return nil, fmt.Errorf("%q has no known SSH port yet", inst.GetName())
	}

	identity, err := sshkey.EnsureDefault()
	if err != nil {
		return nil, err
	}
	knownHosts, err := knownHostsPath(inst.GetId())
	if err != nil {
		return nil, err
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("ssh: not found on PATH")
	}

	args := []string{
		"-p", fmt.Sprintf("%d", port),
		"-i", identity,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
	}
	args = append(args, ttyCompatSSHArgs...)
	args = append(args, fmt.Sprintf("%s@%s", user, host))
	args = append(args, command...)

	cmd := exec.Command(sshBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// buildContainerExecCommand mirrors internal/cli/commands/
// container_exec.go's runContainerExec: `docker exec`/`podman exec`
// against inst's own engine, inheriting stdio. A separate, small copy of
// that logic — this package only reuses pkg/client, not
// internal/cli/commands, per the plan's TUI section.
func buildContainerExecCommand(inst *anvilv1.Instance, command []string) (*exec.Cmd, error) {
	spec := inst.GetContainer()
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return nil, fmt.Errorf("%q isn't running", inst.GetName())
	}
	if spec.GetContainerId() == "" {
		return nil, fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}

	name := "docker"
	if spec.GetEngine() == anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN {
		name = "podman"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("%s: not found on PATH", name)
	}

	args := append([]string{"exec", "-it", spec.GetContainerId()}, command...)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// knownHostsPath mirrors internal/cli/commands/ssh.go's sshKnownHostsPath:
// keyed by instance ID rather than "host:port", since a SLIRP
// host-forwarded port is ephemeral and gets reused across unrelated
// instances.
func knownHostsPath(instanceID string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding a cache directory: %w", err)
	}
	dir := filepath.Join(cacheDir, "anvil", "known_hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating known_hosts dir: %w", err)
	}
	return filepath.Join(dir, instanceID), nil
}
