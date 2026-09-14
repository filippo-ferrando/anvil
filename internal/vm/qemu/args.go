package qemu

import "fmt"

// Config describes everything needed to build a qemu-system-* command line
// for one instance. It intentionally doesn't reference internal/instance's
// Spec type directly — the caller (internal/instance/manager) is
// responsible for translating a VMSpec into this narrower, QEMU-specific
// shape, keeping this package ignorant of the registry/domain model.
type Config struct {
	Arch          string // "x86_64" for v1; kept as a field so aarch64 is additive later
	CPUs          int
	MemoryMiB     int64
	DiskPath      string
	SeedISOPath   string // NoCloud seed; empty means no cloud-init drive attached
	QMPSocket     string
	SerialLogPath string // guest console (boot messages, cloud-init output) captured here; empty discards it entirely
	KVM           bool   // false falls back to -accel tcg (no /dev/kvm access)

	// Networking: exactly one of these is expected to be set by the caller.
	// SLIRP is the default for a standalone instance; Bridge is used for an
	// intent's shared network (see internal/vm/network).
	SLIRPHostForwards []HostForward // -netdev user,hostfwd=...
	BridgeTapDevice   string        // name of an already-created tap device to attach to

	// MACAddress, when set, is passed to the NIC device explicitly
	// instead of letting QEMU pick one itself — needed for a bridged
	// (BridgeTapDevice) NIC specifically, so the cloud-init network-config
	// generated for it (internal/vm.Backend's bridgeNetworkConfig) can
	// match this exact interface by MAC, sidestepping guest interface
	// *naming* entirely (see that function's doc comment for the real bug
	// this fixes). Empty for SLIRP, which has no guest-side static config
	// to match against in the first place.
	MACAddress string

	// Mounts are 9p host-directory shares — see Mount's doc comment for why
	// this is 9p rather than virtiofs, and why there's no way to add one to
	// an already-running instance without a restart.
	Mounts []Mount
}

type HostForward struct {
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" | "udp", defaults to "tcp" if empty
}

// Mount is one host directory to share into the guest via 9p
// (virtio-9p-pci + a "local" fsdev backend). Chosen over virtiofs
// deliberately: virtiofs needs a separate virtiofsd process per share
// (not a dependency this project has today) plus a shared memory backend
// configured at VM boot time, and — checked empirically against a real
// QEMU 11.1.1 build, not assumed — neither a 9p fsdev backend nor (as far
// as could be determined) a virtiofs one can be hot-added to an
// already-running instance via QMP (`qom-list-types` reports no
// user-creatable fsdev-backend object at all). So a 9p share added to a
// running instance requires restarting its QEMU process either way; 9p
// just avoids the extra virtiofsd dependency and boot-time memory-backend
// requirement for a capability neither backend can truly hot-plug anyway.
type Mount struct {
	HostPath string
	Tag      string // 9p mount_tag; guest-side `mount -t 9p -o trans=virtio` uses this, not the host path
	ReadOnly bool
}

// BinaryName returns the qemu-system-* binary for the given arch.
func BinaryName(arch string) string {
	if arch == "" {
		arch = "x86_64"
	}
	return "qemu-system-" + arch
}

// MachineType returns this arch's default machine type. Only x86_64/q35 is
// implemented for v1; other arches are a placeholder for later work.
func MachineType(arch string) string {
	switch arch {
	case "", "x86_64":
		return "q35"
	default:
		return "virt"
	}
}

// BuildArgs renders the full qemu-system-* argument list for cfg. It never
// includes a graphical display device — anvil VMs are headless by design.
func BuildArgs(cfg Config) ([]string, error) {
	if cfg.DiskPath == "" {
		return nil, fmt.Errorf("qemu: DiskPath is required")
	}
	if cfg.QMPSocket == "" {
		return nil, fmt.Errorf("qemu: QMPSocket is required")
	}

	cpus := cfg.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	mem := cfg.MemoryMiB
	if mem <= 0 {
		mem = 1024
	}

	serial := "null" // discard entirely if the caller didn't ask for a log
	if cfg.SerialLogPath != "" {
		// file: is one-way (guest output only, no input) — fine for our
		// purposes, this is for capturing boot/cloud-init messages for
		// debugging, not an interactive console.
		serial = "file:" + cfg.SerialLogPath
	}

	args := []string{
		"-name", "anvil-instance",
		"-machine", MachineType(cfg.Arch),
		"-nographic",
		"-nodefaults",
		"-smp", fmt.Sprintf("%d", cpus),
		"-m", fmt.Sprintf("%d", mem),
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", cfg.QMPSocket),
		"-serial", serial,
	}

	if cfg.KVM {
		args = append(args, "-accel", "kvm")
		args = append(args, "-cpu", "host")
	} else {
		args = append(args, "-accel", "tcg")
	}

	args = append(args,
		"-drive", fmt.Sprintf("if=virtio,file=%s,format=qcow2", cfg.DiskPath),
	)

	if cfg.SeedISOPath != "" {
		args = append(args,
			"-drive", fmt.Sprintf("if=virtio,file=%s,format=raw,readonly=on", cfg.SeedISOPath),
		)
	}

	netdevArgs, err := buildNetdev(cfg)
	if err != nil {
		return nil, err
	}
	args = append(args, netdevArgs...)

	args = append(args, buildMounts(cfg.Mounts)...)

	return args, nil
}

func buildMounts(mounts []Mount) []string {
	var args []string
	for i, m := range mounts {
		fsdevID := fmt.Sprintf("fsdev%d", i)
		opts := fmt.Sprintf("local,id=%s,path=%s,security_model=mapped-xattr", fsdevID, m.HostPath)
		if m.ReadOnly {
			opts += ",readonly=on"
		}
		args = append(args,
			"-fsdev", opts,
			"-device", fmt.Sprintf("virtio-9p-pci,fsdev=%s,mount_tag=%s", fsdevID, m.Tag),
		)
	}
	return args
}

func buildNetdev(cfg Config) ([]string, error) {
	if cfg.BridgeTapDevice != "" {
		device := "virtio-net-pci,netdev=net0"
		if cfg.MACAddress != "" {
			device += ",mac=" + cfg.MACAddress
		}
		return []string{
			"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", cfg.BridgeTapDevice),
			"-device", device,
		}, nil
	}

	// SLIRP is the default networking mode even with zero host-forwards.
	netdev := "user,id=net0"
	for _, f := range cfg.SLIRPHostForwards {
		proto := f.Protocol
		if proto == "" {
			proto = "tcp"
		}
		netdev += fmt.Sprintf(",hostfwd=%s::%d-:%d", proto, f.HostPort, f.GuestPort)
	}
	return []string{
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=net0",
	}, nil
}
