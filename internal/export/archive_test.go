package export

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWriteExtractTarZstRoundTrip builds a real bundle (manifest bytes, a
// single file, and a directory tree) and confirms extractTarZst recovers
// it byte-for-byte.
func TestWriteExtractTarZstRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed, skipping")
	}

	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "vol", "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "vol", "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "vol", "nested", "b.txt"), []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(srcDir, "disk.qcow2")
	if err := os.WriteFile(diskPath, []byte("fake qcow2 bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	bundlePath := filepath.Join(t.TempDir(), "bundle.tar.zst")
	ctx := context.Background()
	err := writeTarZst(ctx, bundlePath, func(tw *tar.Writer) error {
		if err := addBytesToTar(tw, "manifest.json", []byte(`{"IntentName":"demo"}`)); err != nil {
			return err
		}
		if err := addFileToTar(tw, "disks/x.qcow2", diskPath); err != nil {
			return err
		}
		return addDirToTar(tw, "volumes/x/0", filepath.Join(srcDir, "vol"))
	})
	if err != nil {
		t.Fatalf("writeTarZst: %v", err)
	}

	destDir := t.TempDir()
	if err := extractTarZst(ctx, bundlePath, destDir); err != nil {
		t.Fatalf("extractTarZst: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(destDir, "manifest.json"))
	if err != nil || string(manifest) != `{"IntentName":"demo"}` {
		t.Fatalf("manifest.json = %q, %v", manifest, err)
	}
	disk, err := os.ReadFile(filepath.Join(destDir, "disks/x.qcow2"))
	if err != nil || string(disk) != "fake qcow2 bytes" {
		t.Fatalf("disks/x.qcow2 = %q, %v", disk, err)
	}
	a, err := os.ReadFile(filepath.Join(destDir, "volumes/x/0/a.txt"))
	if err != nil || string(a) != "hello" {
		t.Fatalf("volumes/x/0/a.txt = %q, %v", a, err)
	}
	b, err := os.ReadFile(filepath.Join(destDir, "volumes/x/0/nested/b.txt"))
	if err != nil || string(b) != "world" {
		t.Fatalf("volumes/x/0/nested/b.txt = %q, %v", b, err)
	}
}

// TestExtractAllRejectsPathTraversal confirms a maliciously crafted "../"
// entry name is contained inside destDir instead of escaping it.
func TestExtractAllRejectsPathTraversal(t *testing.T) {
	destDir := t.TempDir()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(pw)
	go func() {
		_ = tw.WriteHeader(&tar.Header{Name: "../../evil.txt", Mode: 0o640, Size: 4})
		_, _ = tw.Write([]byte("evil"))
		_ = tw.Close()
		_ = pw.Close()
	}()

	if err := extractAll(tar.NewReader(pr), destDir); err != nil {
		t.Fatalf("extractAll: %v", err)
	}

	if _, err := os.Stat(filepath.Join(destDir, "evil.txt")); err != nil {
		t.Fatalf("expected evil.txt contained inside destDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(destDir), "evil.txt")); err == nil {
		t.Fatal("path traversal escaped destDir")
	}
}
