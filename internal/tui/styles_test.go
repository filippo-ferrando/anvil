package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestHelpBarWrap(t *testing.T) {
	pairs := []string{"n", "launch", "s", "start/stop", "d", "delete", "x", "shell", "e", "exec"}

	t.Run("fits on one line when width is generous", func(t *testing.T) {
		got := helpBarWrap(500, pairs...)
		if strings.Count(got, "\n") != 0 {
			t.Fatalf("expected one line, got %d newlines:\n%s", strings.Count(got, "\n"), got)
		}
	})

	t.Run("wraps to multiple lines that each fit width, dropping nothing", func(t *testing.T) {
		const width = 30
		got := helpBarWrap(width, pairs...)
		lines := strings.Split(got, "\n")
		if len(lines) < 2 {
			t.Fatalf("expected wrapping into multiple lines, got:\n%s", got)
		}
		for _, line := range lines {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("line %q is %d wide, want <= %d", line, w, width)
			}
		}
		for i := 0; i+1 < len(pairs); i += 2 {
			if !strings.Contains(got, pairs[i]) || !strings.Contains(got, pairs[i+1]) {
				t.Errorf("output is missing pair %q/%q:\n%s", pairs[i], pairs[i+1], got)
			}
		}
	})

	t.Run("a non-positive width falls back to the single-line form", func(t *testing.T) {
		if got, want := helpBarWrap(0, pairs...), helpBar(pairs...); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestFriendlyError(t *testing.T) {
	msg, hint := friendlyError("start: rpc error: code = Unavailable desc = connection refused")
	if msg != "start: connection refused" {
		t.Errorf("msg = %q, want the gRPC framing stripped", msg)
	}
	if hint == "" {
		t.Error("expected a hint for Unavailable")
	}
	if msg, hint := friendlyError("invalid host port"); msg != "invalid host port" || hint != "" {
		t.Errorf("plain text changed: %q, %q", msg, hint)
	}
}

func TestScreenForDigit(t *testing.T) {
	want := map[string]screen{"1": screenInstances, "2": screenEphemeral, "5": screenManifests, "6": screenCI,
		"9": screenCloudInit, "0": screenMirrors, "a": screenMigration, "b": screenDevices, "c": screenEvents, "d": screenTasks}
	for k, s := range want {
		if got, ok := screenForDigit(k); !ok || got != s {
			t.Errorf("%s -> %v, %v, want %v", k, got, ok, s)
		}
	}
	for _, k := range []string{"e", "12", "-", ""} {
		if _, ok := screenForDigit(k); ok {
			t.Errorf("%q should not map to a screen", k)
		}
	}
}
