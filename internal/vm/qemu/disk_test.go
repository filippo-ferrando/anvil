//go:build linux

package qemu

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestProbeDiskTuning(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}
	diskPath := filepath.Join(t.TempDir(), "disk.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, "16M").CombinedOutput(); err != nil {
		t.Fatalf("creating disk: %v: %s", err, out)
	}
	tuning := ProbeDiskTuning(context.Background(), diskPath)
	t.Logf("probed tuning: %+v", tuning)
	if tuning.AIO == "native" && !tuning.DirectIO {
		t.Error("aio=native must only be picked together with direct I/O")
	}
}

func TestSupportsDirectIOMissingFile(t *testing.T) {
	if supportsDirectIO(filepath.Join(t.TempDir(), "missing")) {
		t.Error("expected false for a missing file")
	}
}
