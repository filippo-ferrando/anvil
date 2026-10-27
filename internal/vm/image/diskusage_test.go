package image

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLiveDiskUsageRawAndHeader(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(raw, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, total, err := LiveDiskUsage(raw); err != nil || total != 4096 {
		t.Fatalf("raw: total = %d, err = %v, want 4096", total, err)
	}

	hdr := make([]byte, 512)
	copy(hdr, qcow2Magic)
	binary.BigEndian.PutUint64(hdr[24:], 10<<30)
	qcow := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(qcow, hdr, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, total, err := LiveDiskUsage(qcow); err != nil || total != 10<<30 {
		t.Fatalf("qcow2: total = %d, err = %v, want 10GiB", total, err)
	}
}

// LiveDiskUsage must agree with qemu-img on a real qcow2.
func TestLiveDiskUsageMatchesQemuImg(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not on PATH")
	}
	path := filepath.Join(t.TempDir(), "disk.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, "3G").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %v: %s", err, out)
	}
	wantUsed, wantTotal, err := DiskUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	used, total, err := LiveDiskUsage(path)
	if err != nil {
		t.Fatal(err)
	}
	if used != wantUsed || total != wantTotal {
		t.Fatalf("LiveDiskUsage = %d/%d, qemu-img says %d/%d", used, total, wantUsed, wantTotal)
	}
}
