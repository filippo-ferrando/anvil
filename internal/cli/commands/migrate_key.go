package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// newMigrateKeyCommand is `anvil migrate-key`: prints anvild's own public
// key for its outbound `anvil migrate` SSH connections, generating a
// fresh passwordless keypair first if one doesn't exist yet (normally
// already done once by packaging/anvild.install, but a manually-run
// anvild wouldn't have one otherwise). Copy the printed key into a
// target host's ~/.ssh/authorized_keys before migrating to it — adding a
// host with `anvil host add` grants no trust by itself, this is the
// actual key anvild connects with.
func newMigrateKeyCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-key",
		Short: "Print anvild's public key for anvil migrate, generating one if needed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Migrate.Key(cmd.Context(), &anvilv1.MigrateKeyRequest{})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), reply.GetPublicKey())
			return nil
		},
	}
}
