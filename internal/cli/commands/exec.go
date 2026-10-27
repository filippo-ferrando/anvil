package commands

import (
	"github.com/spf13/cobra"
)

func newExecCommand(flags *globalFlags) *cobra.Command {
	var f sshFlags
	cmd := &cobra.Command{
		Use:   "exec <name> -- <command> [args...]",
		Short: "Run a command inside a VM (over SSH) or container (via docker/podman exec)",
		Long: "Run a command inside a VM or container. Put `--` before the remote command if it " +
			"has its own flags, same as `ssh host -- cmd --flag`, otherwise cobra will try to " +
			"parse those flags as anvil's own.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInInstance(cmd, flags, f, args[0], args[1:])
		},
	}
	f.add(cmd)
	return cmd
}
