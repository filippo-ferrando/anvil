package commands

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// newManCommand generates a man page for every anvil subcommand into a
// directory, one file per command. It's used by the packaging scripts
// (packaging/archlinux/PKGBUILD, packaging/deb/build.sh,
// packaging/rpm/build.sh, and `make man`) to ship man pages without
// hand-writing one per subcommand; not meant for end users, so it's
// hidden from `anvil --help`, same as it not getting a TUI or gRPC
// surface of its own.
func newManCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:    "man <dir>",
		Short:  "Generate man pages for the anvil command tree into a directory",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			header := &doc.GenManHeader{
				Title:   "ANVIL",
				Section: "1",
				Source:  "Anvil",
				Manual:  "Anvil Manual",
			}
			if err := doc.GenManTree(root, header, args[0]); err != nil {
				return fmt.Errorf("generating man pages: %w", err)
			}
			return nil
		},
	}
}
