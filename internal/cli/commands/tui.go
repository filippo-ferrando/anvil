package commands

import (
	"github.com/spf13/cobra"

	"git.fferrando.cc/rdfilippo/anvil/internal/tui"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// newTuiCommand launches the terminal UI using the shared daemon socket flag.
func newTuiCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch the terminal UI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(func() (*client.Client, error) { return dial(flags) }, flags.socket)
		},
	}
}
