package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const bytesPerGiB = 1 << 30

// Vault is the two-tier base-image store: PreparedDir holds shared base
// images; OverlayFor creates a per-instance overlay onto one of them.
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
// present, and returns its local path. progress is forwarded to Downloader.Fetch.
func (v *Vault) Ensure(ctx context.Context, entry DistroEntry, progress func(status string)) (string, error) {
	dest := v.preparedPath(entry)
	if err := v.Downloader.Fetch(ctx, entry, dest, progress); err != nil {
		return "", err
	}
	return dest, nil
}

// OverlayFor creates a QCOW2 overlay at overlayPath backed by entry's
// prepared image (downloaded first if needed); diskGiB 0 keeps the base size.
func (v *Vault) OverlayFor(ctx context.Context, entry DistroEntry, overlayPath string, diskGiB int64, progress func(status string)) error {
	base, err := v.Ensure(ctx, entry, progress)
	if err != nil {
		return err
	}

	// Query the base image's real virtual size instead of trusting the
	// catalog's MinDiskGiB, which can go stale.
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

	if progress != nil {
		progress("creating disk overlay")
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

	// Only resize when actually growing.
	if requestedBytes > baseSizeBytes {
		resizeCmd := exec.Command("qemu-img", "resize", overlayPath, fmt.Sprintf("%dG", diskGiB))
		if out, err := resizeCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("image: qemu-img resize failed: %w: %s", err, string(out))
		}
	}
	return nil
}

type qemuImgSnapshotInfo struct {
	Name        string `json:"name"`
	VMStateSize int64  `json:"vm-state-size"`
	DateSec     int64  `json:"date-sec"`
	DateNSec    int64  `json:"date-nsec"`
}

type qemuImgInfo struct {
	VirtualSize     int64                 `json:"virtual-size"`
	ActualSize      int64                 `json:"actual-size"`
	BackingFilename string                `json:"backing-filename"`
	Snapshots       []qemuImgSnapshotInfo `json:"snapshots"`
}

func qemuImgInspect(path string) (qemuImgInfo, error) {
	// -U opens the image read-only in shared mode, so this still works
	// against a disk a running QEMU process holds a write lock on.
	cmd := exec.Command("qemu-img", "info", "-U", "--output=json", path)
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

// BackingFile returns the backing file path qemu-img reports for the
// qcow2 image at path, or "" if it has none.
func BackingFile(path string) (string, error) {
	info, err := qemuImgInspect(path)
	if err != nil {
		return "", err
	}
	return info.BackingFilename, nil
}

// Snapshot is one internal QCOW2 snapshot recorded in a disk image's own metadata.
type Snapshot struct {
	Name      string
	CreatedAt time.Time

	// HasVMState is true when the snapshot embeds full VM state (RAM), as QMP's
	// savevm does; false for a disk-only snapshot (e.g. qemu-img snapshot -c).
	HasVMState bool
}

// ListSnapshots returns every internal snapshot recorded in path's own qcow2
// metadata, oldest first. Works even while a running QEMU process holds a write lock on the disk (qemu-img info -U opens it read-only).
func ListSnapshots(path string) ([]Snapshot, error) {
	info, err := qemuImgInspect(path)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, len(info.Snapshots))
	for i, s := range info.Snapshots {
		out[i] = Snapshot{
			Name:       s.Name,
			CreatedAt:  time.Unix(s.DateSec, s.DateNSec),
			HasVMState: s.VMStateSize > 0,
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// DiskUsage returns how many bytes of path are actually allocated on host
// disk (its sparse file's real footprint) versus its provisioned virtual size.
func DiskUsage(path string) (usedBytes, totalBytes int64, err error) {
	info, err := qemuImgInspect(path)
	if err != nil {
		return 0, 0, err
	}
	return info.ActualSize, info.VirtualSize, nil
}

// CachedImage is one base image currently sitting in the vault's
// prepared tier, as found on disk.
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

// Delete removes a cached base image by path. The caller is responsible
// for checking it isn't still referenced by an instance's overlay disk.
func (v *Vault) Delete(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("image: deleting %s: %w", path, err)
	}
	return nil
}
