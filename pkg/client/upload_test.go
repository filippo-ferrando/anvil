package client

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func tarNames(t *testing.T, dir string) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteUploadTar(dir, &buf); err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

func TestUploadTar(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"main.go": "package main", "build/out.bin": "x", "src/a.txt": "a", ".gitignore": "build/\n"} {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := tarNames(t, dir); !slices.Contains(got, "build/out.bin") || !slices.Contains(got, "src") {
		t.Errorf("outside git everything goes: %v", got)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	got := tarNames(t, dir)
	want := []string{".gitignore", "main.go", "src/a.txt"} // untracked but not ignored files come too
	if !slices.Equal(got, want) {
		t.Errorf("in a git repo = %v, want %v", got, want)
	}
}
