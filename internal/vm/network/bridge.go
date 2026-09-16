//go:build linux

// Package network is the Linux-specific implementation of vm.Networker:
// tap devices attached to an existing Linux bridge, via netlink. A future
// non-Linux VM backend (e.g. one built on Apple's Virtualization.framework)
// would supply its own vm.Networker instead of using this package, since
// Linux netlink taps and bridges have no macOS equivalent.
package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// LinuxBridge implements vm.Networker.
type LinuxBridge struct{}

// Attach creates a persistent tap device for instanceID, attaches it to
// the already-existing bridge bridgeName, and brings it up.
func (LinuxBridge) Attach(instanceID, bridgeName string) (string, error) {
	tapName := TapName(instanceID)

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return "", fmt.Errorf("network: finding bridge %q: %w", bridgeName, err)
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return "", fmt.Errorf("network: creating tap device %q: %w", tapName, err)
	}

	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		_ = netlink.LinkDel(tap)
		return "", fmt.Errorf("network: attaching %q to bridge %q: %w", tapName, bridgeName, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		_ = netlink.LinkDel(tap)
		return "", fmt.Errorf("network: bringing up tap device %q: %w", tapName, err)
	}
	return tapName, nil
}

// Detach removes the tap device Attach created for instanceID. A missing
// device is not an error.
func (LinuxBridge) Detach(instanceID string) error {
	tapName := TapName(instanceID)
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("network: finding tap device %q: %w", tapName, err)
	}
	return netlink.LinkDel(link)
}

// Stats returns instanceID's tap device's cumulative byte counters as
// seen from the host: rxBytes is what the guest has sent (host receives
// it on the tap), txBytes is what the guest has received (host sent it
// onto the tap).
func (LinuxBridge) Stats(instanceID string) (rxBytes, txBytes uint64, err error) {
	tapName := TapName(instanceID)
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		return 0, 0, fmt.Errorf("network: finding tap device %q: %w", tapName, err)
	}
	stats := link.Attrs().Statistics
	if stats == nil {
		return 0, 0, nil
	}
	return stats.RxBytes, stats.TxBytes, nil
}

// TapName derives a deterministic tap device name from the last 10
// characters of an instance ID, to fit Linux's interface name length limit.
func TapName(instanceID string) string {
	id := instanceID
	if len(id) > 10 {
		id = id[len(id)-10:]
	}
	return "tap-" + id
}
