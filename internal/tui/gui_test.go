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

func TestEphemeralAppArgs(t *testing.T) {
	a := appRequest{distro: "debian-13", pkg: "libreoffice", command: "libreoffice --writer", x11: true, memory: 4096}
	got := ephemeralAppArgs("/s", []string{"gui", "--detach"}, a)
	want := []string{"--socket", "/s", "gui", "--detach", "--ephemeral", "--x11", "--memory", "4096", "debian-13", "libreoffice", "--", "libreoffice", "--writer"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
	got = ephemeralAppArgs("", []string{"gui", "export"}, appRequest{distro: "fedora-42", pkg: "gimp"})
	if !slices.Equal(got, []string{"gui", "export", "--ephemeral", "fedora-42", "gimp"}) {
		t.Fatalf("got %q", got)
	}
	if lastLine("building\nExported /x.desktop\n") != "Exported /x.desktop" {
		t.Fatal("lastLine")
	}
}
