//go:build linux

package qemu

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// NetdevID is the fixed id BuildArgs gives the VM's single netdev (bridge
// or SLIRP), used as a stable target for hostfwd_add/hostfwd_remove HMP commands.
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
	Disk          DiskTuning

	// Networking: exactly one of these is expected to be set by the caller.
	SLIRPHostForwards []HostForward // -netdev user,hostfwd=...
	BridgeTapDevice   string        // name of an already-created tap device to attach to

	// MACAddress, when set, is passed to the NIC device explicitly
	// instead of letting QEMU pick one. Empty for SLIRP.
	MACAddress string

	// Mounts are virtiofs host-directory shares, each served by its own virtiofsd.
	Mounts []Mount

	// MaxCPUs/MaxMemoryMiB, when above CPUs/MemoryMiB, leave room to add vCPUs and
	// memory (virtio-mem) while the VM runs. 0 means no headroom.
	MaxCPUs      int
	MaxMemoryMiB int64

	// GuestAgentSocket, when set, exposes a virtio-serial channel for qemu-guest-agent on this unix socket.
	GuestAgentSocket string
}

type HostForward struct {
	HostPort  int
	GuestPort int
	Protocol  string // "tcp" | "udp", defaults to "tcp" if empty
}

// Mount is one virtiofs share: a vhost-user-fs-pci device talking to the virtiofsd
// already listening on SocketPath. Read-only is enforced by that virtiofsd.
type Mount struct {
	Tag        string // virtiofs tag; the guest mounts it with `mount -t virtiofs <tag> <path>`
	SocketPath string
}

// VirtioMemID is the virtio-mem device that holds memory added while the VM runs.
const (
	VirtioMemID        = "vmem0"
	VirtioMemBackendID = "vmem0-ram"
	VirtioMemBlockMiB  = 2 // virtio-mem's default block size on x86_64
)

// smpArg gives every vCPU its own socket, so each one can be hot-plugged on its own.
func smpArg(cpus, maxCPUs int) string {
	if maxCPUs <= cpus {
		return fmt.Sprintf("%d", cpus)
	}
	return fmt.Sprintf("cpus=%d,maxcpus=%d,sockets=%d,cores=1,threads=1", cpus, maxCPUs, maxCPUs)
}

func memArg(mem, maxMem int64) string {
	if hotplugMemoryMiB(mem, maxMem) == 0 {
		return fmt.Sprintf("%dM", mem)
	}
	return fmt.Sprintf("%dM,maxmem=%dM", mem, mem+hotplugMemoryMiB(mem, maxMem))
}

// hotplugMemoryMiB is the virtio-mem region size: the headroom above mem, in whole blocks.
func hotplugMemoryMiB(mem, maxMem int64) int64 {
	if maxMem <= mem {
		return 0
	}
	hot := maxMem - mem
	return hot - hot%VirtioMemBlockMiB
}

// HotplugPorts is how many PCIe root ports each VM gets for virtiofs devices, which caps
// its mounts. Every mount sits on its own port, so any of them can be unplugged live.
const HotplugPorts = 8

// HotplugPortID names the i-th root port, as used for a device's "bus" property.
func HotplugPortID(i int) string { return fmt.Sprintf("hp%d", i) }

// VirtiofsDeviceID and VirtiofsChardevID name one mount's QEMU objects, so a
// hot-plugged device and a boot-time one can be removed the same way.
func VirtiofsDeviceID(tag string) string  { return "fs-" + tag }
func VirtiofsChardevID(tag string) string { return "fsc-" + tag }

// BinaryName returns the qemu-system-* binary for the given arch.
func BinaryName(arch string) string {
	if arch == "" {
		arch = "x86_64"
	}
	return "qemu-system-" + arch
}

// HostArch returns the host's CPU arch in VM Arch's naming convention
// ("x86_64", "aarch64"), for choosing KVM (same-arch) vs TCG (cross-arch).
func HostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
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

// ovmfArchHints maps a target QEMU arch to path/filename substrings that
// identify its UEFI firmware across common packaging (edk2, OVMF, AAVMF).
var ovmfArchHints = map[string][]string{
	"x86_64":  {"ovmf", "x64", "amd64"},
	"aarch64": {"aavmf", "aarch64", "arm64", "qemu_efi"},
}

// ovmfSearchRoot is where the firmware scan looks. A variable so tests can
// point it at a fixture instead of depending on what the host has installed.
var ovmfSearchRoot = "/usr/share"

// ovmfCache holds the firmware path found for an arch. The scan below walks
// the whole search root, which is slow enough to matter on every VM start.
var ovmfCache sync.Map

