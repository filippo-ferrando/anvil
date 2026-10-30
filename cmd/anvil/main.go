// Command anvil is the CLI client for anvild.
package main

import (
	"errors"
	"fmt"
	"os"

	"git.fferrando.cc/rdfilippo/anvil/internal/cli/commands"
)

func main() {
	if err := commands.NewRootCommand().Execute(); err != nil {
		var exit commands.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, "anvil:", err)
		os.Exit(1)
	}
}
