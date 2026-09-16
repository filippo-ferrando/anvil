//go:build linux

package main

import (
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// newVMBackend constructs this platform's VM backend. Linux builds get the
// QEMU backend (internal/vm); darwin builds get the Apple
// Virtualization.framework one (internal/vm/vz, see platform_darwin.go) —
// exactly one is ever compiled into a given anvild binary.
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	return vm.NewBackend(catalog, vault, db)
}
