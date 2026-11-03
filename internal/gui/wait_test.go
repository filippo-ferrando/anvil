package gui

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A launcher that forks the app and exits right away must not end the session.
func TestWaitArgvWaitsForBackgroundChild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	done := filepath.Join(t.TempDir(), "done")
	launcher := `(sleep 2; touch "$1") >/dev/null 2>&1 & exit 3`
	argv := WaitArgv([]string{"sh", "-c", launcher, "sh", done})
	start := time.Now()
	err := exec.Command(argv[0], argv[1:]...).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("want the launcher's exit code 3, got %v", err)
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("returned after %v, before the background child finished", time.Since(start))
	}
}

func TestWaitShellCommand(t *testing.T) {
	line := WaitShellCommand([]string{"printf", "%s|", "it's", "a b"})
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil || string(out) != "it's|a b|" {
		t.Fatalf("got %q %v", out, err)
	}
}
