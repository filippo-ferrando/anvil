// Package network manages the host-side networking a VM needs to join an
// intent's shared bridge (see the plan's Intents section): creating a tap
// device and attaching it to an already-existing Linux bridge, so a QEMU
// process can be pointed at it with `-netdev tap,ifname=...,script=no`
// (see internal/vm/qemu.Config.BridgeTapDevice, already wired to accept
// this). Nothing here creates the bridge itself — that's
// internal/container's job (a Docker/Podman network's own bridge device),
// this package only ever attaches a VM's tap to a bridge that already
// exists by the time it's called.
//
// Uses github.com/vishvananda/netlink (real netlink sockets, not shelling
// out to `ip`) — the same library Docker/containerd/most Go CNI tooling
// uses for this exact job. This is meaningfully less verified than the
// rest of this project's networking code: there was no way to fetch this
// dependency or exercise it against a real bridge in the sandbox this was
// written in (no network access, no root, no existing bridge to test
// against) — treat this file as the one part of M4 that most needs a real
// smoke test before trusting it.
package network

import (
	"fmt"

	"github.com/vishvananda/netlink"
)

// CreateTap creates a persistent tap device named tapName and attaches it
// to the already-existing bridge bridgeName, bringing the tap up.
// Requires CAP_NET_ADMIN in the calling process's effective set — anvild
// runs as the unprivileged "anvil" user (see PLAN.md's M6 notes), granted
// this specific capability as an ambient capability by
// packaging/anvild.service, not by running as root.
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

// DeleteTap removes a tap device previously created by CreateTap. Called
// on VM stop/delete so tap devices don't accumulate on the host across
// restarts — a missing device is not an error, matching how
// internal/container's Delete treats an already-gone container as
// success.
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

// TapName derives a deterministic tap device name from an instance ID.
// Linux interface names are capped at 15 usable characters (IFNAMSIZ=16
// including the trailing NUL), too short for a full ULID, so this just
// takes a prefix — collisions are only possible if two instance IDs share
// the same first 10 characters, which ULID's own monotonic/random design
// makes astronomically unlikely for anything actually running on the same
// host at once.
func TapName(instanceID string) string {
	id := instanceID
	if len(id) > 10 {
		id = id[:10]
	}
	return "tap-" + id
}
