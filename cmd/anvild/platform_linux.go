//go:build linux

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// newVMBackend constructs this platform's VM backend: Linux builds get the
// QEMU backend (internal/vm), darwin builds get vz (internal/vm/vz); exactly one is compiled in.
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	return vm.NewBackend(catalog, vault, db)
}
