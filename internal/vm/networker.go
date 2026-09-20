//go:build linux

package vm

// Networker attaches a VM's NIC to its shared network and reports on it.
// It decouples Backend from LinuxBridge's netlink specifics for portability.
type Networker interface {
	// Attach joins instanceID's device to the bridge/network named
	// bridgeName (creating it if needed) and returns the device name.
	Attach(instanceID, bridgeName string) (deviceName string, err error)

	// Detach tears down whatever Attach created for instanceID. A
	// missing device is not an error.
	Detach(instanceID string) error

	// Stats returns instanceID's attached device's cumulative rx/tx
	// byte counters as seen from the host.
	Stats(instanceID string) (rxBytes, txBytes uint64, err error)
}
