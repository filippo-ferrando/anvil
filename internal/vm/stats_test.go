//go:build linux

package vm

import (
	"os"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
)

func TestReadProcCPUTicksOnRealProcess(t *testing.T) {
	// Burn a bit of real CPU so utime/stime is guaranteed non-zero, then
	// read this test binary's own /proc entry: a real process, no mocking.
	sum := 0
	for i := 0; i < 100_000_000; i++ {
		sum += i
	}
	if sum == 0 {
		t.Fatal("unreachable, just here to stop the compiler eliding the loop")
	}

	ticks, err := readProcCPUTicks(os.Getpid())
	if err != nil {
		t.Fatalf("readProcCPUTicks: %v", err)
	}
	if ticks == 0 {
		t.Error("expected non-zero CPU ticks for a process that just did real work")
	}
}

func TestReadProcCPUTicksRejectsUnknownPid(t *testing.T) {
	if _, err := readProcCPUTicks(1 << 30); err == nil {
		t.Error("expected an error for a pid that can't exist")
	}
}

func TestReadRSSBytesOnRealProcess(t *testing.T) {
	rss, err := readRSSBytes(os.Getpid())
	if err != nil {
		t.Fatalf("readRSSBytes: %v", err)
	}
	if rss <= 0 {
		t.Errorf("expected a positive RSS for the running test process, got %d", rss)
	}
}

func TestVMAddressBridgeMode(t *testing.T) {
	v := &instance.VMSpec{NetworkMode: "bridge", StaticIP: "10.55.201.4/24"}
	if got := vmAddress(v); got != "10.55.201.4" {
		t.Errorf("expected the bare bridge IP, got %q", got)
	}
}

func TestVMAddressSLIRPMode(t *testing.T) {
	v := &instance.VMSpec{SSHPort: 2222}
	if got := vmAddress(v); got != "127.0.0.1:2222" {
		t.Errorf("expected the loopback SSH-forward address, got %q", got)
	}
}

func TestVMAddressUnknown(t *testing.T) {
	v := &instance.VMSpec{}
	if got := vmAddress(v); got != "" {
		t.Errorf("expected an empty address when nothing is known, got %q", got)
	}
}
