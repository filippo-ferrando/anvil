//go:build !linux

// anvil-guest only runs inside Linux guests.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "anvil-guest: only runs inside a Linux guest")
	os.Exit(1)
}
