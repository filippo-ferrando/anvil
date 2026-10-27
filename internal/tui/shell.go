package tui

import (
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// shellDoneMsg reports the outcome of a suspended shell/exec session back into the update loop.
type shellDoneMsg struct{ err error }

// identityLabel is the shell/exec/gui forms' optional private key field.
const identityLabel = "Identity (optional)"

// identityField asks for a private key path to use instead of anvil's own key (VM only).
func identityField(value string) formField {
	return pathField(identityLabel, "private key on this machine, blank = anvil's own key", value)
}

// shellInto suspends the TUI and opens an interactive shell in inst: ssh for a VM,
// where user and identity override the defaults, and docker/podman exec for a container.
func (m model) shellInto(inst *anvilv1.Instance, user, identity string) (tea.Model, tea.Cmd) {
	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = m.client.ContainerExecCommand(inst, nil, true)
	} else {
		cmd, err = buildShellCommand(m.client, inst, user, identity, nil)
	}
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// execInto runs command inside inst (SSH for a VM, docker/podman exec for a container).
func (m model) execInto(inst *anvilv1.Instance, user, identity, command string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		m.setStatus("exec: a command is required", true)
		return m, nil
	}

	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = m.client.ContainerExecCommand(inst, fields, true)
	} else {
		cmd, err = buildShellCommand(m.client, inst, user, identity, fields)
	}
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// buildShellCommand builds the ssh invocation for inst: an interactive
// login when command is empty, a one-off remote command otherwise.
func buildShellCommand(c *client.Client, inst *anvilv1.Instance, user, identity string, command []string) (*exec.Cmd, error) {
	target, err := c.SSHTarget(inst, user)
	if err != nil {
		return nil, err
	}
	key, err := client.ResolveIdentity(identity)
	if err != nil {
		return nil, err
	}
	return client.SSHCommand(target, key, command)
}
