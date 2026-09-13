package commands

import (
	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newDeleteCommand(flags *globalFlags) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "delete <name>...",
		Short: "Delete one or more instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Delete(cmd.Context(), &anvilv1.DeleteRequest{Names: args, Purge: purge})
			return err
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "permanently remove immediately, instead of keeping it recoverable until `anvil purge`")
	return cmd
}

func newPurgeCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "purge [name...]",
		Short: "Permanently remove deleted instances (all of them, if no names are given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Purge(cmd.Context(), &anvilv1.PurgeRequest{Names: args})
			return err
		},
	}
}
