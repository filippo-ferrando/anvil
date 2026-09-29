package commands

import (
	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newForkCommand(flags *globalFlags) *cobra.Command {
	var start bool

	cmd := &cobra.Command{
		Use:   "fork <instance> <new-name>",
		Short: "Create a new instance by copying an existing instance's disk",
		Long: "Fork copies <instance>'s current disk into a new instance <new-name>, preserving " +
			"its backing file (the shared base image) rather than flattening it, so the fork " +
			"only stores its own deltas, same as any freshly launched overlay. Safe to run while " +
			"<instance> is running: QEMU copies the disk exactly as it was when the fork started, " +
			"while <instance> keeps running. With the guest agent connected, the guest's " +
			"filesystems are frozen for that instant, so the copy is clean, not just like a " +
			"power cut. The fork carries over <instance>'s " +
			"CPUs, memory, cloud-init, SSH keys, mounts, and ports, but always starts out as a " +
			"standalone instance: it doesn't inherit intent membership or a bridge address. " +
			"VM instances only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			stream, err := c.Fork(cmd.Context(), &anvilv1.ForkRequest{Name: args[0], NewName: args[1], Start: start})
			if err != nil {
				return err
			}
			return streamLaunchProgress(cmd.OutOrStdout(), stream)
		},
	}

	cmd.Flags().BoolVar(&start, "start", false, "start the forked instance immediately")
	return cmd
}
