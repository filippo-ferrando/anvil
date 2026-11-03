// Command anvil is the CLI client for anvild.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"git.fferrando.cc/rdfilippo/anvil/internal/cli/commands"
)

func main() {
	if err := commands.NewRootCommand().Execute(); err != nil {
		var exit commands.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		// shell, exec, transfer and gui run ssh or waypipe: pass its exit code through.
		var child *exec.ExitError
		if errors.As(err, &child) && child.ExitCode() > 0 {
			os.Exit(child.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "anvil:", err)
		os.Exit(1)
	}
}
