package commands

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
	fmt.Fprintf(w, "Cloud-init:\t%s\n", format.CloudInit(g.GetCloudInit()))
	for _, ip := range g.GetIpAddresses() {
		fmt.Fprintf(w, "IP:\t%s\n", ip)
	}
}

// sshHint is the SSH line for a VM. The forward listens on the daemon host's loopback,
// so under --remote it jumps through that host and adds its address on a Host line.
func sshHint(remote, hostAddr string, port int32, user, name string) string {
	if remote == "" {
		return fmt.Sprintf("SSH:\tssh -p %d %s@localhost  (or just: anvil shell %s)\n", port, user, name)
	}
	return fmt.Sprintf("SSH:\tssh -J %s -p %d %s@localhost  (or just: anvil --remote %s shell %s)\nHost:\t%s\n",
		remote, port, user, remote, name, hostAddr)
}

func newInfoCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>...",
		Short: "Show detailed information about one or more instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Info(cmd.Context(), &anvilv1.InfoRequest{Names: args})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				// The remote daemon's host, looked up once when an SSH hint needs it.
				var hostAddr string
				remoteHost := func() string {
					if c.Remote != "" && hostAddr == "" {
						hostAddr = c.RemoteHostAddr(cmd.Context())
					}
					return hostAddr
				}
				for _, inst := range reply.GetInstances() {
					printInstanceInfo(out, inst)
					if vm := inst.GetVm(); vm != nil {
						printVMInfo(out, inst, c.Remote, remoteHost)
					}
					if ct := inst.GetContainer(); ct != nil {
						printContainerInfo(out, ct)
					}
					fmt.Fprintln(out)
				}
				return nil
			})
		},
	}
}

func printInstanceInfo(out io.Writer, inst *anvilv1.Instance) {
	fmt.Fprintf(out, "Name:\t%s\n", inst.GetName())
	fmt.Fprintf(out, "Kind:\t%s\n", format.Kind(inst.GetKind()))
	fmt.Fprintf(out, "State:\t%s\n", stateLabel(inst.GetState()))
	fmt.Fprintf(out, "Autostart:\t%s\n", yesNo(inst.GetAutostart()))
	fmt.Fprintf(out, "Restart:\t%s\n", format.RestartPolicy(inst.GetRestartPolicy()))
	fmt.Fprintf(out, "GUI:\t%s\n", yesNo(inst.GetGuiEnabled()))
	if intentName, role := inst.GetLabels()["intent"], inst.GetLabels()["role"]; intentName != "" {
		fmt.Fprintf(out, "Intent:\t%s (role: %s)\n", intentName, role)
	}
}

func printVMInfo(out io.Writer, inst *anvilv1.Instance, remote string, remoteHost func() string) {
	vm := inst.GetVm()
	fmt.Fprintf(out, "Image:\t%s\n", vm.GetImageRef())
	fmt.Fprintf(out, "Hypervisor:\t%s\n", format.Hypervisor(vm.GetHypervisor()))
	fmt.Fprintf(out, "CPUs:\t%d\n", vm.GetCpus())
	fmt.Fprintf(out, "Memory:\t%d MiB\n", vm.GetMemoryMib())
	fmt.Fprintf(out, "Disk:\t%d GiB\n", vm.GetDiskGib())
	if devs := vm.GetDevices(); len(devs) > 0 {
		fmt.Fprintf(out, "Devices:\t%s\n", strings.Join(devs, ", "))
	}
	if vm.GetSshPort() != 0 {
		user := vm.GetDefaultUser()
		if user == "" {
			user = "root"
		}
		fmt.Fprint(out, sshHint(remote, remoteHost(), vm.GetSshPort(), user, inst.GetName()))
	}
	for _, p := range vm.GetPorts() {
		fmt.Fprintf(out, "Port:\t%d -> %d/%s\n", p.GetHostPort(), p.GetGuestPort(), p.GetProtocol())
	}
	for _, m := range vm.GetMounts() {
		fmt.Fprintf(out, "Mount:\t%s -> %s (%s)\n", m.GetHostPath(), m.GetGuestPath(), accessMode(m.GetReadOnly()))
	}
	printExtraHosts(out, vm.GetExtraHosts())
	printGuestInfo(out, inst)
	if s := vm.GetSnapshotSchedule(); s.GetEverySeconds() > 0 {
		fmt.Fprintf(out, "Snapshots:\t%s\n", format.Schedule(s, "2006-01-02 15:04"))
	}
}

func printContainerInfo(out io.Writer, ct *anvilv1.ContainerSpec) {
	fmt.Fprintf(out, "Image:\t%s\n", ct.GetImageRef())
	fmt.Fprintf(out, "Engine:\t%s\n", format.Engine(ct.GetEngine()))
	if id := ct.GetContainerId(); id != "" {
		fmt.Fprintf(out, "Container ID:\t%s\n", id)
	}
	if alias := ct.GetNetworkAlias(); alias != "" {
		fmt.Fprintf(out, "Network Alias:\t%s\n", alias)
	}
	for _, p := range ct.GetPorts() {
		fmt.Fprintf(out, "Port:\t%d -> %d/%s\n", p.GetHostPort(), p.GetGuestPort(), p.GetProtocol())
	}
	for _, v := range ct.GetVolumes() {
		fmt.Fprintf(out, "Volume:\t%s -> %s (%s)\n", v.GetHostPath(), v.GetContainerPath(), accessMode(v.GetReadOnly()))
	}
	printExtraHosts(out, ct.GetExtraHosts())
}

func accessMode(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}
