package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompletePathMatches(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bundle.tar.zst", "bundle2.tar.zst", "other.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}

	t.Run("filters by prefix and hides dotfiles", func(t *testing.T) {
		got := completePathMatches(filepath.Join(dir, "bundle"))
		want := []string{
			filepath.Join(dir, "bundle.tar.zst"),
			filepath.Join(dir, "bundle2.tar.zst"),
		}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("directories get a trailing slash", func(t *testing.T) {
		got := completePathMatches(filepath.Join(dir, "sub"))
		want := []string{filepath.Join(dir, "subdir") + "/"}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("an explicit dot prefix reveals dotfiles", func(t *testing.T) {
		got := completePathMatches(filepath.Join(dir, ".hid"))
		want := []string{filepath.Join(dir, ".hidden")}
		if !equalStrings(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("an unreadable directory yields no matches, not an error", func(t *testing.T) {
		if got := completePathMatches("/no/such/directory/prefix"); got != nil {
			t.Fatalf("got %v, want nil", got)
		}
	})
}
