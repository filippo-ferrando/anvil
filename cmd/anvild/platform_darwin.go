//go:build darwin

package main

import (
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/dns"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/vz"
)

// newVMBackend constructs this platform's VM backend. See platform_linux.go.
func newVMBackend(catalog *image.Catalog, vault *image.Vault, db *store.Store) VMBackend {
	return vz.NewBackend(catalog, vault, db)
}

// platformEphemeralEngine is nil: ephemeral clones need Firecracker.
func platformEphemeralEngine() ephemeral.Engine { return nil }

// platformDNSHook has nothing to hook on macOS. See platform_linux.go.
func platformDNSHook(refresh func()) {}

// platformDNSZones has nothing to add on macOS. See platform_linux.go.
func platformDNSZones() []dns.Zone { return nil }
