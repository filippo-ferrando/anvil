package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newSnapshotCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Create, restore, delete, or list QCOW2 internal snapshots of a VM's disk",
		Long: "Manage QCOW2 internal snapshots of a VM's disk — point-in-time checkpoints " +
			"you can restore back to later, in place. Create/delete apply live if the VM " +
			"is running, no stop needed; restore always stops the VM first (a plain " +
			"restart afterward, always booting fresh from the restored disk) since a " +
			"running QEMU process holds the disk file locked. Container instances aren't supported.",
	}
	cmd.AddCommand(
		newSnapshotCreateCommand(flags),
		newSnapshotRestoreCommand(flags),
		newSnapshotDeleteCommand(flags),
		newSnapshotListCommand(flags),
	)
	return cmd
}

func newSnapshotCreateCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "create <instance> <snapshot-name>",
		Short: "Create a new snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Snapshot.Create(cmd.Context(), &anvilv1.SnapshotCreateRequest{Name: args[0], SnapshotName: args[1]})
			return err
		},
	}
}

func newSnapshotRestoreCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <instance> <snapshot-name>",
		Short: "Reset an instance's disk back to a previously created snapshot",
		Long: "Reset an instance's disk back to a previously created snapshot. Stops the " +
			"instance first if it's running, and restarts it afterward — always a fresh " +
			"boot from the restored disk, not a live resume of that snapshot's saved " +
			"state even if it has one (see `anvil snapshot list`'s VM-state column).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Snapshot.Restore(cmd.Context(), &anvilv1.SnapshotRestoreRequest{Name: args[0], SnapshotName: args[1]})
			return err
		},
	}
}

func newSnapshotDeleteCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <instance> <snapshot-name>",
		Short: "Delete a snapshot",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Snapshot.Delete(cmd.Context(), &anvilv1.SnapshotDeleteRequest{Name: args[0], SnapshotName: args[1]})
			return err
		},
	}
}

func newSnapshotListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list <instance>",
		Short: "List an instance's snapshots",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Snapshot.List(cmd.Context(), &anvilv1.SnapshotListRequest{Name: args[0]})
			if err != nil {
				return err
			}
			snaps := reply.GetSnapshots()
			if len(snaps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no snapshots")
				return nil
			}
			for _, s := range snaps {
				state := "disk-only"
				if s.GetHasVmState() {
					state = "disk+VM state"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-30s %-20s %s\n",
					s.GetName(), time.Unix(s.GetCreatedAtUnix(), 0).Format(time.RFC3339), state)
			}
			return nil
		},
	}
}
