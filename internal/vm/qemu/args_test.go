//go:build linux

package qemu

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildArgsRequiresDiskAndSocket(t *testing.T) {
	if _, err := BuildArgs(Config{}); err == nil {
		t.Fatal("expected error for missing DiskPath/QMPSocket")
	}
	if _, err := BuildArgs(Config{DiskPath: "/x"}); err == nil {
		t.Fatal("expected error for missing QMPSocket")
	}
}

func TestBuildArgsSLIRPDefault(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:  "/var/lib/anvil/instances/x/disk.qcow2",
		QMPSocket: "/run/anvil/x.qmp",
		CPUs:      2,
		MemoryMiB: 2048,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-netdev user,id=net0") {
		t.Errorf("expected SLIRP netdev, got: %s", joined)
	}
	if strings.Contains(joined, "-vnc") || strings.Contains(joined, "-display") {
		t.Errorf("expected no graphical display device, got: %s", joined)
	}
	if !strings.Contains(joined, "-nographic") {
		t.Errorf("expected -nographic, got: %s", joined)
	}
}

func TestBuildArgsSLIRPHostForward(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:  "/d",
		QMPSocket: "/q",
		SLIRPHostForwards: []HostForward{
			{HostPort: 2222, GuestPort: 22, Protocol: "tcp"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "hostfwd=tcp::2222-:22") {
		t.Errorf("expected hostfwd entry, got: %s", joined)
	}
}

func TestBuildArgsBridgeTap(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:        "/d",
		QMPSocket:       "/q",
		BridgeTapDevice: "anvil-tap0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "ifname=anvil-tap0") {
		t.Errorf("expected tap device attachment, got: %s", joined)
	}
	if strings.Contains(joined, "-netdev user") {
		t.Errorf("bridge mode should not also configure SLIRP, got: %s", joined)
	}
}

func TestBuildArgsBridgeTapWithMAC(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:        "/d",
		QMPSocket:       "/q",
		BridgeTapDevice: "anvil-tap0",
		MACAddress:      "52:54:00:ab:cd:ef",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "virtio-net-pci,netdev=net0,mac=52:54:00:ab:cd:ef") {
		t.Errorf("expected the NIC device to carry the given MAC, got: %s", joined)
	}
}

func TestBuildArgsBridgeTapWithoutMAC(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:        "/d",
		QMPSocket:       "/q",
		BridgeTapDevice: "anvil-tap0",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "mac=") {
		t.Errorf("expected no mac= suffix when MACAddress is unset, got: %s", joined)
	}
}

func TestBuildArgsSerialLog(t *testing.T) {
	noLog, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.Join(noLog, " "), "-serial null") {
		t.Errorf("expected -serial null when SerialLogPath is unset, got: %s", strings.Join(noLog, " "))
	}

	withLog, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", SerialLogPath: "/tmp/console.log"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(withLog, " ")
	if !strings.Contains(joined, "-serial file:/tmp/console.log") {
		t.Errorf("expected -serial file:/tmp/console.log, got: %s", joined)
	}
}

func TestBuildArgsMounts(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:  "/d",
		QMPSocket: "/q",
		MemoryMiB: 2048,
		Mounts: []Mount{
			{Tag: "mount0", SocketPath: "/run/x/fs-mount0.sock"},
			{Tag: "mount3", SocketPath: "/run/x/fs-mount3.sock"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-machine q35,memory-backend=mem0",
		"-object memory-backend-memfd,id=mem0,size=2048M,share=on",
		"-device pcie-root-port,id=hp0,chassis=1",
		"-device pcie-root-port,id=hp7,chassis=8",
		"-chardev socket,id=fsc-mount0,path=/run/x/fs-mount0.sock",
		"-device vhost-user-fs-pci,id=fs-mount0,chardev=fsc-mount0,tag=mount0,queue-size=1024,bus=hp0",
		"-device vhost-user-fs-pci,id=fs-mount3,chardev=fsc-mount3,tag=mount3,queue-size=1024,bus=hp1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %s", want, joined)
		}
	}
}

func TestBuildArgsTooManyMounts(t *testing.T) {
	mounts := make([]Mount, HotplugPorts+1)
	for i := range mounts {
		mounts[i] = Mount{Tag: fmt.Sprintf("mount%d", i), SocketPath: "/s"}
	}
	if _, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", Mounts: mounts}); err == nil {
		t.Error("expected an error with more mounts than hot-plug ports")
	}
}

