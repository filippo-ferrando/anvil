package image

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const bytesPerGiB = 1 << 30

// Vault is the two-tier base-image store: PreparedDir holds downloaded,
// shared base images (one per catalog entry); OverlayFor creates a
// per-instance QCOW2 backing-file overlay onto one of them, resized to the
// requested disk size.
//
// Refcounting/pruning of prepared images (so a base image can be garbage
// collected once no instance overlays it) is not implemented yet — it
// needs the bbolt-backed registry (internal/store, not yet written) to
// track which instances reference which prepared image; this type only
// handles the download/overlay mechanics.
type Vault struct {
	PreparedDir string
	Downloader  *Downloader
}

func NewVault(preparedDir string) *Vault {
	return &Vault{PreparedDir: preparedDir, Downloader: NewDownloader()}
}

// preparedPath returns where entry's base image is cached.
func (v *Vault) preparedPath(entry DistroEntry) string {
	return filepath.Join(v.PreparedDir, entry.ID+"-"+entry.Arch+".qcow2")
}

// Ensure downloads entry's base image into the vault if not already
// present (see Downloader.Fetch for the idempotency/dedup rules) and
// returns its local path.
func (v *Vault) Ensure(entry DistroEntry) (string, error) {
	dest := v.preparedPath(entry)
	if err := v.Downloader.Fetch(entry, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// OverlayFor creates a new QCOW2 overlay at overlayPath backed by entry's
// prepared image (downloading it first if needed). diskGiB of 0 means
// "just use the base image's own size, whatever that is". Requires
// `qemu-img` on PATH (part of the qemu-base package already depended on
// for qemu-system-x86_64 itself).
func (v *Vault) OverlayFor(entry DistroEntry, overlayPath string, diskGiB int64) error {
	base, err := v.Ensure(entry)
	if err != nil {
		return err
	}

	// entry.MinDiskGiB used to be the floor this validated against, but a
	// catalog value can go stale (a distro's cloud image can grow release
	// over release) in a way this live query can't — a real launch hit
	// exactly that: min_disk_gib said 3 for ubuntu-24.04, but the actual
	// downloaded image was already bigger, so resizing "down" to 3 failed
	// outright (qemu-img refuses to shrink without --shrink). Query the
	// base image's real virtual size instead of trusting the catalog.
	baseSizeBytes, err := qemuImgVirtualSize(base)
	if err != nil {
		return fmt.Errorf("image: inspecting base image: %w", err)
	}

	var requestedBytes int64
	if diskGiB > 0 {
		requestedBytes = diskGiB * bytesPerGiB
		if requestedBytes < baseSizeBytes {
			return fmt.Errorf("image: requested disk %dGiB is smaller than %s's base image (%.1fGiB), can't shrink it",
				diskGiB, entry.ID, float64(baseSizeBytes)/bytesPerGiB)
		}
	}

	if err := os.MkdirAll(filepath.Dir(overlayPath), 0o750); err != nil {
		return fmt.Errorf("image: creating instance dir: %w", err)
	}

	createCmd := exec.Command("qemu-img", "create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", base,
		overlayPath,
	)
	if out, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("image: qemu-img create failed: %w: %s", err, string(out))
	}

	// Only resize when actually growing — a fresh overlay already reports
	// the backing file's own virtual size, so "resizing" to that same size
	// (diskGiB unset) or smaller is exactly the shrink qemu-img refuses.
	if requestedBytes > baseSizeBytes {
		resizeCmd := exec.Command("qemu-img", "resize", overlayPath, fmt.Sprintf("%dG", diskGiB))
		if out, err := resizeCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("image: qemu-img resize failed: %w: %s", err, string(out))
		}
	}
	return nil
}

type qemuImgInfo struct {
	VirtualSize     int64  `json:"virtual-size"`
	BackingFilename string `json:"backing-filename"`
}

func qemuImgInspect(path string) (qemuImgInfo, error) {
	cmd := exec.Command("qemu-img", "info", "--output=json", path)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return qemuImgInfo{}, fmt.Errorf("qemu-img info: %w: %s", err, exitErr.Stderr)
		}
		return qemuImgInfo{}, fmt.Errorf("qemu-img info: %w", err)
	}
	var info qemuImgInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return qemuImgInfo{}, fmt.Errorf("parsing qemu-img info output: %w", err)
	}
	return info, nil
}

func qemuImgVirtualSize(path string) (int64, error) {
	info, err := qemuImgInspect(path)
	if err != nil {
		return 0, err
	}
	return info.VirtualSize, nil
}

// BackingFile returns the backing file path qemu-img reports for the qcow2
// image at path (empty if it has none) — used to check whether a prepared
// base image is still referenced by some instance's overlay disk before
// deleting it (see internal/daemon's ImageServer.Delete).
func BackingFile(path string) (string, error) {
	info, err := qemuImgInspect(path)
	if err != nil {
		return "", err
	}
	return info.BackingFilename, nil
}

// CachedImage is one base image currently sitting in the vault's prepared
// tier, as actually found on disk (not from the catalog — this reflects
// reality even after a mirror is removed or a distro version bumped and
// the catalog no longer references a given file).
type CachedImage struct {
	ID        string
	Arch      string
	Path      string
	SizeBytes int64
}

// List returns every base image currently cached in PreparedDir.
func (v *Vault) List() ([]CachedImage, error) {
	entries, err := os.ReadDir(v.PreparedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("image: listing %s: %w", v.PreparedDir, err)
	}

	var out []CachedImage
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".qcow2") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		id, arch := parseCachedFilename(e.Name())
		out = append(out, CachedImage{
			ID:        id,
			Arch:      arch,
			Path:      filepath.Join(v.PreparedDir, e.Name()),
			SizeBytes: info.Size(),
		})
	}
	return out, nil
}

// parseCachedFilename recovers the (id, arch) pair from a prepared image's
// filename, the reverse of preparedPath's "<id>-<arch>.qcow2" convention.
func parseCachedFilename(name string) (id, arch string) {
	name = strings.TrimSuffix(name, ".qcow2")
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return name, ""
	}
	return name[:idx], name[idx+1:]
}

// Delete removes a cached base image by path. It's the caller's job (see
// internal/daemon's ImageServer.Delete) to have already checked it isn't
// still referenced by some instance's overlay disk — Vault itself has no
// visibility into the instance registry.
func (v *Vault) Delete(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("image: deleting %s: %w", path, err)
	}
	return nil
}
