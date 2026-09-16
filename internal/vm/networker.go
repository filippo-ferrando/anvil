//go:build linux

package vm

// Networker attaches a VM instance's virtual NIC to its intent's shared
// network and reports on it. The only implementation today is
// internal/vm/network.LinuxBridge (Linux tap devices on a netlink
// bridge) — this interface is what keeps Backend's disk/seed/QEMU
// lifecycle logic independent of that Linux-specific mechanism, so a
// future non-Linux VM backend (e.g. one built on Apple's
// Virtualization.framework, which has no netlink/bridge equivalent)
// could supply its own instead of reusing this one.
type Networker interface {
	// Attach joins instanceID's network device to the bridge/network
	// named bridgeName, creating the device if needed, and returns the
	// OS-level device name for qemu.Config.BridgeTapDevice.
	Attach(instanceID, bridgeName string) (deviceName string, err error)

	// Detach tears down whatever Attach created for instanceID. A
	// missing device is not an error.
	Detach(instanceID string) error

	// Stats returns instanceID's attached device's cumulative rx/tx
	// byte counters as seen from the host.
	Stats(instanceID string) (rxBytes, txBytes uint64, err error)
}
