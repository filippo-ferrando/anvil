package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newTransferCommand(flags *globalFlags) *cobra.Command {
	var user, identity string
	cmd := &cobra.Command{
		Use:   "transfer <source> <destination>",
		Short: "Copy a file to or from a VM (scp) or container (exactly one side must be <name>:<path>)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := client.ParseTransfer(args[0], args[1])
			if err != nil {
				return err
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			inst, err := resolveInstance(cmd.Context(), c, t.Instance)
			c.Close()
			if err != nil {
				return err
			}

			if inst.GetContainer() != nil && (cmd.Flags().Changed("user") || cmd.Flags().Changed("identity")) {
				fmt.Fprintln(os.Stderr, "anvil: --user/--identity have no effect on a container instance, ignoring")
			}
			copyCmd, err := c.TransferCommand(inst, t, user, identity)
			if err != nil {
				return err
			}
			return copyCmd.Run()
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SSH user for the remote side (VM only, defaults to the image's default user)")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key (VM only)")
	return cmd
}
