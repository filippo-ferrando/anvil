package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/sshkey"
)

// newMigrateGuestKeyCommand is `anvil migrate-guest-key`: plumbing, not
// meant to be run by hand, same as `anvil migrate-import`. Run by the
// source host over SSH (see internal/migrate.Manager.GuestKey), driven
// by the CLI's own `anvil migrate` before it migrates a VM. Prints this
// invoking identity's own default anvil guest-access public key —
// exactly the one `anvil launch` already bakes into every VM this
// identity creates, see resolveSSHKeys/internal/sshkey — generating it
// on the spot if it doesn't exist yet.
//
// The source injects the printed key into a VM's guest before migrating
// it, since a migrated disk skips cloud-init entirely on relaunch (see
// PLAN.md's Migration section): without this, nothing this host's own
// `anvil shell`/`exec`/`transfer` could authenticate with would ever be
// authorized in a VM migrated from elsewhere.
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
