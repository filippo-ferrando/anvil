package commands

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// printExtraHosts shows a Host: line per entry, sorted by name for a
// stable, readable order rather than Go's random map iteration.
func printExtraHosts(w io.Writer, hosts map[string]string) {
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "Host:\t%s -> %s\n", name, hosts[name])
	}
}

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
				if intentName, role := inst.GetLabels()["intent"], inst.GetLabels()["role"]; intentName != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Intent:\t%s (role: %s)\n", intentName, role)
				}
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
					for _, p := range vmSpec.GetPorts() {
						fmt.Fprintf(cmd.OutOrStdout(), "Port:\t%d -> %d/%s\n", p.GetHostPort(), p.GetGuestPort(), p.GetProtocol())
					}
					for _, m := range vmSpec.GetMounts() {
						mode := "rw"
						if m.GetReadOnly() {
							mode = "ro"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "Mount:\t%s -> %s (%s)\n", m.GetHostPath(), m.GetGuestPath(), mode)
					}
					printExtraHosts(cmd.OutOrStdout(), vmSpec.GetExtraHosts())
				}
				if containerSpec := inst.GetContainer(); containerSpec != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Image:\t%s\n", containerSpec.GetImageRef())
					fmt.Fprintf(cmd.OutOrStdout(), "Engine:\t%s\n", engineLabel(containerSpec.GetEngine()))
					if containerSpec.GetContainerId() != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "Container ID:\t%s\n", containerSpec.GetContainerId())
					}
					if containerSpec.GetNetworkAlias() != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "Network Alias:\t%s\n", containerSpec.GetNetworkAlias())
					}
					for _, p := range containerSpec.GetPorts() {
						fmt.Fprintf(cmd.OutOrStdout(), "Port:\t%d -> %d/%s\n", p.GetHostPort(), p.GetGuestPort(), p.GetProtocol())
					}
					for _, v := range containerSpec.GetVolumes() {
						mode := "rw"
						if v.GetReadOnly() {
							mode = "ro"
						}
						fmt.Fprintf(cmd.OutOrStdout(), "Volume:\t%s -> %s (%s)\n", v.GetHostPath(), v.GetContainerPath(), mode)
					}
					printExtraHosts(cmd.OutOrStdout(), containerSpec.GetExtraHosts())
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
}
