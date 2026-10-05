package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

// TestUploadDirRoundTrip runs the real tar pipeline with the "remote" side on
// this machine, which is the only way to tell the two ends still agree.
func TestUploadDirRoundTrip(t *testing.T) {
	localSSH(t, nil)
	dir := t.TempDir()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "b.txt"), []byte("world"), 0o640); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	if err := uploadDir(context.Background(), target{}, src, dst, func(string) {}); err != nil {
		t.Fatalf("uploadDir: %v", err)
	}
	for path, want := range map[string]string{"a.txt": "hello", "nested/b.txt": "world"} {
		got, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s is %q, want %q", path, got, want)
		}
	}

	// A second run replaces the destination instead of merging into it.
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "nested", "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := uploadDir(context.Background(), target{}, src, dst, func(string) {}); err != nil {
		t.Fatalf("second uploadDir: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "changed" {
		t.Errorf("a.txt is %q after the second run, want %q", got, "changed")
	}
	if _, err := os.Stat(filepath.Join(dst, "nested", "b.txt")); !os.IsNotExist(err) {
		t.Error("expected a removed file to be gone from the destination")
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1024), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 512), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := dirSize(dir); got != 1536 {
		t.Errorf("dirSize = %d, want 1536", got)
	}
	if got := dirSize(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("dirSize of a missing directory = %d, want 0", got)
	}
}

func TestPreflightCountsBindMounts(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.sock"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	spec := &instance.Spec{
		Name: "api",
		Kind: instance.KindContainer,
		Container: &instance.ContainerSpec{ImageRef: "nginx", Engine: instance.ContainerEngineDocker, Volumes: []instance.VolumeMount{
			{HostPath: filepath.Join(dir, "data"), ContainerPath: "/data"},
			{HostPath: filepath.Join(dir, "a.sock"), ContainerPath: "/run/a.sock"},
		}},
	}
	if got := dataDirs(spec); got != 1 {
		t.Errorf("dataDirs = %d, want 1 (only the directory can travel)", got)
	}
	if got := bindMountPaths(spec); got != 2 {
		t.Errorf("bindMountPaths = %d, want 2", got)
	}

	caps := remoteCaps{anvil: "/usr/bin/anvil", anvild: "/usr/bin/anvild", arch: hostArch(), docker: true, tar: true}
	fatal, warn := preflightContainer(spec, caps)
	if len(fatal) != 0 {
		t.Fatalf("expected no failures, got %v", fatal)
	}
	if len(warn) != 2 || !strings.Contains(warn[0], "travel") || !strings.Contains(warn[1], "not directories") {
		t.Errorf("expected a travel and a skipped warning, got %v", warn)
	}
}

func TestPreflightNeedsTarForBindMounts(t *testing.T) {
	dir := t.TempDir()
	spec := &instance.Spec{
		Name: "api",
		Kind: instance.KindContainer,
		Container: &instance.ContainerSpec{ImageRef: "nginx", Engine: instance.ContainerEngineDocker, Volumes: []instance.VolumeMount{
			{HostPath: dir, ContainerPath: "/data"},
		}},
	}
	cannedProbe(t, "arch="+hostArch()+"\nanvil=/usr/bin/anvil\nanvild=/usr/bin/anvild\ndocker\n")
	_, _, err := (&Manager{}).preflight(context.Background(), target{User: "u", Host: "h"}, spec, "api")
	if err == nil || !strings.Contains(err.Error(), "tar is not installed") {
		t.Errorf("expected a missing tar failure, got %v", err)
	}
}
