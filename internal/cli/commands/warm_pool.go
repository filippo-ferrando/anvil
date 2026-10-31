package commands

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newWarmPoolCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Keep clones of a warm snapshot restored ahead of time",
		Long: "A pool keeps a number of clones of one distro and size restored and running, so " +
			"`anvil launch --ephemeral` and `anvil matrix` take one in milliseconds instead of restoring. " +
			"anvild refills it in the background and fills it again when it starts.",
	}
	cmd.AddCommand(newWarmPoolSetCommand(flags), newWarmPoolListCommand(flags), newWarmPoolRmCommand(flags))
	return cmd
}

func newWarmPoolSetCommand(flags *globalFlags) *cobra.Command {
	var (
		f    ephemeralFlags
		size int32
	)
	cmd := &cobra.Command{
		Use:   "set <distro> --size <n>",
		Short: "Set how many clones of a distro and size a pool keeps ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("size") {
				return fmt.Errorf("give the pool's --size (0 removes it)")
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmPoolSet(cmd.Context(), &anvilv1.WarmPoolSetRequest{
					Distro: args[0], Arch: f.arch, Cpus: f.cpus, MemoryMib: f.memoryMiB, Size: size,
				})
				if err != nil {
					return err
				}
				p := reply.GetPool()
				if p.GetSize() == 0 {
					fmt.Printf("removed the %s pool\n", poolLabel(p))
					return nil
				}
				fmt.Printf("the %s pool keeps %d clones ready, filling in the background\n", poolLabel(p), p.GetSize())
				return nil
			})
		},
	}
	f.addSizeFlags(cmd)
	cmd.Flags().Int32Var(&size, "size", 0, "clones to keep ready (0 removes the pool)")
	return cmd
}

func newWarmPoolListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List pools and how many clones each has ready",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmPoolList(cmd.Context(), &anvilv1.WarmPoolListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "DISTRO\tARCH\tCPUS\tMEMORY\tREADY\tWARM KEY\tERROR")
				for _, p := range reply.GetPools() {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%d MiB\t%d/%d\t%s\t%s\n", p.GetDistro(), p.GetArch(), p.GetCpus(), p.GetMemoryMib(),
						p.GetReady(), p.GetSize(), dashIfEmpty(p.GetWarmKey()), dashIfEmpty(p.GetError()))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newWarmPoolRmCommand(flags *globalFlags) *cobra.Command {
	var f ephemeralFlags
	cmd := &cobra.Command{
		Use:     "rm <distro>",
		Aliases: []string{"remove"},
		Short:   "Remove the pool of a distro and size, deleting its clones",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmPoolSet(cmd.Context(), &anvilv1.WarmPoolSetRequest{
					Distro: args[0], Arch: f.arch, Cpus: f.cpus, MemoryMib: f.memoryMiB,
				})
				if err != nil {
					return err
				}
				fmt.Printf("removed the %s pool\n", poolLabel(reply.GetPool()))
				return nil
			})
		},
	}
	f.addSizeFlags(cmd)
	return cmd
}

// poolLabel is "ubuntu-24.04 (2 CPUs, 512 MiB)".
func poolLabel(p *anvilv1.WarmPool) string {
	return fmt.Sprintf("%s (%s, %d MiB)", p.GetDistro(), format.CPUs(p.GetCpus()), p.GetMemoryMib())
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// warmPool is warm list's POOL column: ready/size, "-" without a pool.
func warmPool(w *anvilv1.WarmSnapshot) string {
	if w.GetPoolSize() == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", w.GetPoolReady(), w.GetPoolSize())
}