func OVMFPath(arch string) (string, error) {
	if arch == "" {
		arch = "x86_64"
	}
	if cached, ok := ovmfCache.Load(arch); ok {
		return cached.(string), nil
	}
	hints := ovmfArchHints[arch]

	var candidates, matches []string

	err := filepath.WalkDir(ovmfSearchRoot, func(path string, d fs.DirEntry, err error) error {
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

		lower := strings.ToLower(d.Name())
		if !strings.HasSuffix(lower, ".fd") || strings.Contains(lower, "vars") {
			return nil
		}
		if !(strings.Contains(lower, "code") || lower == "ovmf.fd" || strings.Contains(lower, "qemu_efi")) {
			return nil
		}

		candidates = append(candidates, path)
		// Matched below the search root, so a directory above it can't skew
		// which arch a firmware file looks like.
		rel, relErr := filepath.Rel(ovmfSearchRoot, path)
		if relErr != nil {
			rel = path
		}
		lowerPath := strings.ToLower(rel)
		for _, h := range hints {
			if strings.Contains(lowerPath, h) {
				matches = append(matches, path)
				break
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("error scanning for OVMF: %w", err)
	}

	if len(matches) > 0 {
		ovmfCache.Store(arch, matches[0])
		return matches[0], nil
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("could not dynamically locate any UEFI firmware (.fd) in %s", ovmfSearchRoot)
	}
	return "", fmt.Errorf("could not locate %s UEFI firmware in %s (found firmware for another arch only: %v); install the %s edk2/OVMF firmware package", arch, ovmfSearchRoot, candidates, arch)
}

// BuildArgs renders the full qemu-system-* argument list for cfg. It never
// includes a graphical display device: anvil VMs are headless by design.
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

	if len(cfg.Mounts) > HotplugPorts {
		return nil, fmt.Errorf("qemu: at most %d mounts per VM, got %d", HotplugPorts, len(cfg.Mounts))
	}

	// Guest RAM is a shared memfd: vhost-user devices (virtiofsd) must map it, and it has
	// to be shared from boot for a mount to be hot-plugged later.
	args := []string{
		"-name", "anvil-instance",
		"-machine", MachineType(cfg.Arch) + ",memory-backend=mem0",
		"-nographic",
		"-nodefaults",
		"-smp", smpArg(cpus, cfg.MaxCPUs),
		"-m", memArg(mem, cfg.MaxMemoryMiB),
		"-object", fmt.Sprintf("memory-backend-memfd,id=mem0,size=%dM,share=on", mem),
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
		// "virt" (aarch64/riscv) defaults to a 32-bit-only CPU under TCG
		// (e.g. cortex-a15), so use "max" instead: valid everywhere and 64-bit.
		args = append(args, "-accel", "tcg")
		args = append(args, "-cpu", "max")
	}

	// The main disk gets its own iothread so disk I/O doesn't wait on the main QEMU loop.
	// discard/detect-zeroes let guest TRIM (fstrim) shrink the qcow2 file again.
	args = append(args,
		"-object", "iothread,id=io0",
		"-drive", fmt.Sprintf("if=none,id=disk0,file=%s,format=qcow2,discard=unmap,detect-zeroes=unmap%s", cfg.DiskPath, cfg.Disk.driveOpts()),
		"-device", "virtio-blk-pci,drive=disk0,iothread=io0",
	)

	if cfg.SeedISOPath != "" {
		args = append(args,
			"-drive", fmt.Sprintf("if=none,id=seed0,file=%s,format=raw,readonly=on", cfg.SeedISOPath),
			"-device", "virtio-blk-pci,drive=seed0",
		)
	}

	netdevArgs, err := buildNetdev(cfg)
	if err != nil {
		return nil, err
	}
	args = append(args, netdevArgs...)

	args = append(args, buildMounts(cfg.Mounts)...)

	if hot := hotplugMemoryMiB(mem, cfg.MaxMemoryMiB); hot > 0 {
		// Shared like the boot RAM, since vhost-user devices (virtiofsd) map it too.
		args = append(args,
			"-object", fmt.Sprintf("memory-backend-memfd,id=%s,size=%dM,share=on", VirtioMemBackendID, hot),
			"-device", fmt.Sprintf("virtio-mem-pci,id=%s,memdev=%s,requested-size=0", VirtioMemID, VirtioMemBackendID),
		)
	}

	if cfg.GuestAgentSocket != "" {
		args = append(args,
			"-chardev", fmt.Sprintf("socket,id=qga0,path=%s,server=on,wait=off", escapeQEMUOpt(cfg.GuestAgentSocket)),
			"-device", "virtio-serial-pci,id=vserial0",
			"-device", "virtserialport,bus=vserial0.0,chardev=qga0,name="+GuestAgentChannel,
		)
	}

	// virtio-rng avoids a stalled first boot waiting on guest entropy; the balloon's
	// free page reporting hands memory the guest has freed back to the host.
	args = append(args,
		"-object", "rng-random,id=rng0,filename=/dev/urandom",
		"-device", "virtio-rng-pci,rng=rng0",
		"-device", "virtio-balloon-pci,free-page-reporting=on",
	)

	return args, nil
}

// escapeQEMUOpt doubles literal commas in v so it survives being embedded
// as one field of a QEMU comma-delimited option string.
func escapeQEMUOpt(v string) string {
	return strings.ReplaceAll(v, ",", ",,")
}

// buildMounts adds the hot-plug root ports, then puts each boot-time mount on its own port.
func buildMounts(mounts []Mount) []string {
	var args []string
	for i := range HotplugPorts {
		args = append(args, "-device", fmt.Sprintf("pcie-root-port,id=%s,chassis=%d", HotplugPortID(i), i+1))
	}
	for i, m := range mounts {
		args = append(args,
			"-chardev", fmt.Sprintf("socket,id=%s,path=%s", VirtiofsChardevID(m.Tag), escapeQEMUOpt(m.SocketPath)),
			"-device", fmt.Sprintf("vhost-user-fs-pci,id=%s,chardev=%s,tag=%s,queue-size=1024,bus=%s",
				VirtiofsDeviceID(m.Tag), VirtiofsChardevID(m.Tag), m.Tag, HotplugPortID(i)),
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
