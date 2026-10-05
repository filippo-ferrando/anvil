package commands

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
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
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Host.Add(cmd.Context(), &anvilv1.HostAddRequest{
				Host: &anvilv1.Host{Alias: args[0], Target: args[1], Identity: identity, StrictHostKey: strictKey},
			})
			return err
		},
	}
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use for this host, instead of ssh's own default identity resolution "+
		"(anvild runs as the unprivileged \"anvil\" user, so this needs to be a key that user can read; "+
		"otherwise ssh falls back to that user's own ~/.ssh, not yours)")
	cmd.Flags().BoolVar(&strictKey, "strict-host-key", false, "refuse to connect unless this host's SSH key is already in anvil's own known_hosts file; "+
		"without it the first key seen is accepted and recorded")
	return cmd
}

func newHostListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known hosts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Host.List(cmd.Context(), &anvilv1.HostListRequest{})
			if err != nil {
				return err
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
		},
	}
}

func newHostRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a known host",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Host.Remove(cmd.Context(), &anvilv1.HostRemoveRequest{Alias: args[0]})
			return err
		},
	}
}

func newHostTestCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "test <alias>",
		Short: "Check that a known host is reachable and has anvil installed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Host.Test(cmd.Context(), &anvilv1.HostTestRequest{Alias: args[0]})
			if err != nil {
				return err
			}
			if !reply.GetOk() {
				return fmt.Errorf("%s", reply.GetMessage())
			}
			fmt.Fprintln(cmd.OutOrStdout(), reply.GetMessage())
			return nil
		},
	}
}

func newHostDiscoverCommand(flags *globalFlags) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find other anvil hosts on the local network over mDNS",
		Long: "Find other anvil hosts announcing themselves on the local network. Nothing " +
			"is saved and no trust is granted: pass what it prints to `anvil host add`, " +
			"with the user to log in as.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			// Rounds up: the field is whole seconds, and truncating a
			// sub-second timeout to 0 would silently mean "use the default".
			secs := int32((timeout + time.Second - 1) / time.Second)
			reply, err := c.Host.Discover(cmd.Context(), &anvilv1.HostDiscoverRequest{
				TimeoutSeconds: secs,
			})
			if err != nil {
				return err
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
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "how long to wait for answers")
	return cmd
}
