//go:build darwin

// Package vz will implement instance.Backend for VM instances on Apple
// Silicon via Virtualization.framework, instead of QEMU. Not implemented yet.
package vz

import (
	"context"
	"fmt"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// Source is the subset of *store.Store's methods Backend needs to
// resolve cloud-init configs and VM mirrors, like internal/vm.Source.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
	GetCloudInit(name string) (store.CloudInitConfig, error)
}

// Backend implements instance.Backend for instance.KindVM on macOS. It has no optional
// capabilities (ports, snapshots, fork) yet, so the Manager reports them as unsupported.
type Backend struct {
	Catalog *image.Catalog
	Vault   *image.Vault
	Source  Source
}

var _ instance.Backend = (*Backend)(nil)

// NewBackend returns a Backend over catalog, vault and source.
func NewBackend(catalog *image.Catalog, vault *image.Vault, source Source) *Backend {
	return &Backend{Catalog: catalog, Vault: vault, Source: source}
}

// ListCatalog returns every distro entry this backend resolves against.
// Satisfies internal/daemon.VMCatalog, same as internal/vm.Backend.
func (b *Backend) ListCatalog() ([]image.DistroEntry, error) {
	return b.Catalog.List(), nil
}

// ExportDisk satisfies internal/migrate.Exporter, same as internal/vm.Backend.
func (b *Backend) ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error {
	return errNotImplemented("ExportDisk")
}

// BaseImageChecksum, ExportDiskFile and PrepareImportedDisk satisfy internal/export.VMImporter,
// same as internal/vm.Backend.
func (b *Backend) BaseImageChecksum(spec *instance.Spec) (string, error) {
	return "", errNotImplemented("BaseImageChecksum")
}

func (b *Backend) ExportDiskFile(ctx context.Context, spec *instance.Spec, dir string) (string, error) {
	return "", errNotImplemented("ExportDiskFile")
}

func (b *Backend) PrepareImportedDisk(ctx context.Context, hypervisor instance.VMHypervisor, imageRef, arch, diskPath, baseSHA256 string) error {
	return errNotImplemented("PrepareImportedDisk")
}

func (b *Backend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	return errNotImplemented("Create")
}

func (b *Backend) Start(ctx context.Context, spec *instance.Spec) error {
	return errNotImplemented("Start")
}

func (b *Backend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	return errNotImplemented("Stop")
}

func (b *Backend) Delete(ctx context.Context, spec *instance.Spec) error {
	return errNotImplemented("Delete")
}

func (b *Backend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	return instance.StateError, errNotImplemented("Status")
}

func (b *Backend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	return errNotImplemented("Logs")
}

func errNotImplemented(op string) error {
	return fmt.Errorf("vz: %s: the Apple Virtualization.framework VM backend isn't implemented yet", op)
}
