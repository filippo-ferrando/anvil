//go:build linux

package guestexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestReapHelper is the anvil-guest stand-in Reap tests run in a child process.
func TestReapHelper(t *testing.T) {
	if os.Getenv("ANVIL_REAP_HELPER") != "1" {
		t.Skip("helper process")
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Exit(Reap(os.Args[i+1:]))
		}
	}
	os.Exit(2)
}

// A launcher that daemonizes the app and exits right away must not end the run early.
func TestReapWaitsForOrphans(t *testing.T) {
	done := filepath.Join(t.TempDir(), "done")
	launcher := `setsid sh -c 'sleep 1; touch "$1"' sh "$1" >/dev/null 2>&1 & exit 3`
	cmd := exec.Command(os.Args[0], "-test.run=^TestReapHelper$", "--", "sh", "-c", launcher, "sh", done)
	cmd.Env = append(os.Environ(), "ANVIL_REAP_HELPER=1")
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("want the launcher's exit code 3, got %v", err)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatal("returned before the daemonized child finished")
	}
}
