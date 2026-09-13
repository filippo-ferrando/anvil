package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newInfoCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>...",
		Short: "Show detailed information about one or more instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Info(cmd.Context(), &anvilv1.InfoRequest{Names: args})
			if err != nil {
				return err
			}
			for _, inst := range reply.GetInstances() {
				fmt.Fprintf(cmd.OutOrStdout(), "Name:\t%s\n", inst.GetName())
				fmt.Fprintf(cmd.OutOrStdout(), "Kind:\t%s\n", kindLabel(inst.GetKind()))
				fmt.Fprintf(cmd.OutOrStdout(), "State:\t%s\n", stateLabel(inst.GetState()))
				if vmSpec := inst.GetVm(); vmSpec != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Image:\t%s\n", vmSpec.GetImageRef())
					fmt.Fprintf(cmd.OutOrStdout(), "CPUs:\t%d\n", vmSpec.GetCpus())
					fmt.Fprintf(cmd.OutOrStdout(), "Memory:\t%d MiB\n", vmSpec.GetMemoryMib())
					fmt.Fprintf(cmd.OutOrStdout(), "Disk:\t%d GiB\n", vmSpec.GetDiskGib())
					if vmSpec.GetSshPort() != 0 {
						user := vmSpec.GetDefaultUser()
						if user == "" {
							user = "root"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "SSH:\tssh -p %d %s@localhost  (or just: anvil shell %s)\n",
							vmSpec.GetSshPort(), user, inst.GetName())
					}
					for _, m := range vmSpec.GetMounts() {
						mode := "rw"
						if m.GetReadOnly() {
							mode = "ro"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "Mount:\t%s -> %s (%s)\n", m.GetHostPath(), m.GetGuestPath(), mode)
					}
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
}
