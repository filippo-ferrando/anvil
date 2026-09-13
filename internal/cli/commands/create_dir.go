package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/hostpath"
)

// newCreateDirCommand is `anvil create-dir`: makes a host directory and
// grants anvild's own unprivileged "anvil" system user access to it, so
// a later `anvil mount` on the same path doesn't hit the permission-denied
// error hostpath.Hint describes. This runs entirely client-side (mkdir +
// setfacl are local filesystem operations the invoking user can already
// do), not through anvild — there's no RPC involved.
func newCreateDirCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "create-dir <path>",
		Short: "Create a host directory and prepare it for anvil mount",
		Long: "Create a host directory (if it doesn't already exist) and grant the " +
			"\"anvil\" system user access to it, so it's ready for `anvil mount` right " +
			"away. Without this, mounting an ordinary directory you own usually fails " +
			"with permission denied, since anvild runs as its own unprivileged user, " +
			"not root and not you.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			if err := os.MkdirAll(path, 0o755); err != nil {
				return fmt.Errorf("create-dir: %w", err)
			}
			if err := hostpath.Grant(path); err != nil {
				return fmt.Errorf("create-dir: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is ready to mount\n", path)
			return nil
		},
	}
}
