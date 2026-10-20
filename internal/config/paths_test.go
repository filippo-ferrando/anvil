package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreeDirSkipsTakenPaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "0")
	if got := FreeDir(base); got != base {
		t.Fatalf("FreeDir on a free path = %q, want %q", got, base)
	}
	for _, p := range []string{base, base + "-2"} {
		if err := os.Mkdir(p, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if got := FreeDir(base); got != base+"-3" {
		t.Errorf("FreeDir = %q, want %q", got, base+"-3")
	}
}
