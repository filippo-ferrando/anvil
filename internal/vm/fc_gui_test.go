//go:build linux

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

func TestFCToolsISO(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not installed, skipping")
	}
	tools := t.TempDir()
	t.Setenv("ANVIL_GUI_TOOLS", tools)
	waypipe := filepath.Join(tools, "waypipe")
	if err := os.WriteFile(waypipe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &FCBackend{Cache: &image.FCCache{Dir: t.TempDir()}}

	iso, err := b.toolsISO()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("xorriso", "-indev", iso, "-pvd_info").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "anviltools") {
		t.Fatalf("ISO label: %v\n%s", err, out)
	}
	first, _ := os.Stat(iso)

	if _, err := b.toolsISO(); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(iso); !again.ModTime().Equal(first.ModTime()) {
		t.Fatal("the ISO was rebuilt with no tool changed")
	}
	later := first.ModTime().Add(time.Minute)
	if err := os.Chtimes(waypipe, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := b.toolsISO(); err != nil {
		t.Fatal(err)
	}
	if rebuilt, _ := os.Stat(iso); rebuilt.ModTime().Equal(first.ModTime()) {
		t.Fatal("the ISO wasn't rebuilt after a tool changed")
	}
}
