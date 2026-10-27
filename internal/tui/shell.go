package tui

import (
	"errors"
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

// transferDoneMsg reports how a suspended scp or docker cp run went.
type transferDoneMsg struct {
	err  error
	what string // "src → dst", for the status line
}

const (
	transferFromField  = "From instance"
	transferLocalField = "Local path"
	transferInstField  = "Instance path"
	transferUserField  = "As user (optional)"
)

// transferFields builds the transfer form; user and identity only show for a VM.
func transferFields(inst *anvilv1.Instance, last instancesModel) []formField {
	fields := []formField{
		toggleField(transferFromField, "on: copy out of the instance, off: copy into it", last.lastTransferFrom),
		pathField(transferLocalField, "file or directory on this machine", ""),
		textField(transferInstField, "path inside "+inst.GetName(), ""),
	}
	if inst.GetVm() != nil {
		fields = append(fields, textField(transferUserField, "", last.lastShellUser), identityField(last.lastIdentity))
	}
	return fields
}

// transferFrom reads the transfer form into the copy it describes, the same as `anvil transfer`.
func transferFrom(form simpleForm, name string) (client.Transfer, error) {
	t := client.Transfer{
		Instance:     name,
		LocalPath:    form.Value(transferLocalField),
		InstancePath: form.Value(transferInstField),
		ToInstance:   !form.Bool(transferFromField),
	}
	if t.LocalPath == "" || t.InstancePath == "" {
		return t, errors.New("transfer: a local path and an instance path are required")
	}
	return t, nil
}

// transferWith suspends the TUI and copies t with scp (VM) or docker/podman cp (container).
func (m model) transferWith(inst *anvilv1.Instance, t client.Transfer, user, identity string) (tea.Model, tea.Cmd) {
	cmd, err := m.client.TransferCommand(inst, t, user, identity)
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	ref := inst.GetName() + ":" + t.InstancePath
	what := t.LocalPath + " → " + ref
	if !t.ToInstance {
		what = ref + " → " + t.LocalPath
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return transferDoneMsg{err: err, what: what} })
}
