//go:build linux

// Package firecracker runs Firecracker microVMs: the --config-file it boots from, the
// process itself, its REST API on a unix socket, and host-to-guest vsock connections.
package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the subset of Firecracker's full VM configuration anvil uses, written to the
// file passed as --config-file. Keys follow Firecracker's FullVmConfiguration schema.
type Config struct {
	BootSource        BootSource         `json:"boot-source"`
	MachineConfig     MachineConfig      `json:"machine-config"`
	Drives            []Drive            `json:"drives"`
	NetworkInterfaces []NetworkInterface `json:"network-interfaces,omitempty"`
	Vsock             *Vsock             `json:"vsock,omitempty"`
	MemoryHotplug     *MemoryHotplug     `json:"memory-hotplug,omitempty"`
	Balloon           *Balloon           `json:"balloon,omitempty"`
	// Entropy adds a virtio-rng device, so first boot doesn't wait on guest entropy.
	Entropy *struct{} `json:"entropy,omitempty"`
}

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

type MachineConfig struct {
	VCPUCount  int   `json:"vcpu_count"`
	MemSizeMiB int64 `json:"mem_size_mib"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	// PartUUID picks the root partition on a partitioned root drive.
	PartUUID string `json:"partuuid,omitempty"`
	// Discard passes guest TRIM through, so fstrim shrinks a sparse disk file again.
	Discard bool `json:"discard,omitempty"`
}

type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// MemoryHotplug sets up a virtio-mem region of TotalSizeMiB on top of the boot memory.
type MemoryHotplug struct {
	TotalSizeMiB int64 `json:"total_size_mib"`
}

// Balloon is the virtio-balloon device. With FreePageReporting the guest reports the pages
// it frees and firecracker gives them back to the host.
type Balloon struct {
	AmountMiB         int64 `json:"amount_mib"`
	DeflateOnOOM      bool  `json:"deflate_on_oom"`
	FreePageReporting bool  `json:"free_page_reporting,omitempty"`
}

// GuestCID is the vsock context ID every guest gets; each VM has its own vsock socket.
const GuestCID = 3

// WriteFile writes cfg as JSON to path.
func (cfg Config) WriteFile(path string) error {
	if cfg.Drives == nil {
		cfg.Drives = []Drive{} // firecracker rejects "drives": null
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: encoding config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("firecracker: writing config: %w", err)
	}
	return nil
}
