package commands

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// printExtraHosts prints a "Host:" line per entry, sorted by name.
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

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// restartPolicyLabel renders p the way --restart takes it.
func restartPolicyLabel(p *anvilv1.RestartPolicy) string {
	mode := p.GetMode()
	if mode == "" {
		mode = "no"
	}
	if p.GetMaxRetries() > 0 {
		return fmt.Sprintf("%s:%d", mode, p.GetMaxRetries())
	}
	return mode
}

// scheduleLabel renders a snapshot schedule, e.g. "every 6h0m0s, keep 8, last 2026-09-29 18:00".
func scheduleLabel(s *anvilv1.SnapshotSchedule) string {
	out := fmt.Sprintf("every %s, keep %d", time.Duration(s.GetEverySeconds())*time.Second, s.GetKeep())
	if s.GetLastRunUnix() > 0 {
		out += ", last " + time.Unix(s.GetLastRunUnix(), 0).Format("2006-01-02 15:04")
	}
	return out
}

// printGuestInfo prints what the guest agent reports, for a running VM.
func printGuestInfo(w io.Writer, inst *anvilv1.Instance) {
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return
	}
	g := inst.GetGuest()
	agent := "not connected"
	if g.GetAgentConnected() {
		agent = "connected"
	} else if inst.GetVm().GetNoGuestAgent() {
		agent = "disabled (--no-guest-agent)"
	}
	fmt.Fprintf(w, "Guest agent:\t%s\n", agent)
	fmt.Fprintf(w, "Cloud-init:\t%s\n", cloudInitLabel(g.GetCloudInit()))
	for _, ip := range g.GetIpAddresses() {
		fmt.Fprintf(w, "IP:\t%s\n", ip)
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
				fmt.Fprintf(cmd.OutOrStdout(), "Autostart:\t%s\n", yesNo(inst.GetAutostart()))
				fmt.Fprintf(cmd.OutOrStdout(), "Restart:\t%s\n", restartPolicyLabel(inst.GetRestartPolicy()))
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
					printGuestInfo(cmd.OutOrStdout(), inst)
					if s := vmSpec.GetSnapshotSchedule(); s.GetEverySeconds() > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "Snapshots:\t%s\n", scheduleLabel(s))
					}
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
