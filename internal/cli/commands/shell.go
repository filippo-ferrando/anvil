package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newShellCommand(flags *globalFlags) *cobra.Command {
	var user, identity string
	cmd := &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in a VM (over SSH) or container (via docker/podman exec)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			inst, err := resolveInstance(cmd.Context(), c, args[0])
			c.Close() // release the daemon connection before handing off the terminal
			if err != nil {
				return err
			}

			if inst.GetContainer() != nil {
				// Warn when VM-only flags are set for a container instance.
				if cmd.Flags().Changed("user") || cmd.Flags().Changed("identity") {
					fmt.Fprintln(os.Stderr, "anvil: --user/--identity have no effect on a container instance, ignoring")
				}
				return runContainerExec(inst, nil)
			}

			key, err := resolveIdentity(identity)
			if err != nil {
				return err
			}
			target, err := resolveSSHTarget(inst, user)
			if err != nil {
				return err
			}
			return runSSH(target, key, nil)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SSH user (VM only, defaults to the image's default user)")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key (VM only)")
	return cmd
}
