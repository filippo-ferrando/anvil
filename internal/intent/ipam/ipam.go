// Package ipam computes the subnet/address allocation for an intent's
// shared network.
package ipam

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
)

// AllocateSubnet deterministically derives a private /24 subnet, its
// gateway, and the sub-range reserved for the container engine's IPAM.
func AllocateSubnet(seed string, attempt int) (subnet, gateway, dockerIPRange string) {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, attempt)))
	x, y := h[0], h[1]
	return fmt.Sprintf("10.%d.%d.0/24", x, y),
		fmt.Sprintf("10.%d.%d.1", x, y),
		fmt.Sprintf("10.%d.%d.128/25", x, y)
}

// VMAddress returns the index-th static VM address in subnet's reserved
// low range (index 0 -> .2, index 1 -> .3, ...) as a CIDR.
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

// UsedVMIndices returns the set of VMAddress indices already occupied by
// addrs that fall inside subnet's reserved low range.
func UsedVMIndices(subnet string, addrs []string) (map[int]bool, error) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return nil, fmt.Errorf("ipam: invalid subnet %q: %w", subnet, err)
	}
	base := prefix.Addr().As4()
	used := make(map[int]bool, len(addrs))
	for _, a := range addrs {
		addr, err := netip.ParseAddr(a)
		if err != nil || !addr.Is4() {
			continue
		}
		ab := addr.As4()
		if ab[0] != base[0] || ab[1] != base[1] || ab[2] != base[2] {
			continue // not part of this subnet
		}
		if idx := int(ab[3]) - 2; idx >= 0 && idx <= 125 {
			used[idx] = true
		}
	}
	return used, nil
}

// NextFreeVMIndex returns the lowest VMAddress index not marked used.
func NextFreeVMIndex(used map[int]bool) (int, error) {
	for i := 0; i <= 125; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("ipam: no free static VM address left in this subnet (max 126 in use)")
}
