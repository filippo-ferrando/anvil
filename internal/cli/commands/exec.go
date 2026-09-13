package commands

import (
	"github.com/spf13/cobra"
)

func newExecCommand(flags *globalFlags) *cobra.Command {
	var user, identity string
	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command inside a VM over SSH",
		Long: "Run a command inside a VM over SSH. Put `--` before the remote command if it " +
			"has its own flags, same as `ssh host -- cmd --flag`, otherwise cobra will try to " +
			"parse those flags as anvil's own.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			command := args[1:]

			key, err := resolveIdentity(identity)
			if err != nil {
				return err
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			target, err := resolveSSHTarget(cmd.Context(), c, name, user)
			c.Close()
			if err != nil {
				return err
			}
			return runSSH(target, key, command)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SSH user (defaults to the image's default user)")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key")
	return cmd
}
