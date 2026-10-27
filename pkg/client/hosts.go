package client

import (
	"fmt"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// DiscoveredAddress is "host[:port]" for a discovered host, leaving out the default SSH port.
func DiscoveredAddress(h *anvilv1.DiscoveredHost) string {
	if port := h.GetSshPort(); port != 0 && port != 22 {
		return fmt.Sprintf("%s:%d", h.GetAddress(), port)
	}
	return h.GetAddress()
}

// FindDiscovered returns the one discovered host whose name or address is key.
func FindDiscovered(hosts []*anvilv1.DiscoveredHost, key string) (*anvilv1.DiscoveredHost, error) {
	var matches []*anvilv1.DiscoveredHost
	for _, h := range hosts {
		if h.GetName() == key || h.GetAddress() == key {
			matches = append(matches, h)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		var names []string
		for _, h := range hosts {
			names = append(names, h.GetName())
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("no anvil hosts answered on this network")
		}
		return nil, fmt.Errorf("no discovered host named %q (found: %s)", key, strings.Join(names, ", "))
	default:
		var addrs []string
		for _, h := range matches {
			addrs = append(addrs, DiscoveredAddress(h))
		}
		return nil, fmt.Errorf("%q matches several hosts (%s), pass an address instead", key, strings.Join(addrs, ", "))
	}
}
