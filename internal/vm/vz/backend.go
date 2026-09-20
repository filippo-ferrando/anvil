//go:build darwin

// Package vz will implement instance.Backend for VM instances on Apple
// Silicon via Virtualization.framework, instead of QEMU. Not implemented yet.
package vz

import (
	"context"
	"fmt"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/image"
)

// Source is the subset of *store.Store's methods Backend needs to
// resolve cloud-init configs and VM mirrors. Mirrors internal/vm.Source.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
	GetCloudInit(name string) (store.CloudInitConfig, error)
}

// Backend implements instance.Backend for instance.KindVM on macOS.
type Backend struct {
	Catalog *image.Catalog
	Vault   *image.Vault
	Source  Source
}

var _ instance.Backend = (*Backend)(nil)

func NewBackend(catalog *image.Catalog, vault *image.Vault, source Source) *Backend {
	return &Backend{Catalog: catalog, Vault: vault, Source: source}
}

// ListCatalog returns every distro entry this backend resolves against.
// Satisfies internal/daemon.VMCatalog, same as internal/vm.Backend.
func (b *Backend) ListCatalog() ([]image.DistroEntry, error) {
	return b.Catalog.List(), nil
}

// ExportDisk flattens spec's current disk into a standalone file at
// destPath. Satisfies internal/migrate.Exporter, same as internal/vm.Backend.
func (b *Backend) ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error {
	return errNotImplemented("ExportDisk")
}

// PrepareImportedDisk ensures imageRef/arch's base image is present and
// rebases diskPath's backing file onto it. Satisfies internal/export.VMImporter.
func (b *Backend) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath string) error {
	return errNotImplemented("PrepareImportedDisk")
}

// AddPort and RemovePort satisfy instance.PortForwarder, same as internal/vm.Backend.
func (b *Backend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	return errNotImplemented("AddPort")
}

func (b *Backend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	return errNotImplemented("RemovePort")
}

// CreateSnapshot, RestoreSnapshot, DeleteSnapshot, and ListSnapshots
// satisfy instance.Snapshotter, same as internal/vm.Backend.
func (b *Backend) CreateSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	return errNotImplemented("CreateSnapshot")
}

func (b *Backend) RestoreSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	return errNotImplemented("RestoreSnapshot")
}

func (b *Backend) DeleteSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	return errNotImplemented("DeleteSnapshot")
}

func (b *Backend) ListSnapshots(ctx context.Context, spec *instance.Spec) ([]instance.Snapshot, error) {
	return nil, errNotImplemented("ListSnapshots")
}

// Fork satisfies instance.Forker, same as internal/vm.Backend.
func (b *Backend) Fork(ctx context.Context, source, dest *instance.Spec, progress func(status string)) error {
	return errNotImplemented("Fork")
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
