package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/sshkey"
)

// newMigrateGuestKeyCommand prints this identity's default anvil
// guest-access public key, generating it first if it doesn't exist yet.
func newMigrateGuestKeyCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:    "migrate-guest-key",
		Short:  "Internal: print this identity's default anvil guest-access public key",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pub, err := sshkey.EnsureDefaultPublic()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), pub)
			return nil
		},
	}
}
