package commands

import (
	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/tui"
)

// newTuiCommand is `anvil tui` (M8): launches the terminal UI. Same
// socket flag as every other command, no separate config — see
// internal/tui's own doc comment for what it covers and its one honest
// caveat (untested against a real terminal in this sandbox).
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
