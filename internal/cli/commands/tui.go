package commands

import (
	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/tui"
)

// newTuiCommand launches the terminal UI using the shared daemon socket flag.
func newTuiCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Launch the terminal UI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(flags.socket)
		},
	}
}
