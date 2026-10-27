package commands

import (
	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newDeleteCommand(flags *globalFlags) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "delete <name>...",
		Short: "Delete one or more instances, with their disks",
		Long: `Delete stops the instances and removes them: a VM's disk, a container, and the record.
The cached base image a VM was made from stays, so the next launch of that image is quick.
--purge removes that base image too, when no other VM uses it.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Delete(cmd.Context(), &anvilv1.DeleteRequest{Names: args, Purge: purge})
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the VM's cached base image when no other VM uses it")
	return cmd
}
