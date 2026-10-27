package tui

import (
	"slices"
	"testing"
)

func TestGUIArgs(t *testing.T) {
	got := guiArgs("/run/anvil/anvild.sock", "me@far", "re-lab", guiRequest{command: "ghidraRun  -debug", x11: true})
	want := []string{"--socket", "/run/anvil/anvild.sock", "--remote", "me@far", "gui", "--detach", "--x11", "re-lab", "--", "ghidraRun", "-debug"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	got = guiArgs("", "", "dev", guiRequest{command: "gimp", user: "bob", identity: "/k/id"})
	if !slices.Equal(got, []string{"gui", "--detach", "--identity", "/k/id", "--user", "bob", "dev", "--", "gimp"}) {
		t.Fatalf("got %q", got)
	}

	// gui export has no --identity flag, so the identity is left out.
	g := guiRequest{command: "ghidra-gui", x11: true, identity: "/k/id", user: "bob", title: "Ghidra", icon: "/i.png"}
	got = guiExportArgs("", "me@far", "re-lab", g)
	want = []string{"--remote", "me@far", "gui", "export", "--title", "Ghidra", "--icon", "/i.png", "--x11", "--user", "bob", "re-lab", "--", "ghidra-gui"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}
