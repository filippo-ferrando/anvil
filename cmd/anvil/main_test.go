package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoTerminalQuery runs the real binary on a terminal (through script) and checks it
// doesn't ask for the background color: termbg's init must still run before bubbletea's.
func TestNoTerminalQuery(t *testing.T) {
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script not installed")
	}
	bin := filepath.Join(t.TempDir(), "anvil")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	out, err := exec.Command("script", "-qec", bin+" --help", "/dev/null").CombinedOutput()
	if err != nil {
		t.Fatalf("anvil --help: %v: %s", err, out)
	}
	if strings.Contains(string(out), "\x1b]11;?") {
		t.Errorf("anvil asked the terminal for its background color:\n%q", out)
	}
}
