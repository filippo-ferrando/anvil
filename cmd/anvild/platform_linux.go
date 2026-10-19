//go:build linux

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// newVMBackend constructs this platform's VM backend: Linux builds get QEMU and Firecracker
// behind internal/vm.Hypervisors, darwin builds get vz (internal/vm/vz).
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	qemuBackend := vm.NewBackend(catalog, vault, db)
	h := vm.NewHypervisors(qemuBackend)
	h.Firecracker = vm.NewFCBackend(qemuBackend, config.FirecrackerImageDir())
	return h
}
