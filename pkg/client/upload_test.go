package client

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
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

// A tree of many small files goes out in full chunks, not one per header and file.
func TestSendUploadFullChunks(t *testing.T) {
	dir := t.TempDir()
	for i := range 300 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), bytes.Repeat([]byte("x"), 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var chunks [][]byte
	if err := SendUpload(dir, func(c []byte) error { chunks = append(chunks, c); return nil }); err != nil {
		t.Fatal(err)
	}
	// 300 entries of 512 + 1024 bytes and the end blocks: about 450 KiB, two chunks.
	if len(chunks) != 2 || len(chunks[0]) != UploadChunkSize {
		t.Fatalf("%d chunks (first %d bytes), want 2 with a full first one", len(chunks), len(chunks[0]))
	}
	tr := tar.NewReader(bytes.NewReader(slices.Concat(chunks...)))
	n := 0
	for {
		if _, err := tr.Next(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 300 {
		t.Errorf("tar holds %d files, want 300", n)
	}

	// A failed send stops the upload, and the tar writer with it.
	sent := 0
	err := SendUpload(dir, func([]byte) error { sent++; return errors.New("stream closed") })
	if err == nil || sent != 1 {
		t.Errorf("SendUpload = %v after %d sends, want the send error after one", err, sent)
	}
}
