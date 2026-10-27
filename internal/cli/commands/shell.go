package commands

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// sshFlags are the --user/--identity flags shared by shell and exec.
type sshFlags struct{ user, identity string }

func (f *sshFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.user, "user", "", "SSH user (VM only, defaults to the image's default user)")
	cmd.Flags().StringVarP(&f.identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key (VM only)")
}

// runInInstance runs command in name over SSH for a VM or docker/podman exec for a
// container. An empty command opens an interactive shell.
func runInInstance(cmd *cobra.Command, flags *globalFlags, f sshFlags, name string, command []string) error {
	c, err := dial(flags)
	if err != nil {
		return err
	}
	inst, err := resolveInstance(cmd.Context(), c, name)
	c.Close() // release the daemon connection before handing off the terminal
	if err != nil {
		return err
	}

	var run *exec.Cmd
	if inst.GetContainer() != nil {
		if cmd.Flags().Changed("user") || cmd.Flags().Changed("identity") {
			fmt.Fprintln(os.Stderr, "anvil: --user/--identity have no effect on a container instance, ignoring")
		}
		run, err = c.ContainerExecCommand(inst, command, isStdinTerminal())
	} else {
		var key string
		if key, err = resolveIdentity(f.identity); err != nil {
			return err
		}
		target, terr := c.SSHTarget(inst, f.user)
		if terr != nil {
			return terr
		}
		run, err = client.SSHCommand(target, key, command)
	}
	if err != nil {
		return err
	}
	return run.Run()
}

func newShellCommand(flags *globalFlags) *cobra.Command {
	var f sshFlags
	cmd := &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in a VM (over SSH) or container (via docker/podman exec)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInInstance(cmd, flags, f, args[0], nil)
		},
	}
	f.add(cmd)
	return cmd
}
