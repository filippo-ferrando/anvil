//go:build linux

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// newVMBackend constructs this platform's VM backend: Linux builds get QEMU and Firecracker
// behind internal/vm.Hypervisors, darwin builds get vz (internal/vm/vz).
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	qemuBackend := vm.NewBackend(catalog, vault, db)
	h := vm.NewHypervisors(qemuBackend)
	h.Firecracker = vm.NewFCBackend(qemuBackend, config.FirecrackerImageDir(), db)
	return h
}

// platformDNSHook makes the DNS server pick up the standalone Firecracker zone as soon as
// its bridge is created, not on the next periodic refresh.
func platformDNSHook(refresh func()) { vm.FCNetworkUp = refresh }

// platformDNSZones adds the forward-only zone of standalone Firecracker VMs, once their bridge exists.
func platformDNSZones() []dns.Zone {
	if z, ok := vm.FCDNSZone(); ok {
		return []dns.Zone{z}
	}
	return nil
}
