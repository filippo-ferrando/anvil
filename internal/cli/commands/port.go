package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
			"effect immediately over QMP if it's running; no restart needed. On a container, " +
			"Docker has no live port-binding mutation, so this recreates the underlying " +
			"container instead, restarting it if it was running, rather than making you " +
			"relaunch the whole instance yourself. Bridge-networked VMs have their own " +
			"address and don't use host-forwarded ports at all.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			port, err := client.ParsePortMapping(args[1])
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				_, err = c.AddPort(cmd.Context(), &anvilv1.AddPortRequest{Name: args[0], Port: port})
				return err
			})
		},
	}
}

func newPortRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <host>[/tcp|udp]",
		Short: "Remove a host-to-guest port forward added with `anvil port add`",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostPort, protocol, err := client.ParseHostPort(args[1])
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				_, err = c.RemovePort(cmd.Context(), &anvilv1.RemovePortRequest{
					Name: args[0], HostPort: int32(hostPort), Protocol: protocol,
				})
				return err
			})
		},
	}
}

func newPortListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list <name>",
		Short: "List an instance's host-to-guest port forwards",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
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
				if output == "json" {
					// No list reply exists for ports, so print the instance's own reply.
					return printJSON(cmd.OutOrStdout(), reply)
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
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}
