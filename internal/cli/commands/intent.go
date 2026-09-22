package commands

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
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

// memberFlag is one parsed `--vm role:image` or `--container role:image`
// value.
type memberFlag struct {
	kind  anvilv1.Kind
	role  string
	image string
}

// parseMemberFlags turns --vm/--container repeated flag values (each "role:image",
// split on the first colon) into an ordered member list, VMs before containers.
func parseMemberFlags(vmSpecs, containerSpecs []string) ([]memberFlag, error) {
	var out []memberFlag
	for _, spec := range vmSpecs {
		role, image, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf(`--vm %q must be "<role>:<image>"`, spec)
		}
		out = append(out, memberFlag{kind: anvilv1.Kind_KIND_VM, role: role, image: image})
	}
	for _, spec := range containerSpecs {
		role, image, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf(`--container %q must be "<role>:<image>"`, spec)
		}
		out = append(out, memberFlag{kind: anvilv1.Kind_KIND_CONTAINER, role: role, image: image})
	}
	return out, nil
}

func newIntentCreateCommand(flags *globalFlags) *cobra.Command {
	var vmSpecs, containerSpecs []string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new intent with one or more members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			members, err := parseMemberFlags(vmSpecs, containerSpecs)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				return fmt.Errorf("intent create needs at least one --vm or --container member")
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			_, err = c.Intent.Info(cmd.Context(), &anvilv1.IntentInfoRequest{Name: name})
			switch {
			case err == nil:
				return fmt.Errorf("intent %q already exists, use `anvil intent add` to grow it", name)
			case status.Code(err) == codes.NotFound:
			default:
				return fmt.Errorf("intent: checking whether %q already exists: %w", name, err)
			}
			for _, m := range members {
				if err := launchMember(cmd, c, name, m); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&vmSpecs, "vm", nil, "VM member as <role>:<image>, repeatable")
	cmd.Flags().StringArrayVar(&containerSpecs, "container", nil, "container member as <role>:<image>, repeatable")
	return cmd
}

func newIntentAddCommand(flags *globalFlags) *cobra.Command {
	var vmSpecs, containerSpecs []string
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add one or more members to an existing intent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			members, err := parseMemberFlags(vmSpecs, containerSpecs)
			if err != nil {
				return err
			}
			if len(members) == 0 {
				return fmt.Errorf("intent add needs at least one --vm or --container member")
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			if _, err := c.Intent.Info(cmd.Context(), &anvilv1.IntentInfoRequest{Name: name}); err != nil {
				if status.Code(err) == codes.NotFound {
					return fmt.Errorf("no such intent %q, use `anvil intent create` first", name)
				}
				return fmt.Errorf("intent: looking up %q: %w", name, err)
			}
			for _, m := range members {
				if err := launchMember(cmd, c, name, m); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&vmSpecs, "vm", nil, "VM member as <role>:<image>, repeatable")
	cmd.Flags().StringArrayVar(&containerSpecs, "container", nil, "container member as <role>:<image>, repeatable")
	return cmd
}

// launchMember runs one member through the streaming Launch RPC, tagged with intentName/role.
func launchMember(cmd *cobra.Command, c *client.Client, intentName string, m memberFlag) error {
	req := &anvilv1.LaunchRequest{
		Name:       intentName + "-" + m.role,
		Kind:       m.kind,
		IntentName: intentName,
		Role:       m.role,
	}
	switch m.kind {
	case anvilv1.Kind_KIND_VM:
		resolvedKeys, err := resolveSSHKeys(nil)
		if err != nil {
			return err
		}
		req.Vm = &anvilv1.VMSpec{ImageRef: m.image, Cpus: 1, MemoryMib: 1024, SshPublicKeys: resolvedKeys}
	case anvilv1.Kind_KIND_CONTAINER:
		req.Container = &anvilv1.ContainerSpec{ImageRef: m.image}
	}

	stream, err := c.Launch(cmd.Context(), req)
	if err != nil {
		return err
	}
	return streamLaunchProgress(cmd.OutOrStdout(), stream)
}

func newIntentListCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List intents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Intent.List(cmd.Context(), &anvilv1.IntentListRequest{})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMEMBERS")
			for _, it := range reply.GetIntents() {
				fmt.Fprintf(tw, "%s\t%d\n", it.GetName(), len(it.GetMembers()))
			}
			return tw.Flush()
		},
	}
}

func newIntentInfoCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show an intent's members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

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
		},
	}
}

// printIntentMembers prints one row per member, with "-" for a missing
// address or DNS name.
func printIntentMembers(w io.Writer, members []*anvilv1.IntentMember) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tKIND\tIP\tDNS NAME\tINSTANCE ID")
	for _, m := range members {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetRole(), kindLabel(m.GetKind()),
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
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Intent.Remove(cmd.Context(), &anvilv1.IntentRemoveRequest{Name: args[0], Member: args[1]})
			return err
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
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Intent.Delete(cmd.Context(), &anvilv1.IntentDeleteRequest{Name: args[0], PurgeMembers: purgeMembers})
			return err
		},
	}
	cmd.Flags().BoolVar(&purgeMembers, "purge-members", false, "also delete every member instance, not just the group record")
	return cmd
}
