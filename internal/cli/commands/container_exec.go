package commands

import (
	"fmt"
	"os"
	"os/exec"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// isStdinTerminal reports whether stdin is an interactive terminal.
func isStdinTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runContainerCopy copies a file to or from inst's container via
// `docker cp`/`podman cp`; toContainer selects the copy direction.
func runContainerCopy(c *client.Client, inst *anvilv1.Instance, localPath, containerPath string, toContainer bool) error {
	spec := inst.GetContainer()
	if spec == nil {
		return fmt.Errorf("%q isn't a container", inst.GetName())
	}
	if spec.GetContainerId() == "" {
		return fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}

	bin, err := client.ContainerBin(spec.GetEngine())
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
	cmd.Env = c.DockerEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
