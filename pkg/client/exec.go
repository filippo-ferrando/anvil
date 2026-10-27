package client

import (
	"fmt"
	"os"
	"os/exec"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
)

// sshCmd builds the ssh invocation running command on target, with no stdio attached.
// An empty command opens an interactive login.
func sshCmd(target SSHTarget, identity string, command []string) (*exec.Cmd, error) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("ssh: not found on PATH")
	}
	args, err := SSHArgs(target, "-p", identity)
	if err != nil {
		return nil, err
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	args = append(args, command...)
	return exec.Command(sshBin, args...), nil
}

// SSHCommand is the ssh invocation running command on target with the current stdio.
// An empty command opens an interactive login.
func SSHCommand(target SSHTarget, identity string, command []string) (*exec.Cmd, error) {
	cmd, err := sshCmd(target, identity, command)
	if err != nil {
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

// ContainerBin returns the path of the docker or podman CLI for engine.
func ContainerBin(engine anvilv1.ContainerEngine) (string, error) {
	name := format.Engine(engine)
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s: not found on PATH (needed for `anvil shell`/`anvil exec` against a %s container)", name, name)
	}
	return bin, nil
}

// ContainerExecCommand is a docker/podman exec of command in inst's running container,
// with the current stdio. An empty command opens /bin/sh; tty adds -t.
func (c *Client) ContainerExecCommand(inst *anvilv1.Instance, command []string, tty bool) (*exec.Cmd, error) {
	spec := inst.GetContainer()
	if spec == nil {
		return nil, fmt.Errorf("%q isn't a container", inst.GetName())
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return nil, fmt.Errorf("%q isn't running (state: %s)", inst.GetName(), format.State(inst.GetState()))
	}
	if spec.GetContainerId() == "" {
		return nil, fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}
	bin, err := ContainerBin(spec.GetEngine())
	if err != nil {
		return nil, err
	}
	args := []string{"exec", "-i"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, spec.GetContainerId())
	if len(command) == 0 {
		command = []string{"/bin/sh"}
	}
	cmd := exec.Command(bin, append(args, command...)...)
	cmd.Env = c.DockerEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}
