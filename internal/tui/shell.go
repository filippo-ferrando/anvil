package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// shellDoneMsg reports the outcome of a suspended shell/exec session back into the update loop.
type shellDoneMsg struct{ err error }

// shellInto suspends the TUI and opens an interactive shell in inst: ssh for a VM,
// where user overrides the image's default user, and docker/podman exec for a container.
func (m model) shellInto(inst *anvilv1.Instance, user string) (tea.Model, tea.Cmd) {
	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = buildContainerExecCommand(m.client, inst, []string{"/bin/sh"})
	} else {
		cmd, err = buildShellCommand(m.client, inst, user, nil)
	}
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// execInto runs command inside inst (SSH for a VM, docker/podman exec for a container).
func (m model) execInto(inst *anvilv1.Instance, user, command string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		m.setStatus("exec: a command is required", true)
		return m, nil
	}

	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = buildContainerExecCommand(m.client, inst, fields)
	} else {
		cmd, err = buildShellCommand(m.client, inst, user, fields)
	}
	if err != nil {
		m.setStatus(err.Error(), true)
		return m, nil
	}
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg { return shellDoneMsg{err: err} })
}

// buildShellCommand builds the ssh invocation for inst: an interactive
// login when command is empty, a one-off remote command otherwise.
func buildShellCommand(c *client.Client, inst *anvilv1.Instance, user string, command []string) (*exec.Cmd, error) {
	target, err := c.SSHTarget(inst, user)
	if err != nil {
		return nil, err
	}
	identity, err := sshkey.EnsureDefault()
	if err != nil {
		return nil, err
	}
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("ssh: not found on PATH")
	}
	args, err := client.SSHArgs(target, "-p", identity)
	if err != nil {
		return nil, err
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	args = append(args, command...)

	cmd := exec.Command(sshBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// buildContainerExecCommand builds a `docker exec`/`podman exec` invocation against inst's engine.
func buildContainerExecCommand(c *client.Client, inst *anvilv1.Instance, command []string) (*exec.Cmd, error) {
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
	cmd.Env = c.DockerEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}
