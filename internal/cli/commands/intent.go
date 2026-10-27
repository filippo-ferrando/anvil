package commands

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newIntentCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intent",
		Short: "Manage intents: named groups of VM/container instances",
		Long: "Manage intents: named groups of VM/container instances. A member is " +
			"provisioned exactly the way a standalone `anvil launch` would be, just tagged " +
			"with the intent's name and a role. Every member shares one network and can " +
			"resolve every other member by name.",
	}
	cmd.AddCommand(
		newIntentCreateCommand(flags),
		newIntentAddCommand(flags),
		newIntentListCommand(flags),
		newIntentInfoCommand(flags),
		newIntentRemoveCommand(flags),
		newIntentDeleteCommand(flags),
	)
	return cmd
}

// parseMemberFlags turns --vm/--container repeated flag values (each "role:image",
// split on the first colon) into an ordered member list, VMs before containers.
func parseMemberFlags(vmSpecs, containerSpecs []string) ([]*anvilv1.IntentMemberSpec, error) {
	var out []*anvilv1.IntentMemberSpec
	for _, flag := range []struct {
		name  string
		kind  anvilv1.Kind
		specs []string
	}{{"--vm", anvilv1.Kind_KIND_VM, vmSpecs}, {"--container", anvilv1.Kind_KIND_CONTAINER, containerSpecs}} {
		for _, spec := range flag.specs {
			m, err := client.ParseMemberSpec(flag.kind, spec)
			if err != nil {
				return nil, fmt.Errorf("%s %w", flag.name, err)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func newIntentCreateCommand(flags *globalFlags) *cobra.Command {
	return newIntentMembersCommand(flags, true, "create <name>", "Create a new intent with one or more members")
}

func newIntentAddCommand(flags *globalFlags) *cobra.Command {
	return newIntentMembersCommand(flags, false, "add <name>", "Add one or more members to an existing intent")
}

// newIntentMembersCommand builds `intent create` (create=true) and `intent add`, both
// served by IntentService.AddMembers, which launches the members in order.
func newIntentMembersCommand(flags *globalFlags, create bool, use, short string) *cobra.Command {
	var vmSpecs, containerSpecs []string
	var hypervisor string
	verb := "add"
	if create {
		verb = "create"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + ". Members launch one at a time, VMs first; the first failure stops " +
			"the rest, and members launched before it are kept.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			members, err := parseMemberFlags(vmSpecs, containerSpecs)
			if err != nil {
				return err
			}
			hv, err := parseVMHypervisor(hypervisor)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				return fmt.Errorf("intent %s needs at least one --vm or --container member", verb)
			}
			req, err := client.NewAddMembersRequest(args[0], create, members, hv)
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Intent.AddMembers(cmd.Context(), req)
				if err != nil {
					return err
				}
				return streamAddMembers(cmd.OutOrStdout(), stream)
			})
		},
	}
	cmd.Flags().StringArrayVar(&vmSpecs, "vm", nil, "VM member as <role>:<image>, repeatable")
	cmd.Flags().StringArrayVar(&containerSpecs, "container", nil, "container member as <role>:<image>, repeatable")
	cmd.Flags().StringVar(&hypervisor, "hypervisor", "", `hypervisor for the --vm members: "qemu" (default) or "firecracker"`)
	return cmd
}

func newIntentListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List intents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Intent.List(cmd.Context(), &anvilv1.IntentListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(cmd.OutOrStdout(), reply)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tMEMBERS")
				for _, it := range reply.GetIntents() {
					fmt.Fprintf(tw, "%s\t%d\n", it.GetName(), len(it.GetMembers()))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newIntentInfoCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show an intent's members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Intent.Info(cmd.Context(), &anvilv1.IntentInfoRequest{Name: args[0]})
				if err != nil {
					return err
				}
				it := reply.GetIntent()
				fmt.Fprintf(cmd.OutOrStdout(), "Name:\t%s\n", it.GetName())
				if net := it.GetNetwork(); net != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "Network:\t%s (bridge %s, subnet %s)\n",
						net.GetEngineNetworkName(), net.GetBridgeInterface(), net.GetSubnet())
					if net.GetDnsDomain() != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "DNS:\t%s (server %s)\n", net.GetDnsDomain(), net.GetDnsServer())
					}
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Network:\tnone yet")
				}
				printIntentMembers(cmd.OutOrStdout(), it.GetMembers())
				return nil
			})
		},
	}
}

// printIntentMembers prints one row per member, with "-" for a missing
// address or DNS name.
func printIntentMembers(w io.Writer, members []*anvilv1.IntentMember) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tKIND\tIP\tDNS NAME\tINSTANCE ID")
	for _, m := range members {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetRole(), format.Kind(m.GetKind()),
			orDash(m.GetIp()), orDash(m.GetDnsName()), m.GetInstanceId())
	}
	tw.Flush()
}

func newIntentRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name> <member>",
		Short: "Ungroup a member (by role or instance ID) from an intent, without deleting it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Intent.Remove(cmd.Context(), &anvilv1.IntentRemoveRequest{Name: args[0], Member: args[1]})
				return err
			})
		},
	}
}

func newIntentDeleteCommand(flags *globalFlags) *cobra.Command {
	var purgeMembers bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an intent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				_, err := c.Intent.Delete(cmd.Context(), &anvilv1.IntentDeleteRequest{Name: args[0], PurgeMembers: purgeMembers})
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&purgeMembers, "purge-members", false, "also delete every member instance, not just the group record")
	return cmd
}
