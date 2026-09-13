package commands

import (
	"github.com/spf13/cobra"
)

func newShellCommand(flags *globalFlags) *cobra.Command {
	var user, identity string
	cmd := &cobra.Command{
		Use:   "shell <name>",
		Short: "Open an interactive shell in a VM over SSH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := resolveIdentity(identity)
			if err != nil {
				return err
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			target, err := resolveSSHTarget(cmd.Context(), c, args[0], user)
			c.Close() // hand the terminal over to `ssh`, don't hold the daemon connection open for it
			if err != nil {
				return err
			}
			return runSSH(target, key, nil)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SSH user (defaults to the image's default user)")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key")
	return cmd
}
