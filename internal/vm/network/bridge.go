// Package network manages the tap device a VM needs to join a shared bridge.
package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// CreateTap creates a persistent tap device named tapName, attaches it to
// the already-existing bridge bridgeName, and brings it up.
func CreateTap(tapName, bridgeName string) error {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("network: finding bridge %q: %w", bridgeName, err)
	}

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: tapName},
		Mode:      netlink.TUNTAP_MODE_TAP,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return fmt.Errorf("network: creating tap device %q: %w", tapName, err)
	}

	if err := netlink.LinkSetMaster(tap, bridge); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("network: attaching %q to bridge %q: %w", tapName, bridgeName, err)
	}
	if err := netlink.LinkSetUp(tap); err != nil {
		_ = netlink.LinkDel(tap)
		return fmt.Errorf("network: bringing up tap device %q: %w", tapName, err)
	}
	return nil
}

// DeleteTap removes a tap device previously created by CreateTap. A
// missing device is not an error.
func DeleteTap(tapName string) error {
	link, err := netlink.LinkByName(tapName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("network: finding tap device %q: %w", tapName, err)
	}
	return netlink.LinkDel(link)
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

// TapStats returns tapName's cumulative byte counters as seen from the
// host: rxBytes is what the guest has sent (host receives it on the tap),
// txBytes is what the guest has received (host sent it onto the tap).
func TapStats(tapName string) (rxBytes, txBytes uint64, err error) {
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
