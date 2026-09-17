//go:build linux

package qemu

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// NetdevID is the fixed id BuildArgs gives the VM's single netdev, both in
// bridge and SLIRP mode — a stable target for a live hostfwd_add/
// hostfwd_remove HMP command (see QMPClient.AddHostForward).
const NetdevID = "net0"

// Config describes everything needed to build a qemu-system-* command
// line for one instance.
type Config struct {
	Arch          string // "x86_64" for v1
	CPUs          int
	MemoryMiB     int64
	DiskPath      string
	SeedISOPath   string // NoCloud seed; empty means no cloud-init drive attached
	QMPSocket     string
	SerialLogPath string // guest console captured here; empty discards it
	KVM           bool   // false falls back to -accel tcg

	// Networking: exactly one of these is expected to be set by the caller.
	SLIRPHostForwards []HostForward // -netdev user,hostfwd=...
	BridgeTapDevice   string        // name of an already-created tap device to attach to

	// MACAddress, when set, is passed to the NIC device explicitly
	// instead of letting QEMU pick one. Empty for SLIRP.
	MACAddress string

	// Mounts are 9p host-directory shares.
	Mounts []Mount
}

type HostForward struct {
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" | "udp", defaults to "tcp" if empty
}

// Mount is one host directory to share into the guest via 9p
// (virtio-9p-pci + a "local" fsdev backend).
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

// MachineType returns this arch's default machine type.
func MachineType(arch string) string {
	switch arch {
	case "", "x86_64":
		return "q35"
	default:
		return "virt"
	}
}

func OVMFPath(arch string) (string, error) {
	if arch == "" {
		arch = "x86_64"
	}

	var matches []string

	err := filepath.WalkDir("/usr/share", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Ignore permission denied or access errors
		}

		if d.IsDir() {
			name := d.Name()
			if name == "doc" || name == "fonts" || name == "icons" ||
				name == "locale" || name == "man" || name == "zoneinfo" || name == "themes" {
				return filepath.SkipDir
			}
			return nil
		}

		name := d.Name()
		if (strings.HasPrefix(name, "OVMF_CODE") || name == "OVMF.fd") && strings.HasSuffix(name, ".fd") {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error scanning for OVMF: %w", err)
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("could not dynamically locate any OVMF firmware (.fd) in /usr/share")
	}

	// If we are looking for x86_64, try to avoid ARM firmwares if both are installed
	for _, m := range matches {
		lowerPath := strings.ToLower(m)
		if arch == "x86_64" && (strings.Contains(lowerPath, "aarch64") || strings.Contains(lowerPath, "arm")) {
			continue
		}
		return m, nil
	}

	return matches[0], nil
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

	OVMF, err := OVMFPath(cfg.Arch)
	if err != nil {
		return nil, fmt.Errorf("qemu: failed to locate OVMF firmware: %w", err)
	}

	args = append(args,
		"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", OVMF),
	)

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

// escapeQEMUOpt doubles literal commas in v so it survives being embedded
// as one field of a QEMU comma-delimited option string.
func escapeQEMUOpt(v string) string {
	return strings.ReplaceAll(v, ",", ",,")
}

func buildMounts(mounts []Mount) []string {
	var args []string
	for i, m := range mounts {
		fsdevID := fmt.Sprintf("fsdev%d", i)
		opts := fmt.Sprintf("local,id=%s,path=%s,security_model=mapped-xattr", fsdevID, escapeQEMUOpt(m.HostPath))
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
		device := "virtio-net-pci,netdev=" + NetdevID
		if cfg.MACAddress != "" {
			device += ",mac=" + cfg.MACAddress
		}
		return []string{
			"-netdev", fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", NetdevID, cfg.BridgeTapDevice),
			"-device", device,
		}, nil
	}

	// SLIRP is the default networking mode even with zero host-forwards.
	netdev := "user,id=" + NetdevID
	for _, f := range cfg.SLIRPHostForwards {
		proto := f.Protocol
		if proto == "" {
			proto = "tcp"
		}
		netdev += fmt.Sprintf(",hostfwd=%s::%d-:%d", proto, f.HostPort, f.GuestPort)
	}
	return []string{
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=" + NetdevID,
	}, nil
}
