package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// resolveIdentity returns explicit if set, otherwise anvil's own managed key,
// generating it on first use.
func resolveIdentity(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return sshkey.EnsureDefault()
}

// resolveInstance looks up name via the daemon's Info RPC.
func resolveInstance(ctx context.Context, c *client.Client, name string) (*anvilv1.Instance, error) {
	reply, err := c.Info(ctx, &anvilv1.InfoRequest{Names: []string{name}})
	if err != nil {
		return nil, err
	}
	instances := reply.GetInstances()
	if len(instances) == 0 {
		return nil, fmt.Errorf("no such instance %q", name)
	}
	return instances[0], nil
}

// runSSH execs the real `ssh` binary against target, inheriting the current
// process's stdio. An empty command opens an interactive shell; a non-empty one runs it directly.
func runSSH(target client.SSHTarget, identity string, command []string) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH (needed for `anvil shell`/`anvil exec`)")
	}
	args, err := client.SSHArgs(target, "-p", identity)
	if err != nil {
		return err
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	args = append(args, command...)

	cmd := exec.Command(sshBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
