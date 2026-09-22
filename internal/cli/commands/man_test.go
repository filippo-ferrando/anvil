package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManCommand_GeneratesPageTree(t *testing.T) {
	dir := t.TempDir()
	root := NewRootCommand()
	root.SetArgs([]string{"man", dir})
	if err := root.Execute(); err != nil {
		t.Fatalf("anvil man %s: %v", dir, err)
	}

	// The root command and a real subcommand should each get their own
	// page; the hidden `man` command itself shouldn't (see man.go).
	for _, want := range []string{"anvil.1", "anvil-launch.1"} {
		info, err := os.Stat(filepath.Join(dir, want))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", want, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "anvil-man.1")); err == nil {
		t.Fatal("anvil-man.1 should not be generated, `man` is a hidden command")
	}
}
