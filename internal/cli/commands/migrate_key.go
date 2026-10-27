package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// newMigrateKeyCommand prints anvild's public SSH key, generating a
// keypair first if one doesn't exist yet.
func newMigrateKeyCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-key",
		Short: "Print anvild's public key for anvil migrate, generating one if needed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Migrate.Key(cmd.Context(), &anvilv1.MigrateKeyRequest{})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), reply.GetPublicKey())
				return nil
			})
		},
	}
}
