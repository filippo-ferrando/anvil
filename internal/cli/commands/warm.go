package commands

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

func newWarmCommand(flags *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "warm",
		Short: "Manage the warm snapshots ephemeral clones start from",
		Long: "A warm snapshot is a booted, settled Firecracker VM of one distro and size, saved to disk. " +
			"`anvil launch --ephemeral` and `anvil matrix` build one when they need it; these commands " +
			"build, list and remove them by hand, and `anvil warm pool` keeps clones of one restored ahead of time.",
	}
	cmd.AddCommand(newWarmCreateCommand(flags), newWarmListCommand(flags), newWarmRmCommand(flags), newWarmSaveCommand(flags), newWarmPoolCommand(flags))
	return cmd
}

// addSizeFlags registers --cpus, --memory and --arch, part of a warm snapshot's key.
func (f *ephemeralFlags) addSizeFlags(cmd *cobra.Command) {
	cmd.Flags().Int32Var(&f.cpus, "cpus", 0, "vCPUs of the clones (default 2)")
	cmd.Flags().Int64Var(&f.memoryMiB, "memory", 0, "memory of the clones in MiB (default 512)")
	cmd.Flags().StringVar(&f.arch, "arch", "", "image architecture (default: this host's)")
}

// addSetupFlags registers --setup and --key-files, which build a warm snapshot with a script run in it.
func (f *ephemeralFlags) addSetupFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.setup, "setup", "", "run this script once in the warm build, so clones start with what it installed")
	cmd.Flags().StringSliceVar(&f.keyFiles, "key-files", nil, "files put in /work for the setup script; a change to one builds a new snapshot (e.g. go.sum,package-lock.json)")
}

func newWarmCreateCommand(flags *globalFlags) *cobra.Command {
	var (
		f       ephemeralFlags
		rebuild bool
	)
	cmd := &cobra.Command{
		Use:   "create <distro>",
		Short: "Build the warm snapshot for a distro and size, unless it exists",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			setup, err := client.LoadSetup(f.setup, f.keyFiles)
			if err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				stream, err := c.Ephemeral.WarmCreate(cmd.Context(), &anvilv1.WarmCreateRequest{
					Distro: args[0], Arch: f.arch, Cpus: f.cpus, MemoryMib: f.memoryMiB, Rebuild: rebuild, Setup: setup,
				})
				if err != nil {
					return err
				}
				var snap *anvilv1.WarmSnapshot
				err = streamStatus(os.Stderr, stream.Recv, func(l *liveLine, ev *anvilv1.WarmCreateProgress) (bool, error) {
					if ev.GetSnapshot() != nil {
						snap = ev.GetSnapshot()
						return true, nil
					}
					l.set(ev.GetStatus())
					return false, nil
				})
				if err != nil {
					return err
				}
				if snap != nil {
					fmt.Printf("%s %s (%s, %d MiB, %s)\n", snap.GetKey(), snap.GetDistro(), format.CPUs(snap.GetCpus()),
						snap.GetMemoryMib(), progress.Bytes(snap.GetSizeBytes()))
				}
				return nil
			})
		},
	}
	f.addSizeFlags(cmd)
	f.addSetupFlags(cmd)
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "build it again even when it exists")
	return cmd
}

func newWarmListCommand(flags *globalFlags) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List warm snapshots",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmList(cmd.Context(), &anvilv1.WarmListRequest{})
				if err != nil {
					return err
				}
				if output == "json" {
					return printJSON(os.Stdout, reply)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "KEY\tDISTRO\tAPP\tSETUP\tARCH\tCPUS\tMEMORY\tSIZE\tPOOL\tLAST USED")
				for _, w := range reply.GetSnapshots() {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d MiB\t%s\t%s\t%s\n", w.GetKey(), w.GetDistro(), warmApp(w), warmSetup(w), w.GetArch(), w.GetCpus(),
						w.GetMemoryMib(), progress.Bytes(w.GetSizeBytes()), warmPool(w), time.Unix(w.GetLastUsedAtUnix(), 0).Format("2006-01-02 15:04"))
				}
				return tw.Flush()
			})
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}

func newWarmRmCommand(flags *globalFlags) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "rm <key>...",
		Aliases: []string{"remove"},
		Short:   "Remove warm snapshots by key (or a unique start of one)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !all {
				return fmt.Errorf("name the warm snapshots to remove, or --all")
			}
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmDelete(cmd.Context(), &anvilv1.WarmDeleteRequest{Keys: args, All: all})
				for _, k := range reply.GetDeleted() {
					fmt.Println("removed", k)
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "remove every warm snapshot")
	return cmd
}

func newWarmSaveCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "save <clone-id>",
		Short: "Turn a running or kept clone into a warm snapshot of its own",
		Long: "Pause the clone, snapshot it and resume it. Clones of the new snapshot start where the clone was: " +
			"run on it with `@<key>` in place of a distro, e.g. `anvil launch --ephemeral @<key> -- make test`. " +
			"Removing the snapshot the clone came from removes this one too.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(flags, func(c *client.Client) error {
				reply, err := c.Ephemeral.WarmSave(cmd.Context(), &anvilv1.WarmSaveRequest{CloneId: args[0]})
				if err != nil {
					return err
				}
				w := reply.GetSnapshot()
				fmt.Printf("%s %s (%s, %s), run it with @%s\n", w.GetKey(), w.GetDistro(), w.GetSetup(), progress.Bytes(w.GetSizeBytes()), w.GetKey())
				return nil
			})
		},
	}
}

// warmSetup is the SETUP column: the script a snapshot was built with, or the clone it was saved from.
func warmSetup(w *anvilv1.WarmSnapshot) string {
	if w.GetSetup() == "" {
		return "-"
	}
	return w.GetSetup()
}

// warmApp is the APP column: the package an `anvil gui --ephemeral` snapshot has, "-" for none.
func warmApp(w *anvilv1.WarmSnapshot) string {
	switch {
	case w.GetApp() == "":
		return "-"
	case w.GetX11():
		return w.GetApp() + " (x11)"
	}
	return w.GetApp()
}
