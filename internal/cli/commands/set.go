package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newSetCommand(flags *globalFlags) *cobra.Command {
	var (
		cpus      int32
		memoryMiB int64
		diskGiB   int64
		autostart bool
		restart   string
	)
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Change an existing instance's resources, autostart or restart policy",
		Long: "Change an existing instance's settings; only the flags given are changed. " +
			"--disk only grows, and applies right away, live on a running VM (with the guest " +
			"agent, the guest filesystem grows too). --cpus/--memory of a running x86_64 VM " +
			"change live, up to the host's CPU count and memory; memory can't go below what it " +
			"booted with while running. Anything that can't be applied live says so and applies " +
			"at the next start. --restart takes no, on-failure[:N] or always.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &anvilv1.UpdateRequest{Name: args[0]}
			f := cmd.Flags()
			if f.Changed("cpus") {
				req.Cpus = &cpus
			}
			if f.Changed("memory") {
				req.MemoryMib = &memoryMiB
			}
			if f.Changed("disk") {
				req.DiskGib = &diskGiB
			}
			if f.Changed("autostart") {
				req.Autostart = &autostart
			}
			if f.Changed("restart") {
				req.RestartPolicy = &anvilv1.RestartPolicy{Mode: restart}
			}
			if req.Cpus == nil && req.MemoryMib == nil && req.DiskGib == nil && req.Autostart == nil && req.RestartPolicy == nil {
				return fmt.Errorf("nothing to change: pass at least one of --cpus, --memory, --disk, --autostart, --restart")
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			reply, err := c.Update(cmd.Context(), req)
			if err != nil {
				return err
			}
			for _, note := range reply.GetNotes() {
				fmt.Fprintln(cmd.OutOrStdout(), note)
			}
			if len(reply.GetNotes()) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing changed")
			}
			return nil
		},
	}
	cmd.Flags().Int32Var(&cpus, "cpus", 0, "number of vCPUs (VM only)")
	cmd.Flags().Int64Var(&memoryMiB, "memory", 0, "memory in MiB (VM only)")
	cmd.Flags().Int64Var(&diskGiB, "disk", 0, "grow the disk to this many GiB (VM only)")
	cmd.Flags().BoolVar(&autostart, "autostart", false, "start the instance when anvild starts (--autostart=false turns it off)")
	cmd.Flags().StringVar(&restart, "restart", "", "restart policy: no, on-failure[:N] or always")
	return cmd
}
