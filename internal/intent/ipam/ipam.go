// Package ipam is the pure subnet/address math behind an intent's shared
// network — kept separate from internal/intent itself (which imports
// internal/instance, and so needs network access to fetch oklog/ulid in
// this environment) so this logic stays testable on its own, same
// reasoning as internal/vm/qemu vs internal/vm and
// internal/container/docker vs internal/container.
package ipam

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
)

// AllocateSubnet deterministically derives a private /24 subnet, its
// gateway, and the sub-range Docker's own IPAM may assign container
// addresses from, given seed (an intent's ID) and attempt (0 for the
// first try; the caller should increment this and retry if Docker
// rejects the subnet as already in use by another network on the host —
// a real possibility since this isn't coordinated against any existing
// allocation, just hashed).
//
// Reserves 10.<x>.<y>.2-.127 for anvil's own static VM addresses (see
// VMAddress) and 10.<x>.<y>.128-.254 for Docker's own container IPAM
// (dockerIPRange) — .1 is the gateway, .0/.255 are the network/broadcast
// addresses a /24 always reserves either way. The split exists so a VM's
// manually-assigned address can never collide with one Docker
// auto-assigns to a container joining the same network later.
func AllocateSubnet(seed string, attempt int) (subnet, gateway, dockerIPRange string) {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, attempt)))
	// 10.0.0.0/8 is entirely private, host-local space with no routing
	// implications — plenty of room across two hashed octets, and no
	// reason to prefer 172.16/12 or 192.168/16 instead.
	x, y := h[0], h[1]
	return fmt.Sprintf("10.%d.%d.0/24", x, y),
		fmt.Sprintf("10.%d.%d.1", x, y),
		fmt.Sprintf("10.%d.%d.128/25", x, y)
}

// VMAddress returns the index-th static VM address in subnet's reserved
// low range (index 0 -> .2, index 1 -> .3, ...) as a CIDR, ready to hand
// straight to cloud-init's network-config. subnet must be one
// AllocateSubnet produced (a /24 whose host part is currently zero).
func VMAddress(subnet string, index int) (string, error) {
	if index < 0 || index > 125 {
		return "", fmt.Errorf("ipam: index %d out of range (max 125 static VM addresses per subnet)", index)
	}
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return "", fmt.Errorf("ipam: invalid subnet %q: %w", subnet, err)
	}
	addr := prefix.Addr().As4()
	addr[3] += byte(2 + index)
	return fmt.Sprintf("%s/%d", netip.AddrFrom4(addr).String(), prefix.Bits()), nil
}
