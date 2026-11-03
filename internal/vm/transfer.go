//go:build linux

package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// adoptMigratedDisk moves v.SourceDiskPath into place as this instance's
// disk.qcow2, skipping overlay creation and reseeding. A delta disk is rebased onto the local base.
func (b *Backend) adoptMigratedDisk(ctx context.Context, spec *instance.Spec, dir string, progress func(status string)) error {
	v := spec.VM
	if progress != nil {
		progress("adopting migrated disk")
	}

	// Check the base before touching the disk, so a mismatch leaves it where it was.
	var basePath string
	if v.SourceDiskBaseSHA256 != "" {
		local, err := b.matchingLocalBase(ctx, v)
		switch {
		case err == nil:
			// A base image that travelled for nothing, because this host cached
			// its own copy in the meantime.
			if v.SourceBaseImagePath != "" {
				_ = os.Remove(v.SourceBaseImagePath)
			}
		case v.SourceBaseImagePath == "":
			return err
		default:
			if progress != nil {
				progress("adopting the base image that came with the disk")
			}
			if local, err = b.adoptSentBase(ctx, v, dir); err != nil {
				return err
			}
		}
		basePath = local
	}

	diskPath := filepath.Join(dir, "disk.qcow2")
	if err := moveFile(v.SourceDiskPath, diskPath); err != nil {
		return fmt.Errorf("vm: adopting migrated disk: %w", err)
	}
	v.DiskPath = diskPath
	v.SourceDiskPath = ""
	v.SourceBaseImagePath = ""
	adoptMountBookkeeping(v)

	if basePath != "" {
		if progress != nil {
			progress("rebasing migrated disk onto the local base image")
		}
		if err := qemuImg(ctx, "rebasing migrated disk", "rebase", "-u", "-F", "qcow2", "-b", basePath, diskPath); err != nil {
			return err
		}
		v.SourceDiskBaseSHA256 = ""
	}
	return nil
}

// matchingLocalBase returns this host's cached copy of v's base image, after
// checking it is byte-identical to the one the migrated delta was made against.
func (b *Backend) matchingLocalBase(ctx context.Context, v *instance.VMSpec) (string, error) {
	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return "", err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return "", fmt.Errorf("vm: resolving base image of migrated disk: %w", err)
	}
	basePath, ok := b.Vault.CachedPath(entry)
	if !ok {
		return "", fmt.Errorf("vm: migrated disk needs base image %s (%s), which isn't cached here", entry.ID, entry.Arch)
	}
	b.Vault.Use(ctx, basePath)
	sum, err := image.FileChecksum(basePath)
	if err != nil {
		return "", fmt.Errorf("vm: hashing base image %s: %w", basePath, err)
	}
	if sum != v.SourceDiskBaseSHA256 {
		return "", fmt.Errorf("vm: local base image %s differs from the source's (sha256 %s, want %s)", entry.ID, sum, v.SourceDiskBaseSHA256)
	}
	return basePath, nil
}

// adoptSentBase installs a base image that travelled with a migrated disk. It goes
// into the image cache when nothing is cached under that name yet, so later
// migrations only need the delta, and next to the disk otherwise.
func (b *Backend) adoptSentBase(ctx context.Context, v *instance.VMSpec, dir string) (string, error) {
	sum, err := image.FileChecksum(v.SourceBaseImagePath)
	if err != nil {
		return "", fmt.Errorf("vm: hashing the base image that came with the disk: %w", err)
	}
	if sum != v.SourceDiskBaseSHA256 {
		return "", fmt.Errorf("vm: the base image that came with the disk is corrupt (sha256 %s, want %s)", sum, v.SourceDiskBaseSHA256)
	}

	dest := filepath.Join(dir, "base.qcow2")
	if catalog, cerr := b.EffectiveCatalog(); cerr == nil {
		if entry, ferr := catalog.Find(v.ImageRef, v.Arch); ferr == nil {
			// Never overwrite a cached image: other instances' disks back onto it.
			if path, cached := b.Vault.CachedPath(entry); !cached {
				dest = path
				b.Vault.Use(ctx, dest)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("vm: creating %s: %w", filepath.Dir(dest), err)
	}
	if err := moveFile(v.SourceBaseImagePath, dest); err != nil {
		return "", fmt.Errorf("vm: adopting the base image that came with the disk: %w", err)
	}
	image.MoveChecksum(v.SourceBaseImagePath, dest, sum)
	return dest, nil
}

// moveFile renames src to dst, copying instead when they're on different filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// copyFile copies src to dst; used as os.Rename's cross-filesystem fallback.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	return out.Close()
}

// ExportDisk flattens spec's current disk into a standalone qcow2 file
// at destPath. Must only be called while spec's VM is stopped.
func (b *Backend) ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: ExportDisk called with a nil VMSpec")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("vm: creating export destination dir: %w", err)
	}
	return qemuImg(ctx, "exporting disk", "convert", "-O", "qcow2", spec.VM.DiskPath, destPath)
}

