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

// shellDoneMsg reports the outcome of a suspended shell session (see
// shellInto) back into the update loop, so the instances screen can show
// a status line instead of silently swallowing an SSH failure.
type shellDoneMsg struct{ err error }

// shellInto is the M8 checklist's "shell/SSH handoff": tea.ExecProcess is
// Bubble Tea's own supported way to suspend the program, hand the real
// terminal to an external process, and resume automatically when it
// exits — simpler and more robust than tview's manual Suspend/resume
// pairing, since the framework itself owns putting the terminal back the
// way it found it.
func (m model) shellInto(inst *anvilv1.Instance) (tea.Model, tea.Cmd) {
	cmd, err := buildShellCommand(inst)
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

func buildShellCommand(inst *anvilv1.Instance) (*exec.Cmd, error) {
	vm := inst.GetVm()
	if vm == nil {
		return nil, fmt.Errorf("only a VM has an SSH shell in the TUI — a container's shell isn't wired up here, use `anvil exec` from a real terminal")
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return nil, fmt.Errorf("%q isn't running", inst.GetName())
	}

	user := vm.GetDefaultUser()
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

	cmd := exec.Command(sshBin,
		"-p", fmt.Sprintf("%d", port),
		"-i", identity,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+knownHosts,
		fmt.Sprintf("%s@%s", user, host),
	)
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
