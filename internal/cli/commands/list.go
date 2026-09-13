package commands

import (
	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List instances",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.List(cmd.Context(), &anvilv1.ListRequest{})
			if err != nil {
				return err
			}
			printInstanceTable(cmd.OutOrStdout(), reply.GetInstances())
			return nil
		},
	}
}