// BaseImageChecksum returns the SHA256 of the base image spec's disk is an overlay on.
func (b *Backend) BaseImageChecksum(spec *instance.Spec) (string, error) {
	if spec.VM == nil {
		return "", fmt.Errorf("vm: BaseImageChecksum called with a nil VMSpec")
	}
	backing, err := baseOf(spec)
	if err != nil {
		return "", err
	}
	return image.FileChecksum(backing)
}

// BaseImagePath returns the local path of the base image spec's disk is an overlay on.
func (b *Backend) BaseImagePath(spec *instance.Spec) (string, error) {
	if spec.VM == nil {
		return "", fmt.Errorf("vm: BaseImagePath called with a nil VMSpec")
	}
	return baseOf(spec)
}

// baseOf returns the base image spec's qcow2 disk is an overlay on, failing when it has none.
func baseOf(spec *instance.Spec) (string, error) {
	backing, err := image.BackingFile(spec.VM.DiskPath)
	if err != nil {
		return "", fmt.Errorf("vm: inspecting %s's disk: %w", spec.Name, err)
	}
	if backing == "" {
		return "", fmt.Errorf("vm: %s's disk has no base image", spec.Name)
	}
	return backing, nil
}

// ExportDiskDelta writes only the parts of spec's disk that differ from its base
// image to destPath. spec's VM must be stopped.
func (b *Backend) ExportDiskDelta(ctx context.Context, spec *instance.Spec, destPath string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: ExportDiskDelta called with a nil VMSpec")
	}
	backing, err := baseOf(spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("vm: creating export destination dir: %w", err)
	}
	return qemuImg(ctx, "exporting disk delta", "convert", "-O", "qcow2", "-B", backing, "-F", "qcow2", spec.VM.DiskPath, destPath)
}

// ExportDiskFile returns the file an export bundle carries for spec: its overlay as it is,
// backed by the base image. dir is unused, nothing is written.
func (b *Backend) ExportDiskFile(ctx context.Context, spec *instance.Spec, dir string) (string, error) {
	return spec.VM.DiskPath, nil
}

// PrepareImportedDisk rebases an imported disk's backing file onto this host's own
// base image, since it still points at its original export host. A disk with no base is left as is.
// A non-empty baseSHA256 must match that base image, or the disk is rejected.
func (b *Backend) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath, baseSHA256 string) error {
	_, basePath, err := b.importBase(ctx, imageRef, arch, diskPath, baseSHA256)
	if err != nil || basePath == "" {
		return err
	}
	return qemuImg(ctx, "rebasing imported disk", "rebase", "-u", "-F", "qcow2", "-b", basePath, diskPath)
}

// importBase returns the local base image an imported delta at diskPath sits on, after
// checking it against baseSHA256 when set. A disk with no base gives an empty path.
func (b *Backend) importBase(ctx context.Context, imageRef, arch, diskPath, baseSHA256 string) (image.DistroEntry, string, error) {
	// A flattened disk (e.g. one adopted from a migration) holds every block itself.
	// Giving it a base would make its unallocated blocks read the base's data instead of zeros.
	backing, err := image.BackingFile(diskPath)
	if err != nil {
		return image.DistroEntry{}, "", fmt.Errorf("vm: inspecting imported disk: %w", err)
	}
	if backing == "" {
		return image.DistroEntry{}, "", nil
	}
	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return image.DistroEntry{}, "", err
	}
	entry, err := catalog.Find(imageRef, arch)
	if err != nil {
		return entry, "", err
	}
	basePath, err := b.Vault.Ensure(ctx, entry, nil)
	if err != nil {
		return entry, "", fmt.Errorf("vm: preparing imported disk's base image: %w", err)
	}
	if baseSHA256 != "" {
		sum, err := image.FileChecksum(basePath)
		if err != nil {
			return entry, "", fmt.Errorf("vm: hashing base image %s: %w", basePath, err)
		}
		if sum != baseSHA256 {
			return entry, "", fmt.Errorf("vm: local base image %s differs from the one this bundle was made against "+
				"(sha256 %s, want %s), importing it would corrupt the disk", entry.ID, sum, baseSHA256)
		}
	}
	return entry, basePath, nil
}
