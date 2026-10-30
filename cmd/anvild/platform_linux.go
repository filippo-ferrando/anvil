//go:build linux

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
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
	fcBackend = vm.NewFCBackend(qemuBackend, config.FirecrackerImageDir(), db)
	h.Firecracker = fcBackend
	return h
}

// platformEphemeralEngine runs ephemeral clones: Firecracker, set up by newVMBackend.
func platformEphemeralEngine() ephemeral.Engine { return fcBackend }

var fcBackend *vm.FCBackend

// platformDNSHook makes the DNS server pick up the standalone Firecracker zone as soon as
// its bridge is created, and clone zones as clones come and go.
func platformDNSHook(refresh func()) {
	vm.FCNetworkUp = refresh
	fcBackend.Clones.OnChange = refresh
}

// platformDNSZones adds the forward-only zones of standalone Firecracker VMs (once their
// bridge exists) and of ephemeral clones.
func platformDNSZones() []dns.Zone {
	zones := fcBackend.Clones.DNSZones()
	if z, ok := vm.FCDNSZone(); ok {
		zones = append(zones, z)
	}
	return zones
}
