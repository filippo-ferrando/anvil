package image

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FCKernel is anvil's Firecracker guest kernel for one arch.
type FCKernel struct {
	Arch    string `json:"arch"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// FCDistro marks a distro id of the distros list as bootable under Firecracker.
type FCDistro struct {
	ID string `json:"id"`
	// KernelArgs is added to the boot command line, e.g. the root subvolume of a btrfs root.
	KernelArgs string `json:"kernel_args"`
}

// FirecrackerSection is a manifest's optional Firecracker catalog: kernels plus the distro
// ids known to boot with them. Images, checksums and mirrors still come from the distros list.
type FirecrackerSection struct {
	Kernels []FCKernel `json:"kernels"`
	Distros []FCDistro `json:"distros"`
}

// best returns the highest-priority entry matching, the earliest on a tie.
func best[T any](entries []prioritized[T], match func(T) bool) (T, bool) {
	var found *prioritized[T]
	for i := range entries {
		if match(entries[i].v) && (found == nil || entries[i].priority > found.priority) {
			found = &entries[i]
		}
	}
	if found == nil {
		var zero T
		return zero, false
	}
	return found.v, true
}

// FirecrackerKernel returns the guest kernel for arch.
func (c *Catalog) FirecrackerKernel(arch string) (FCKernel, error) {
	if k, ok := best(c.fcKernels, func(k FCKernel) bool { return k.Arch == arch }); ok {
		return k, nil
	}
	return FCKernel{}, fmt.Errorf("image: no firecracker kernel for %s", arch)
}

// FirecrackerDistro returns id's Firecracker entry, failing for a distro not known to boot.
func (c *Catalog) FirecrackerDistro(id string) (FCDistro, error) {
	if d, ok := best(c.fcDistros, func(d FCDistro) bool { return d.ID == id }); ok {
		return d, nil
	}
	return FCDistro{}, fmt.Errorf("image: %s isn't in the firecracker catalog (see `anvil find`)", id)
}

// FCCache keeps what Firecracker boots from: the kernels and raw copies of the
// qcow2 base images the Vault downloads.
type FCCache struct {
	Dir   string
	Vault *Vault
}

// KernelEnv overrides the catalog kernel with a local file, for development and tests.
const KernelEnv = "ANVIL_FC_KERNEL"

// Kernel returns k's local path, downloading it on first use.
func (c *FCCache) Kernel(ctx context.Context, k FCKernel, progress func(status string)) (string, error) {
	if path := os.Getenv(KernelEnv); path != "" {
		return path, nil
	}
	dest := filepath.Join(c.Dir, "anvil-fc-kernel-"+k.Version+"-"+k.Arch)
	entry := DistroEntry{ID: "firecracker kernel " + k.Version, Arch: k.Arch, URL: k.URL, SHA256: k.SHA256}
	if err := c.Vault.Downloader.Fetch(ctx, entry, dest, progress); err != nil {
		return "", err
	}
	return dest, nil
}

// RawBase returns a raw copy of entry's base image, downloading and converting it on
// first use. A base newer than its raw copy (downloaded again) is converted again.
func (c *FCCache) RawBase(ctx context.Context, entry DistroEntry, progress func(status string)) (string, error) {
	base, err := c.Vault.Ensure(ctx, entry, progress)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(c.Dir, entry.ID+"-"+entry.Arch+".raw")
	if fresh(dest, base) {
		return dest, nil
	}
	if err := os.MkdirAll(c.Dir, 0o750); err != nil {
		return "", fmt.Errorf("image: creating %s: %w", c.Dir, err)
	}
	if progress != nil {
		progress("converting " + entry.ID + " to a raw disk for firecracker")
	}
	// A per-call temp name, so two launches converting the same base can't clobber each other.
	tmpFile, err := os.CreateTemp(c.Dir, filepath.Base(dest)+".part-*")
	if err != nil {
		return "", fmt.Errorf("image: creating a temp file in %s: %w", c.Dir, err)
	}
	tmp := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmp)
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "qcow2", "-O", "raw", base, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("image: converting %s to raw: %w: %s", base, err, out)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("image: %w", err)
	}
	return dest, nil
}

// List returns the raw copies and kernels in the cache. A kernel's ID is its file name.
func (c *FCCache) List() ([]CachedImage, error) {
	entries, err := os.ReadDir(c.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("image: listing %s: %w", c.Dir, err)
	}
	var out []CachedImage
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasSuffix(e.Name(), checksumSuffix) {
			continue
		}
		img := CachedImage{Path: filepath.Join(c.Dir, e.Name()), SizeBytes: info.Size()}
		switch name := e.Name(); {
		case strings.HasSuffix(name, ".raw"):
			img.ID, img.Arch = parseCachedFilename(strings.TrimSuffix(name, ".raw") + ".qcow2")
			img.Kind = ImageKindFirecrackerRaw
		case strings.HasPrefix(name, "anvil-fc-kernel-"):
			img.ID, img.Kind = name, ImageKindFirecrackerKernel
			img.Arch = name[strings.LastIndex(name, "-")+1:]
		default:
			continue // temp files, the GUI tools ISO
		}
		out = append(out, img)
	}
	return out, nil
}

// fresh reports whether path exists and is at least as new as source.
func fresh(path, source string) bool {
	dst, err := os.Stat(path)
	if err != nil {
		return false
	}
	src, err := os.Stat(source)
	return err == nil && !dst.ModTime().Before(src.ModTime())
}

// ReflinkDisk copies src to dst only when the filesystem can share the blocks, so it's
// instant; otherwise it fails and leaves no dst.
func ReflinkDisk(ctx context.Context, src, dst string) error {
	if out, err := exec.CommandContext(ctx, "cp", "--reflink=always", src, dst).CombinedOutput(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("image: copy-on-write copy of %s: %w: %s", src, err, out)
	}
	return nil
}

// CloneDisk copies src to dst as a reflink where the filesystem supports it (btrfs,
// xfs), sparse otherwise, then grows dst to sizeGiB. sizeGiB 0 keeps src's size.
func CloneDisk(ctx context.Context, src, dst string, sizeGiB int64) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	size := sizeGiB * bytesPerGiB
	if size > 0 && size < info.Size() {
		return fmt.Errorf("image: requested disk %dGiB is smaller than the base image (%.1fGiB), can't shrink it",
			sizeGiB, float64(info.Size())/bytesPerGiB)
	}
	cmd := exec.CommandContext(ctx, "cp", "--reflink=auto", "--sparse=always", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("image: copying %s: %w: %s", src, err, out)
	}
	if size > info.Size() {
		if err := os.Truncate(dst, size); err != nil {
			return fmt.Errorf("image: growing %s: %w", dst, err)
		}
	}
	return nil
}
