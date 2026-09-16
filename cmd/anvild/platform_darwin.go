//go:build darwin

package main

import (
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
	"github.com/anvil-project/anvil/internal/vm/vz"
)

// newVMBackend constructs this platform's VM backend. See platform_linux.go.
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	return vz.NewBackend(catalog, vault, db)
}