func TestBuildArgsNoMountsMeansNoVirtiofsDevice(t *testing.T) {
	args, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "vhost-user-fs-pci") {
		t.Error("expected no virtiofs device when Mounts is empty")
	}
	if !strings.Contains(joined, "pcie-root-port,id=hp0") {
		t.Error("expected hot-plug ports even without mounts, so a mount can be added live")
	}
}

func TestBuildArgsSeedISO(t *testing.T) {
	args, err := BuildArgs(Config{
		DiskPath:    "/d",
		QMPSocket:   "/q",
		SeedISOPath: "/seed.iso",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/seed.iso") {
		t.Errorf("expected seed ISO drive, got: %s", joined)
	}
}

func TestBuildArgsKVMvsTCG(t *testing.T) {
	kvmArgs, _ := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", KVM: true})
	if !strings.Contains(strings.Join(kvmArgs, " "), "-accel kvm") {
		t.Errorf("expected kvm accel")
	}
	tcgArgs, _ := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", KVM: false})
	if !strings.Contains(strings.Join(tcgArgs, " "), "-accel tcg") {
		t.Errorf("expected tcg fallback")
	}
}

func TestBuildArgsDiskTuning(t *testing.T) {
	cases := []struct {
		name    string
		tuning  DiskTuning
		want    []string
		wantNot []string
	}{
		{"defaults", DiskTuning{}, []string{"discard=unmap,detect-zeroes=unmap"}, []string{"cache=none", "aio="}},
		{"direct io_uring", DiskTuning{DirectIO: true, AIO: "io_uring"}, []string{"cache=none,aio=io_uring"}, nil},
		{"native needs direct", DiskTuning{AIO: "native"}, nil, []string{"aio=native"}},
		{"direct native", DiskTuning{DirectIO: true, AIO: "native"}, []string{"cache=none,aio=native"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", Disk: c.tuning})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			joined := strings.Join(args, " ")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("expected %q in args, got: %s", w, joined)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(joined, w) {
					t.Errorf("expected no %q in args, got: %s", w, joined)
				}
			}
		})
	}
}

func TestBuildArgsDiskUsesIOThread(t *testing.T) {
	args, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-object iothread,id=io0",
		"if=none,id=disk0,file=/d,format=qcow2",
		"-device virtio-blk-pci,drive=disk0,iothread=io0",
		"-device virtio-rng-pci,rng=rng0",
		"-device virtio-balloon-pci,free-page-reporting=on",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %s", want, joined)
		}
	}
}

func TestBuildArgsGuestAgent(t *testing.T) {
	without, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(without, " "), "virtserialport") {
		t.Error("expected no guest agent channel when GuestAgentSocket is unset")
	}
	with, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", GuestAgentSocket: "/run/x/qga.sock"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(with, " ")
	for _, want := range []string{
		"-chardev socket,id=qga0,path=/run/x/qga.sock,server=on,wait=off",
		"-device virtio-serial-pci,id=vserial0",
		"-device virtserialport,bus=vserial0.0,chardev=qga0,name=org.qemu.guest_agent.0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %s", want, joined)
		}
	}
}

func TestBuildArgsHotplugHeadroom(t *testing.T) {
	args, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", CPUs: 2, MemoryMiB: 1024, MaxCPUs: 8, MaxMemoryMiB: 4097})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-smp cpus=2,maxcpus=8,sockets=8,cores=1,threads=1",
		"-m 1024M,maxmem=4096M", // the odd MiB is dropped: virtio-mem works in 2 MiB blocks
		"-object memory-backend-memfd,id=vmem0-ram,size=3072M,share=on",
		"-device virtio-mem-pci,id=vmem0,memdev=vmem0-ram,requested-size=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %s", want, joined)
		}
	}

	plain, _ := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q", CPUs: 2, MemoryMiB: 1024})
	joined = strings.Join(plain, " ")
	if !strings.Contains(joined, "-smp 2 ") || !strings.Contains(joined, "-m 1024M ") || strings.Contains(joined, "virtio-mem") {
		t.Errorf("expected no headroom without MaxCPUs/MaxMemoryMiB, got: %s", joined)
	}
}

func TestOVMFPathCachesItsScan(t *testing.T) {
	ovmfCache.Delete("x86_64")
	first, err := OVMFPath("x86_64")
	if err != nil {
		t.Skipf("no UEFI firmware installed on this machine: %v", err)
	}

	start := time.Now()
	second, err := OVMFPath("x86_64")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("cached lookup returned %q, want %q", second, first)
	}
	// The uncached scan walks all of /usr/share and takes tens of ms.
	if elapsed > time.Millisecond {
		t.Errorf("second lookup took %v, expected it to be served from the cache", elapsed)
	}
}
