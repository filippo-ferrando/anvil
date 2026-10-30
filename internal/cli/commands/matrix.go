package commands

import (
	"strings"

	"github.com/spf13/cobra"
)

func newMatrixCommand(flags *globalFlags) *cobra.Command {
	var f ephemeralFlags
	cmd := &cobra.Command{
		Use:   "matrix <distro>,<distro>... [path] -- <command> [args...]",
		Short: "Run a command on throwaway clones of several distros at once",
		Long: "Restore one clone per distro from its warm snapshot (built first when there is none), " +
			"all in parallel, upload path (honoring .gitignore in a git repo) into /work, run the command " +
			"and delete the clones. Each line of output carries its distro. Exits non-zero when any clone " +
			"failed. Needs Firecracker with its jailer, see docs/ephemeral.md.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			list, path, command, err := splitRunArgs(cmd, args)
			if err != nil {
				return err
			}
			var distros []string
			for _, d := range strings.Split(list, ",") {
				if d = strings.TrimSpace(d); d != "" {
					distros = append(distros, d)
				}
			}
			return runEphemeral(cmd, flags, distros, path, command, f)
		},
	}
	f.addSizeFlags(cmd)
	f.addRunFlags(cmd)
	cmd.Flags().StringArrayVarP(&f.env, "env", "e", nil, "environment variable KEY=VALUE for the command (repeatable)")
	return cmd
}
