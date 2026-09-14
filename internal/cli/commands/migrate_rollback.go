package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// newMigrateRollbackCommand reads a JSON array of instance names from
// stdin and force-deletes them through the local anvild.
func newMigrateRollbackCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:    "migrate-rollback",
		Short:  "Internal: delete instances left by a failed intent migration",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("migrate-rollback: reading names: %w", err)
			}
			var names []string
			if err := json.Unmarshal(data, &names); err != nil {
				return fmt.Errorf("migrate-rollback: invalid names payload: %w", err)
			}
			if len(names) == 0 {
				return nil
			}

			c, err := dial(flags)
			if err != nil {
				return fmt.Errorf("migrate-rollback: dialing local anvild: %w", err)
			}
			defer c.Close()

			_, err = c.Delete(cmd.Context(), &anvilv1.DeleteRequest{Names: names, Purge: true})
			return err
		},
	}
}
