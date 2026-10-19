//go:build darwin

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/vz"
)

// newVMBackend constructs this platform's VM backend. See platform_linux.go.
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	return vz.NewBackend(catalog, vault, db)
}

// platformDNSZones has nothing to add on macOS. See platform_linux.go.
func platformDNSZones() []dns.Zone { return nil }
