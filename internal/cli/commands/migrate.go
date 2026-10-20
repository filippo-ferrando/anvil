package commands

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func newMigrateCommand(flags *globalFlags) *cobra.Command {
	var (
		to         string
		copyMode   bool
		destName   string
		dryRun     bool
		bestEffort bool
	)
	cmd := &cobra.Command{
		Use:   "migrate <name|intent> --to <alias|user@host[:port]>",
		Short: "Move a single instance, or a whole intent, to a different anvil host",
		Long: "Move a single instance, or a whole intent as one group, to a different anvil " +
			"host over SSH (see `anvil host`). name is looked up as a single instance first, " +
			"a whole intent second. Default mode is destructive: copies to the target, " +
			"confirms it actually landed, then deletes the source. It never deletes first. " +
			"`--copy` skips that last step. For an intent, default mode is also " +
			"all-or-nothing: the first member that fails to migrate rolls back whatever " +
			"already landed on the target and leaves every source member untouched; " +
			"`--best-effort` keeps whatever succeeds instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return fmt.Errorf("--to is required")
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			// Authorize the target's guest-access key on the source VM before migrating,
			// since a migrated disk skips cloud-init on relaunch.
			if !dryRun {
				warnings, err := c.InjectGuestKeys(cmd.Context(), args[0], to)
				for _, w := range warnings {
					fmt.Fprintln(os.Stderr, "anvil: "+w)
				}
				if err != nil {
					return fmt.Errorf("preparing guest SSH access on the target: %w", err)
				}
			}

			stream, err := c.Migrate.Migrate(cmd.Context(), &anvilv1.MigrateRequest{
				Name:       args[0],
				To:         to,
				Copy:       copyMode,
				DestName:   destName,
				DryRun:     dryRun,
				BestEffort: bestEffort,
			})
			if err != nil {
				return err
			}

			failed := false
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					if failed {
						return fmt.Errorf("migration finished with failures, see above")
					}
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.MigrateProgress_Status:
					fmt.Fprintln(cmd.OutOrStdout(), e.Status)
				case *anvilv1.MigrateProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.MigrateProgress_Done:
					fmt.Fprintf(cmd.OutOrStdout(), "Migrated: new instance %s on %s\n", e.Done, to)
				case *anvilv1.MigrateProgress_MemberDone:
					mr := e.MemberDone
					if mr.GetError() != "" {
						failed = true
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: FAILED: %s\n", mr.GetRole(), mr.GetError())
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s: migrated as %s\n", mr.GetRole(), mr.GetNewId())
					}
				case *anvilv1.MigrateProgress_IntentDone:
					done := e.IntentDone
					if done.GetRolledBack() {
						failed = true
						fmt.Fprintf(cmd.OutOrStdout(), "Rolled back: %q failed to migrate as a group, target cleaned up, source untouched\n", done.GetIntentName())
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "Migrated: intent %q to %s\n", done.GetIntentName(), to)
					}
				}
			}
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "known host alias, or a literal user@host[:port] (required)")
	cmd.Flags().BoolVar(&copyMode, "copy", false, "leave the source instance in place instead of deleting it after a confirmed successful migration")
	cmd.Flags().StringVar(&destName, "dest-name", "", "rename the instance on the target (single instance only, defaults to its current name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check connectivity and report the plan, transfer nothing")
	cmd.Flags().BoolVar(&bestEffort, "best-effort", false, "intent only: keep whatever members succeed instead of rolling back the whole group on any failure")
	return cmd
}
