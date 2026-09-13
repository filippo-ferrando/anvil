package commands

import (
	"fmt"
	"text/tabwriter"

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
	)
	return cmd
}

func newHostAddCommand(flags *globalFlags) *cobra.Command {
	var identity string
	cmd := &cobra.Command{
		Use:   "add <alias> <user@host[:port]>",
		Short: "Add a known host",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Host.Add(cmd.Context(), &anvilv1.HostAddRequest{
				Host: &anvilv1.Host{Alias: args[0], Target: args[1], Identity: identity},
			})
			return err
		},
	}
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use for this host, instead of ssh's own default identity resolution "+
		"(anvild runs as root, so this needs to be a key root can read, and its own identity resolution otherwise falls back to root's own ~/.ssh, not yours)")
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
			fmt.Fprintln(tw, "ALIAS\tTARGET\tIDENTITY")
			for _, h := range reply.GetHosts() {
				identity := h.GetIdentity()
				if identity == "" {
					identity = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\n", h.GetAlias(), h.GetTarget(), identity)
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
