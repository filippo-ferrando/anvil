package commands

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newMigrateCommand(flags *globalFlags) *cobra.Command {
	var (
		to       string
		copyMode bool
		destName string
		dryRun   bool
	)
	cmd := &cobra.Command{
		Use:   "migrate <name> --to <alias|user@host[:port]>",
		Short: "Move a single instance to a different anvil host",
		Long: "Move a single instance to a different anvil host over SSH (see `anvil host`). " +
			"Default mode is destructive: copies the instance to the target, confirms the " +
			"target actually has it, then deletes the source — never deletes the source " +
			"first. `--copy` skips that last step. A whole intent isn't supported yet, " +
			"single instances only.",
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

			stream, err := c.Migrate.Migrate(cmd.Context(), &anvilv1.MigrateRequest{
				Name:     args[0],
				To:       to,
				Copy:     copyMode,
				DestName: destName,
				DryRun:   dryRun,
			})
			if err != nil {
				return err
			}
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
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
				}
			}
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "known host alias, or a literal user@host[:port] (required)")
	cmd.Flags().BoolVar(&copyMode, "copy", false, "leave the source instance in place instead of deleting it after a confirmed successful migration")
	cmd.Flags().StringVar(&destName, "dest-name", "", "rename the instance on the target (defaults to its current name)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "check connectivity and report the plan, transfer nothing")
	return cmd
}
