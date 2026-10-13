package commands

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStartGUIRelays(t *testing.T) {
	runtime := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(runtime, "wayland-7"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("WAYLAND_DISPLAY", "wayland-7")

	relayDir := t.TempDir()
	env, relays, cleanup, err := startGUIRelays(relayDir, filepath.Join(relayDir, "x11"), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"XDG_RUNTIME_DIR=/run/anvil-gui", "WAYLAND_DISPLAY=wayland-0"}
	if !slices.Equal(env, want) || len(relays) != 1 {
		t.Fatalf("env %q relays %d", env, len(relays))
	}

	// A second session on the same instance gets its own socket.
	env2, _, cleanup2, err := startGUIRelays(relayDir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if env2[1] != "WAYLAND_DISPLAY=wayland-1" {
		t.Fatalf("second session env %q", env2)
	}
	cleanup2()
	cleanup()
	if entries, _ := os.ReadDir(relayDir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}
