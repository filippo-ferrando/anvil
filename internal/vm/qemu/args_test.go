package qemu

import (
	"strings"
	"testing"
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
		Mounts: []Mount{
			{HostPath: "/home/me/project", Tag: "mount0"},
			{HostPath: "/home/me/readonly-stuff", Tag: "mount1", ReadOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-fsdev local,id=fsdev0,path=/home/me/project,security_model=mapped-xattr",
		"-device virtio-9p-pci,fsdev=fsdev0,mount_tag=mount0",
		"-fsdev local,id=fsdev1,path=/home/me/readonly-stuff,security_model=mapped-xattr,readonly=on",
		"-device virtio-9p-pci,fsdev=fsdev1,mount_tag=mount1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %s", want, joined)
		}
	}
}

func TestBuildArgsNoMountsMeansNoFsdev(t *testing.T) {
	args, err := BuildArgs(Config{DiskPath: "/d", QMPSocket: "/q"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.Join(args, " "), "-fsdev") {
		t.Error("expected no -fsdev args when Mounts is empty")
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
