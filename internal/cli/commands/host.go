package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newHostCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Manage known hosts for `anvil migrate --to`",
		Long: "Manage known hosts for `anvil migrate --to`. Adding a host here grants no " +
			"trust by itself, it's just a saved shortcut for whatever real SSH access you " +
			"already have to it.",
	}
	cmd.AddCommand(
		newHostAddCommand(flags),
		newHostListCommand(flags),
		newHostRemoveCommand(flags),
		newHostTestCommand(flags),
		newHostDiscoverCommand(flags),
		newHostDevicesCommand(flags),
	)
	return cmd
}

func newHostAddCommand(flags *globalFlags) *cobra.Command {
	var (
		identity  string
		strictKey bool
	)
	cmd := &cobra.Command{
		Use:   "add <alias> <user@host[:port]>",
		Short: "Add a known host",
		Long: "Add a known host. Adding the same alias again replaces it, which is how " +
			"--strict-host-key is turned on for a host whose key anvil already recorded.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Host.Add(cmd.Context(), &anvilv1.HostAddRequest{
					Host: &anvilv1.Host{Alias: args[0], Target: args[1], Identity: identity, StrictHostKey: strictKey},
				})
				return err
			})
		},
	}
	addHostKeyFlags(cmd, &identity, &strictKey)
	return cmd
}

// addHostKeyFlags registers the SSH options shared by `host add` and `host discover --add`.
func addHostKeyFlags(cmd *cobra.Command, identity *string, strictKey *bool) {
	cmd.Flags().StringVarP(identity, "identity", "i", "", "path to a private key to use for this host, instead of ssh's own default identity resolution "+
		"(anvild runs as the unprivileged \"anvil\" user, so this needs to be a key that user can read; "+
		"otherwise ssh falls back to that user's own ~/.ssh, not yours)")
	cmd.Flags().BoolVar(strictKey, "strict-host-key", false, "refuse to connect unless this host's SSH key is already in anvil's own known_hosts file; "+
		"without it the first key seen is accepted and recorded")
}

func newHostListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Host.List(cmd.Context(), &anvilv1.HostListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ALIAS\tTARGET\tIDENTITY\tHOST KEY")
				for _, h := range reply.GetHosts() {
					identity := h.GetIdentity()
					if identity == "" {
						identity = "-"
					}
					hostKey := "trust on first use"
					if h.GetStrictHostKey() {
						hostKey = "strict"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.GetAlias(), h.GetTarget(), identity, hostKey)
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newHostRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a known host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Host.Remove(cmd.Context(), &anvilv1.HostRemoveRequest{Alias: args[0]})
				return err
			})
		},
	}
}

func newHostTestCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "test <alias>",
		Short: "Check that a known host is reachable and has anvil installed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Host.Test(cmd.Context(), &anvilv1.HostTestRequest{Alias: args[0]})
				if err != nil {
					return err
				}
				if !reply.GetOk() {
					return fmt.Errorf("%s", reply.GetMessage())
				}
				fmt.Fprintln(cmd.OutOrStdout(), reply.GetMessage())
				return nil
			})
		},
	}
}

func newHostDiscoverCommand(flags *globalFlags) *cobra.Command {
	var (
		timeout time.Duration
		output  string
		add     discoverAddOptions
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find other anvil hosts on the local network over mDNS",
		Long: "Find other anvil hosts announcing themselves on the local network. Nothing " +
			"is saved and no trust is granted unless --add is given: --add <name> saves the " +
			"discovered host with that name (or address) as a known host, like `anvil host add`, " +
			"logging in as --user.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			if add.key == "" {
				for _, name := range []string{"alias", "user", "identity", "strict-host-key"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s needs --add", name)
					}
				}
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Host.Discover(cmd.Context(), &anvilv1.HostDiscoverRequest{
					TimeoutSeconds: client.CeilSeconds(timeout),
				})
				if err != nil {
					return err
				}
				if add.key != "" {
					host, err := addDiscoveredHost(cmd.Context(), c.Host, reply.GetHosts(), add)
					if err != nil {
						return err
					}
					if output == "json" {
						return printJSON(cmd.OutOrStdout(), host)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "added host %s (%s)\n", host.GetAlias(), host.GetTarget())
					return nil
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				if len(reply.GetHosts()) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no anvil hosts answered on this network")
					return nil
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tADDRESS\tSSH PORT")
				for _, h := range reply.GetHosts() {
					fmt.Fprintf(tw, "%s\t%s\t%d\n", h.GetName(), h.GetAddress(), h.GetSshPort())
				}
				return tw.Flush()
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "how long to wait for answers")
	cmd.Flags().StringVar(&add.key, "add", "", "save the discovered host with this name or address as a known host")
	cmd.Flags().StringVar(&add.alias, "alias", "", "alias to save the host as (default: its discovered name)")
	cmd.Flags().StringVar(&add.user, "user", "root", "user to log in as on the added host")
	addHostKeyFlags(cmd, &add.identity, &add.strictKey)
	addOutputFlag(cmd, &output)
	return cmd
}

// discoverAddOptions are the `host discover --add` fields, the same ones the TUI pre-fills.
type discoverAddOptions struct {
	key, alias, user, identity string
	strictKey                  bool
}

// addDiscoveredHost saves the discovered host matching o.key through the same Add RPC as `host add`.
func addDiscoveredHost(ctx context.Context, hc anvilv1.HostServiceClient, hosts []*anvilv1.DiscoveredHost, o discoverAddOptions) (*anvilv1.Host, error) {
	h, err := client.FindDiscovered(hosts, o.key)
	if err != nil {
		return nil, err
	}
	if o.user == "" {
		return nil, fmt.Errorf("--user can't be empty")
	}
	alias := o.alias
	if alias == "" {
		alias = h.GetName()
	}
	host := &anvilv1.Host{
		Alias:         alias,
		Target:        o.user + "@" + client.DiscoveredAddress(h),
		Identity:      o.identity,
		StrictHostKey: o.strictKey,
	}
	if _, err := hc.Add(ctx, &anvilv1.HostAddRequest{Host: host}); err != nil {
		return nil, err
	}
	return host, nil
}

func newHostDevicesCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List the host's USB and PCI devices that can be passed to a VM",
		Long: "List the daemon host's USB and PCI devices, with the id `anvil launch --device` takes. " +
			"A PCI device brings the other devices of its IOMMU group along. Devices behind the root " +
			"filesystem, the default route or the host display are blocked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Host.ListHostDevices(cmd.Context(), &anvilv1.ListHostDevicesRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tVENDOR:PRODUCT\tNAME\tDRIVER\tIOMMU\tSTATUS")
				for _, d := range reply.GetDevices() {
					driver := d.GetDriver()
					if driver == "" {
						driver = "-"
					}
					status := client.DeviceStatus(d)
					if len(d.GetGroupMembers()) > 0 {
						status += " (with " + strings.Join(d.GetGroupMembers(), ", ") + ")"
					}
					group := "-"
					if d.GetIommuGroup() >= 0 {
						group = strconv.Itoa(int(d.GetIommuGroup()))
					}
					fmt.Fprintf(tw, "%s\t%s:%s\t%s\t%s\t%s\t%s\n", d.GetId(), d.GetVendorId(), d.GetProductId(), d.GetName(), driver, group, status)
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}
