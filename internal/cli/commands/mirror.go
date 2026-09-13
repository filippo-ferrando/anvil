package commands

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newMirrorCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Manage runtime-added VM image and container registry mirrors",
	}
	cmd.AddCommand(
		newMirrorAddCommand(flags),
		newMirrorListCommand(flags),
		newMirrorRemoveCommand(flags),
		newMirrorEnableCommand(flags, true),
		newMirrorEnableCommand(flags, false),
	)
	return cmd
}

func parseMirrorKind(s string) (anvilv1.MirrorKind, error) {
	switch s {
	case "vm":
		return anvilv1.MirrorKind_MIRROR_KIND_VM, nil
	case "container":
		return anvilv1.MirrorKind_MIRROR_KIND_CONTAINER, nil
	default:
		return anvilv1.MirrorKind_MIRROR_KIND_UNSPECIFIED, fmt.Errorf(`--kind must be "vm" or "container"`)
	}
}

func newMirrorAddCommand(flags *globalFlags) *cobra.Command {
	var (
		kindStr     string
		manifestURL string
		registry    string
		mirrorOf    string
		insecure    bool
		priority    int32
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add a mirror",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseMirrorKind(kindStr)
			if err != nil {
				return err
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			_, err = c.Mirror.Add(cmd.Context(), &anvilv1.MirrorAddRequest{
				Mirror: &anvilv1.Mirror{
					Name:        args[0],
					Kind:        kind,
					ManifestUrl: manifestURL,
					Registry:    registry,
					MirrorOf:    mirrorOf,
					Insecure:    insecure,
					Priority:    priority,
				},
			})
			return err
		},
	}
	cmd.Flags().StringVar(&kindStr, "kind", "", `mirror kind: "vm" or "container" (required)`)
	_ = cmd.MarkFlagRequired("kind")
	cmd.Flags().StringVar(&manifestURL, "manifest-url", "", "VM mirrors: URL of a distribution-info.json-shaped manifest")
	cmd.Flags().StringVar(&registry, "registry", "", "container mirrors: registry host[:port]")
	cmd.Flags().StringVar(&mirrorOf, "mirror-of", "", "container mirrors: the upstream registry this one mirrors")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "container mirrors: skip TLS verification")
	cmd.Flags().Int32Var(&priority, "priority", 0, "higher priority wins on a catalog entry collision")
	return cmd
}

func newMirrorListCommand(flags *globalFlags) *cobra.Command {
	var kindStr string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured mirrors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var kindFilter anvilv1.MirrorKind
			if kindStr != "" {
				var err error
				kindFilter, err = parseMirrorKind(kindStr)
				if err != nil {
					return err
				}
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			reply, err := c.Mirror.List(cmd.Context(), &anvilv1.MirrorListRequest{KindFilter: kindFilter})
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tSOURCE\tPRIORITY\tENABLED")
			for _, m := range reply.GetMirrors() {
				kind := "vm"
				source := m.GetManifestUrl()
				if m.GetKind() == anvilv1.MirrorKind_MIRROR_KIND_CONTAINER {
					kind = "container"
					source = m.GetRegistry()
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%v\n", m.GetName(), kind, source, m.GetPriority(), m.GetEnabled())
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&kindStr, "kind", "", `filter by kind: "vm" or "container" (default: both)`)
	return cmd
}

func newMirrorRemoveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a mirror",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Mirror.Remove(cmd.Context(), &anvilv1.MirrorRemoveRequest{Name: args[0]})
			return err
		},
	}
}

func newMirrorEnableCommand(flags *globalFlags, enable bool) *cobra.Command {
	use, short := "enable <name>", "Enable a mirror"
	if !enable {
		use, short = "disable <name>", "Disable a mirror"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Mirror.SetEnabled(cmd.Context(), &anvilv1.MirrorSetEnabledRequest{Name: args[0], Enabled: enable})
			return err
		},
	}
}
