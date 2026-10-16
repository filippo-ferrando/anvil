package hostpath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveDirRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("mine"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("theirs"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := MoveDir(src, dst); err == nil {
		t.Fatal("expected MoveDir to refuse an existing destination")
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(data) != "mine" {
		t.Errorf("destination was overwritten: %q", data)
	}
}

func TestResolveAndCheckResolved(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "data")
	if err := os.Mkdir(real, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	got, uid, err := Resolve(link)
	if err != nil {
		t.Fatal(err)
	}
	wantReal, _ := filepath.EvalSymlinks(real)
	if got != wantReal || int(uid) != os.Getuid() {
		t.Errorf("Resolve(link) = %s, uid %d; want %s, uid %d", got, uid, wantReal, os.Getuid())
	}
	if err := CheckResolved(got); err != nil {
		t.Errorf("an unchanged path should pass: %v", err)
	}

	// The checked directory is swapped for a symlink to somewhere else.
	if err := os.Rename(got, got+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.TempDir(), got); err != nil {
		t.Fatal(err)
	}
	if err := CheckResolved(got); err == nil {
		t.Error("expected a swapped path to be refused")
	}
}
