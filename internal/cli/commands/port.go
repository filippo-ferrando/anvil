package commands

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newPortCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "port",
		Short: "Add, remove, or list host-to-guest port forwards on an instance, without recreating it",
	}
	cmd.AddCommand(newPortAddCommand(flags), newPortRemoveCommand(flags), newPortListCommand(flags))
	return cmd
}

func newPortAddCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "add <name> <host>:<guest>[/tcp|udp]",
		Short: "Add a host-to-guest port forward, without recreating the instance",
		Long: "Add a host-to-guest port forward to an instance. On a SLIRP VM this takes " +
			"effect immediately over QMP if it's running — no restart. On a container, " +
			"Docker has no live port-binding mutation, so this recreates the underlying " +
			"container instead, restarting it if it was running, rather than making you " +
			"relaunch the whole instance yourself. Bridge-networked VMs have their own " +
			"address and don't use host-forwarded ports at all.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := parsePortMapping(args[1])
			if err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.AddPort(cmd.Context(), &anvilv1.AddPortRequest{Name: args[0], Port: port})
			return err
		},
	}
}

func newPortRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <host>[/tcp|udp]",
		Short: "Remove a host-to-guest port forward added with `anvil port add`",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostPort, protocol, err := parseHostPort(args[1])
			if err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.RemovePort(cmd.Context(), &anvilv1.RemovePortRequest{
				Name: args[0], HostPort: int32(hostPort), Protocol: protocol,
			})
			return err
		},
	}
}

func newPortListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list <name>",
		Short: "List an instance's host-to-guest port forwards",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Info(cmd.Context(), &anvilv1.InfoRequest{Names: []string{args[0]}})
			if err != nil {
				return err
			}
			insts := reply.GetInstances()
			if len(insts) == 0 {
				return fmt.Errorf("no instance named %q", args[0])
			}

			inst := insts[0]
			ports := inst.GetVm().GetPorts()
			if inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
				ports = inst.GetContainer().GetPorts()
			}
			if len(ports) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no port forwards")
				return nil
			}
			for _, p := range ports {
				proto := p.GetProtocol()
				if proto == "" {
					proto = "tcp"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d:%d/%s\n", p.GetHostPort(), p.GetGuestPort(), proto)
			}
			return nil
		},
	}
}

// parsePortMapping parses "<host>:<guest>[/tcp|udp]".
func parsePortMapping(s string) (*anvilv1.PortMapping, error) {
	spec, protocol, err := splitProtocol(s)
	if err != nil {
		return nil, err
	}
	hostStr, guestStr, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf(`port must be "<host>:<guest>[/tcp|udp]", got %q`, s)
	}
	hostPort, err := strconv.Atoi(hostStr)
	if err != nil {
		return nil, fmt.Errorf("invalid host port %q: %w", hostStr, err)
	}
	guestPort, err := strconv.Atoi(guestStr)
	if err != nil {
		return nil, fmt.Errorf("invalid guest port %q: %w", guestStr, err)
	}
	return &anvilv1.PortMapping{HostPort: int32(hostPort), GuestPort: int32(guestPort), Protocol: protocol}, nil
}

// parseHostPort parses "<host>[/tcp|udp]".
func parseHostPort(s string) (hostPort int, protocol string, err error) {
	spec, protocol, err := splitProtocol(s)
	if err != nil {
		return 0, "", err
	}
	hostPort, err = strconv.Atoi(spec)
	if err != nil {
		return 0, "", fmt.Errorf("invalid host port %q: %w", spec, err)
	}
	return hostPort, protocol, nil
}

// splitProtocol splits "<rest>[/tcp|udp]", validating an explicit protocol
// and defaulting to "" (which every consumer already treats as "tcp").
func splitProtocol(s string) (rest, protocol string, err error) {
	rest, protocol, ok := strings.Cut(s, "/")
	if !ok {
		return s, "", nil
	}
	protocol = strings.ToLower(protocol)
	if protocol != "tcp" && protocol != "udp" {
		return "", "", fmt.Errorf(`protocol must be "tcp" or "udp", got %q`, protocol)
	}
	return rest, protocol, nil
}
