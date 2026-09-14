package commands

import (
	"fmt"
	"os"
	"os/exec"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// containerBinFor returns the docker/podman CLI binary path for the given
// container engine.
func containerBinFor(engine anvilv1.ContainerEngine) (string, error) {
	name := engineLabel(engine)
	bin, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s: not found on PATH (needed for `anvil shell`/`anvil exec` against a %s container)", name, name)
	}
	return bin, nil
}

// isStdinTerminal reports whether stdin is an interactive terminal.
func isStdinTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runContainerExec runs command inside inst's container, inheriting stdio.
// An empty command opens an interactive /bin/sh shell.
func runContainerExec(inst *anvilv1.Instance, command []string) error {
	spec := inst.GetContainer()
	if spec == nil {
		return fmt.Errorf("%q isn't a container", inst.GetName())
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return fmt.Errorf("%q isn't running (state: %s)", inst.GetName(), stateLabel(inst.GetState()))
	}
	if spec.GetContainerId() == "" {
		return fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}

	bin, err := containerBinFor(spec.GetEngine())
	if err != nil {
		return err
	}

	args := []string{"exec", "-i"}
	if isStdinTerminal() {
		args = append(args, "-t")
	}
	args = append(args, spec.GetContainerId())
	if len(command) > 0 {
		args = append(args, command...)
	} else {
		args = append(args, "/bin/sh")
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runContainerCopy copies a file to or from inst's container via
// `docker cp`/`podman cp`; toContainer selects the copy direction.
func runContainerCopy(inst *anvilv1.Instance, localPath, containerPath string, toContainer bool) error {
	spec := inst.GetContainer()
	if spec == nil {
		return fmt.Errorf("%q isn't a container", inst.GetName())
	}
	if spec.GetContainerId() == "" {
		return fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}

	bin, err := containerBinFor(spec.GetEngine())
	if err != nil {
		return err
	}

	remote := spec.GetContainerId() + ":" + containerPath
	var args []string
	if toContainer {
		args = []string{"cp", localPath, remote}
	} else {
		args = []string{"cp", remote, localPath}
	}

	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
