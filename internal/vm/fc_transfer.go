//go:build linux

package vm

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// A Firecracker disk travels (migration, export bundles) as qcow2 like a QEMU one: either
// flattened, or a delta on top of the raw copy of its base image. The raw copy is the
// base qcow2's disk content, so the same base gives the same raw copy on every host, and
// the base qcow2's checksum still identifies it.

func qemuImg(ctx context.Context, what string, args ...string) error {
	if err := image.QemuImg(ctx, args...); err != nil {
		return fmt.Errorf("vm: %s: %w", what, err)
	}
	return nil
}

// cachedBase returns the cached qcow2 base image spec was created from.
func (b *FCBackend) cachedBase(spec *instance.Spec) (image.DistroEntry, string, error) {
	catalog, err := b.QEMU.EffectiveCatalog()
	if err != nil {
		return image.DistroEntry{}, "", err
	}
	entry, err := catalog.Find(spec.VM.ImageRef, spec.VM.Arch)
	if err != nil {
		return entry, "", err
	}
	path, ok := b.QEMU.Vault.CachedPath(entry)
	if !ok {
		return entry, "", fmt.Errorf("vm: %s's base image %s isn't cached", spec.Name, entry.ID)
	}
	return entry, path, nil
}

// ExportDisk writes spec's whole disk to destPath as qcow2. spec's VM must be stopped.
func (b *FCBackend) ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("vm: creating export destination dir: %w", err)
	}
	return qemuImg(ctx, "exporting disk", "convert", "-f", "raw", "-O", "qcow2", spec.VM.DiskPath, destPath)
}

func (b *FCBackend) BaseImageChecksum(spec *instance.Spec) (string, error) {
	_, path, err := b.cachedBase(spec)
	if err != nil {
		return "", err
	}
	return image.FileChecksum(path)
}

func (b *FCBackend) BaseImagePath(spec *instance.Spec) (string, error) {
	_, path, err := b.cachedBase(spec)
	return path, err
}

// ExportDiskDelta writes only what differs from the raw copy of spec's base to destPath, as qcow2.
func (b *FCBackend) ExportDiskDelta(ctx context.Context, spec *instance.Spec, destPath string) error {
	entry, _, err := b.cachedBase(spec)
	if err != nil {
		return err
	}
	rawBase, err := b.Cache.RawBase(ctx, entry, nil)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("vm: creating export destination dir: %w", err)
	}
	return qemuImg(ctx, "exporting disk delta",
		"convert", "-f", "raw", "-O", "qcow2", "-B", rawBase, "-F", "raw", spec.VM.DiskPath, destPath)
}

// ExportDiskFile writes the file an export bundle carries for spec into dir: a delta when
// the base is cached here, the whole disk otherwise.
func (b *FCBackend) ExportDiskFile(ctx context.Context, spec *instance.Spec, dir string) (string, error) {
	dest := filepath.Join(dir, spec.ID+".qcow2")
	if err := b.ExportDiskDelta(ctx, spec, dest); err == nil {
		return dest, nil
	}
	_ = os.Remove(dest)
	return dest, b.ExportDisk(ctx, spec, dest)
}

// PrepareImportedDisk points an imported delta at this host's raw copy of the same base,
// after checking the base is identical. A flattened disk is left as is.
func (b *FCBackend) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath, baseSHA256 string) error {
	entry, base, err := b.QEMU.importBase(ctx, imageRef, arch, diskPath, baseSHA256)
	if err != nil || base == "" {
		return err
	}
	rawBase, err := b.Cache.RawBase(ctx, entry, nil)
	if err != nil {
		return err
	}
	return qemuImg(ctx, "rebasing imported disk", "rebase", "-u", "-F", "raw", "-b", rawBase, diskPath)
}

// adoptDisk turns a migrated or imported qcow2 (v.SourceDiskPath) into this instance's raw
// disk. A migrated delta (SourceDiskBaseSHA256 set) is first put on the local raw base.
func (b *FCBackend) adoptDisk(ctx context.Context, spec *instance.Spec, dir string, progress func(string)) error {
	v := spec.VM
	if progress != nil {
		progress("adopting migrated disk")
	}
	if v.SourceDiskBaseSHA256 != "" {
		basePath, err := b.QEMU.matchingLocalBase(ctx, v)
		if err != nil {
			if v.SourceBaseImagePath == "" {
				return err
			}
			if basePath, err = b.QEMU.adoptSentBase(ctx, v, dir); err != nil {
				return err
			}
		} else if v.SourceBaseImagePath != "" {
			_ = os.Remove(v.SourceBaseImagePath) // this host had its own copy already
		}
		rawBase, err := b.rawCopyOf(ctx, v, basePath, dir)
		if err != nil {
			return err
		}
		if err := qemuImg(ctx, "rebasing migrated disk", "rebase", "-u", "-F", "raw", "-b", rawBase, v.SourceDiskPath); err != nil {
			return err
		}
	}

	diskPath := filepath.Join(dir, "disk.raw")
	if err := qemuImg(ctx, "converting migrated disk", "convert", "-O", "raw", v.SourceDiskPath, diskPath); err != nil {
		_ = os.Remove(diskPath) // a partial, full-size raw; the source disk is still there
		return err
	}
	_ = os.Remove(v.SourceDiskPath)
	_ = os.Remove(filepath.Join(dir, "base.raw"))
	_ = os.Remove(filepath.Join(dir, "base.qcow2"))
	v.DiskPath = diskPath
	v.SourceDiskPath, v.SourceDiskBaseSHA256, v.SourceBaseImagePath = "", "", ""
	adoptMountBookkeeping(v)
	return nil
}

// rawCopyOf returns a raw copy of the qcow2 base at basePath: the cached one when it's the
// vault's image, or a temporary one in dir for a base that travelled with the disk.
func (b *FCBackend) rawCopyOf(ctx context.Context, v *instance.VMSpec, basePath, dir string) (string, error) {
	catalog, err := b.QEMU.EffectiveCatalog()
	if err != nil {
		return "", err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return "", err
	}
	if cached, ok := b.QEMU.Vault.CachedPath(entry); ok && cached == basePath {
		return b.Cache.RawBase(ctx, entry, nil)
	}
	raw := filepath.Join(dir, "base.raw")
	return raw, qemuImg(ctx, "converting the base image that came with the disk", "convert", "-O", "raw", basePath, raw)
}

// keepAddress reserves the address a migrated or imported standalone guest already has
// configured inside its disk, which boots without a new seed.
func (b *FCBackend) keepAddress(spec *instance.Spec) error {
	subnet, gateway, err := fcNetwork()
	if err != nil {
		return err
	}
	prefix, err := netip.ParsePrefix(spec.VM.StaticIP)
	if err != nil || !subnet.Contains(prefix.Addr()) {
		return fmt.Errorf("vm: %s's address %s isn't in this host's firecracker subnet %s (%s)",
			spec.Name, spec.VM.StaticIP, subnet, fcSubnetEnv)
	}
	if err := b.ipam.reserve(spec.ID, spec.VM.StaticIP); err != nil {
		return err
	}
	spec.VM.Gateway = gateway.String()
	spec.VM.DNSServers = []string{gateway.String()}
	return nil
}
