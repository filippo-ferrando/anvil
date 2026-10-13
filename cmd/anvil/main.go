// Command anvil is the CLI client for anvild.
package main

import (
	"fmt"
	"os"

	"git.fferrando.cc/rdfilippo/anvil/internal/cli/commands"
)

func main() {
	if err := commands.NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "anvil:", err)
		os.Exit(1)
	}
}
