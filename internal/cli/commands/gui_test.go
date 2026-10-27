package commands

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestWaypipeArgs(t *testing.T) {
	ssh := []string{"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k"}
	got := waypipeArgs(ssh, "ubuntu@localhost", "/opt/anvil", true, []string{"ghidraRun", "--flag"})
	want := []string{"--remote-bin", "/opt/anvil/waypipe", "--xwls", "ssh",
		"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k",
		"ubuntu@localhost", "env", "_JAVA_AWT_WM_NONREPARENTING=1", "ghidraRun", "--flag"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := waypipeArgs(nil, "u@h", "/opt/anvil", false, []string{"app"}); slices.Contains(got, "--xwls") || slices.Contains(got, "env") {
		t.Fatalf("x11 settings without x11: %q", got)
	}
}

func TestWithoutDetach(t *testing.T) {
	got := withoutDetach([]string{"--remote", "h", "gui", "--detach", "--x11", "dev", "--", "app", "--detach"})
	want := []string{"--remote", "h", "gui", "--x11", "dev", "--", "app", "--detach"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestGUILogPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if p, _ := guiLogPath("dev"); p != "/state/anvil/gui/dev.log" {
		t.Fatalf("got %s", p)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/me")
	if p, _ := guiLogPath("dev"); p != filepath.Join("/home/me", ".local/state/anvil/gui/dev.log") {
		t.Fatalf("got %s", p)
	}
}
