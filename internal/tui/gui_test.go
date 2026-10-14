package tui

import (
	"slices"
	"testing"
)

func TestGUIArgs(t *testing.T) {
	got := guiArgs("/run/anvil/anvild.sock", "me@far", "re-lab", "ghidraRun  -debug", true)
	want := []string{"--socket", "/run/anvil/anvild.sock", "--remote", "me@far", "gui", "--detach", "--x11", "re-lab", "--", "ghidraRun", "-debug"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	got = guiArgs("", "", "dev", "gimp", false)
	if !slices.Equal(got, []string{"gui", "--detach", "dev", "--", "gimp"}) {
		t.Fatalf("got %q", got)
	}

	got = guiExportArgs("", "me@far", "re-lab", "ghidra-gui", true)
	want = []string{"--remote", "me@far", "gui", "export", "--x11", "re-lab", "--", "ghidra-gui"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}
