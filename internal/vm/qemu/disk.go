//go:build linux

package qemu

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DiskTuning picks the host-side I/O mode for the VM's main disk.
// The zero value keeps QEMU's defaults (host page cache, thread pool AIO).
type DiskTuning struct {
	DirectIO bool   // cache=none: bypass the host page cache (O_DIRECT)
	AIO      string // "io_uring" | "native" | "" (QEMU default, threads)
}

// driveOpts renders t as extra -drive options.
func (t DiskTuning) driveOpts() string {
	opts := ""
	if t.DirectIO {
		opts += ",cache=none"
	}
	// aio=native is only valid together with O_DIRECT.
	if t.AIO == "io_uring" || (t.AIO == "native" && t.DirectIO) {
		opts += ",aio=" + t.AIO
	}
	return opts
}

var (
	ioUringOnce sync.Once
	ioUringOK   bool
)

// ProbeDiskTuning returns the fastest tuning that works for diskPath on this host.
// Direct I/O is checked per disk (it depends on the filesystem), io_uring once per process.
func ProbeDiskTuning(ctx context.Context, diskPath string) DiskTuning {
	t := DiskTuning{DirectIO: supportsDirectIO(diskPath)}
	if qemuSupportsIOUring(ctx, diskPath) {
		t.AIO = "io_uring"
	} else if t.DirectIO {
		t.AIO = "native"
	}
	return t
}

func supportsDirectIO(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// qemuSupportsIOUring runs one read through qemu-img with aio=io_uring. It fails when
// QEMU is built without liburing or io_uring is disabled by the kernel.
func qemuSupportsIOUring(ctx context.Context, diskPath string) bool {
	ioUringOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "qemu-img", "bench", "-U", "-c", "1", "-i", "io_uring", "-f", "qcow2", diskPath)
		ioUringOK = cmd.Run() == nil
	})
	return ioUringOK
}
