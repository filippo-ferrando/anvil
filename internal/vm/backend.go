//go:build linux

// Package vm implements backend.Backend for VM instances: resolving an
// image, building a disk overlay and cloud-init seed, and running QEMU.
package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/cloudinit"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/network"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

// Source is the subset of *store.Store's methods Backend needs to
// resolve cloud-init configs and VM mirrors.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
	GetCloudInit(name string) (store.CloudInitConfig, error)
}

// Backend implements backend.Backend for instance.KindVM.
type Backend struct {
	Catalog   *image.Catalog
	Vault     *image.Vault
	Seed      cloudinit.Builder
	Source    Source
	Networker Networker
	Devices   DeviceClaimer

	// The last merged catalog and the mirror manifests it came from.
	catalogMu   sync.Mutex
	catalogFrom []image.MirrorManifest
	catalogMem  *image.Catalog

	vmTable[*qemu.Process]
	guestTracker

	// Guarded by mu, like running.
	virtiofs map[string]map[string]*qemu.Virtiofsd // instance ID -> mount tag -> its virtiofsd
	held     map[string]string                     // passthrough device sysfs name -> instance ID
}

var (
	_ instance.Backend    = (*Backend)(nil)
	_ instance.Reconciler = (*Backend)(nil)
	_ instance.Mounter    = (*Backend)(nil)
	_ instance.Forker     = (*Backend)(nil)
	_ Networker           = network.LinuxBridge{}
)

func NewBackend(catalog *image.Catalog, vault *image.Vault, source Source) *Backend {
	return &Backend{
		Catalog:   catalog,
		Vault:     vault,
		Seed:      cloudinit.NewBuilder(),
		Source:    source,
		Networker: network.LinuxBridge{},
		Devices:   hostdev.Client{Socket: config.DevHelperSocket()},
		vmTable:   newVMTable[*qemu.Process](),
		guestTracker: newGuestTracker(func(id string) *qemu.GuestAgent {
			return qemu.NewGuestAgent(guestAgentSocket(id))
		}, "powerdown"),
		virtiofs: make(map[string]map[string]*qemu.Virtiofsd),
		held:     make(map[string]string),
	}
}

// EffectiveCatalog merges the base catalog with every enabled VM mirror
// currently in the registry, using each mirror's cached manifest.
func (b *Backend) EffectiveCatalog() (*image.Catalog, error) {
	mirrors, err := b.Source.ListMirrors(store.MirrorKindVM)
	if err != nil {
		return nil, fmt.Errorf("vm: listing mirrors: %w", err)
	}
	var manifests []image.MirrorManifest
	for _, m := range mirrors {
		if !m.Enabled || m.ManifestJSON == "" {
			continue
		}
		manifests = append(manifests, image.MirrorManifest{ManifestJSON: m.ManifestJSON, Priority: m.Priority})
	}
	if len(manifests) == 0 {
		return b.Catalog, nil
	}
	// Parsing every manifest again is the costly part, so reuse the last merge while the mirrors match.
	b.catalogMu.Lock()
	defer b.catalogMu.Unlock()
	if b.catalogMem != nil && slices.Equal(b.catalogFrom, manifests) {
		return b.catalogMem, nil
	}
	merged, err := b.Catalog.WithMirrors(manifests)
	if err != nil {
		return nil, err
	}
	b.catalogFrom, b.catalogMem = manifests, merged
	return merged, nil
}

// ListCatalog returns every distro entry EffectiveCatalog resolves against.
func (b *Backend) ListCatalog() ([]image.DistroEntry, error) {
	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return nil, err
	}
	return catalog.List(), nil
}

func (b *Backend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Create called with a nil VMSpec")
	}
	v := spec.VM

	if v.MACAddress == "" {
		v.MACAddress = instance.DefaultMAC(spec.ID)
	} else if _, err := net.ParseMAC(v.MACAddress); err != nil {
		return fmt.Errorf("vm: invalid MAC address %q: %w", v.MACAddress, err)
	}

	dir := instanceDir(spec.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}

	if v.SourceDiskPath != "" {
		return b.adoptMigratedDisk(ctx, spec, dir, progress)
	}

	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return err
	}
	v.Arch = entry.Arch
	v.DefaultUser = entry.DefaultUser

	diskPath := filepath.Join(dir, "disk.qcow2")
	if err := b.Vault.OverlayFor(ctx, entry, diskPath, v.DiskGiB, progress); err != nil {
		return fmt.Errorf("vm: preparing disk: %w", err)
	}
	v.DiskPath = diskPath

	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.buildSeed(spec)
}

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

// Fork copies source's disk into dest's dir, preserving the backing-file pointer. A running
// source is copied live and point-in-time (see forkLive); a stopped one with qemu-img.
func (b *Backend) Fork(ctx context.Context, source, dest *instance.Spec, progress func(status string)) error {
	if source.VM == nil || dest.VM == nil {
		return fmt.Errorf("vm: Fork called with a nil VMSpec")
	}

	backing, err := image.BackingFile(source.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: inspecting %s's disk: %w", source.Name, err)
	}
	if backing == "" {
		return fmt.Errorf("vm: %s's disk has no backing file to preserve", source.Name)
	}

	dir := instanceDir(dest.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	diskPath := filepath.Join(dir, "disk.qcow2")

	proc, running := b.lookup(source.ID)
	if running && proc.QMP != nil {
		if err := b.forkLive(ctx, source, proc, backing, diskPath, progress); err != nil {
			_ = os.Remove(diskPath)
			return err
		}
	} else {
		if progress != nil {
			progress("copying disk")
		}
		// -U still opens a disk some untracked QEMU might hold locked.
		if err := qemuImg(ctx, "forking disk", "convert", "-U", "-O", "qcow2",
			"-o", "backing_file="+backing+",backing_fmt=qcow2", source.VM.DiskPath, diskPath); err != nil {
			return err
		}
	}
	dest.VM.DiskPath = diskPath

	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.buildSeed(dest)
}

// validSnapshotName restricts names to a safe token, since qemu-img's CLI
// args and the savevm/delvm HMP lines are built by string concatenation.
var validSnapshotName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func checkSnapshotName(name string) error {
	if !validSnapshotName.MatchString(name) {
		return fmt.Errorf("vm: snapshot name must be non-empty and contain only letters, digits, '_', '-', or '.' (got %q)", name)
	}
	return nil
}

// CreateSnapshot creates a named QCOW2 snapshot of spec's disk, live via
// QMP's savevm if running (no stop needed), or offline via qemu-img otherwise.
func (b *Backend) CreateSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: CreateSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	existing, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: listing %s's existing snapshots: %w", spec.Name, err)
	}
	for _, s := range existing {
		if s.Name == name {
			return fmt.Errorf("vm: %s already has a snapshot named %q; delete it first", spec.Name, name)
		}
	}

	proc, running := b.lookup(spec.ID)

	if running && proc.QMP != nil {
		if _, err := proc.QMP.SaveVM(ctx, name); err != nil {
			return fmt.Errorf("vm: creating snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, true, "creating")
	}

	return qemuImg(ctx, fmt.Sprintf("creating snapshot %q", name), "snapshot", "-c", name, spec.VM.DiskPath)
}

// RestoreSnapshot resets spec's disk only, never resuming a saved VM/RAM state.
// qemu-img can't touch a disk a running QEMU holds locked, so a running instance is stopped first, then restarted.
func (b *Backend) RestoreSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RestoreSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	return restoreSnapshot(ctx, b, spec, name, func() error {
		return image.QemuImg(ctx, "snapshot", "-a", name, spec.VM.DiskPath)
	})
}

// DeleteSnapshot removes a previously created snapshot. Live if spec is
// running (QMP's delvm), otherwise via qemu-img's offline "snapshot -d".
func (b *Backend) DeleteSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: DeleteSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)

	if running && proc.QMP != nil {
		if _, err := proc.QMP.DeleteVMSnapshot(ctx, name); err != nil {
			return fmt.Errorf("vm: deleting snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, false, "deleting")
	}

	return qemuImg(ctx, fmt.Sprintf("deleting snapshot %q", name), "snapshot", "-d", name, spec.VM.DiskPath)
}

// ListSnapshots returns every snapshot currently recorded on spec's disk.
func (b *Backend) ListSnapshots(ctx context.Context, spec *instance.Spec) ([]instance.Snapshot, error) {
	if spec.VM == nil {
		return nil, fmt.Errorf("vm: ListSnapshots called with a nil VMSpec")
	}
	snaps, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return nil, err
	}
	out := make([]instance.Snapshot, len(snaps))
	for i, s := range snaps {
		out[i] = instance.Snapshot{Name: s.Name, CreatedAt: s.CreatedAt, HasVMState: s.HasVMState}
	}
	return out, nil
}

// verifySnapshotPresence confirms a live savevm/delvm took effect by
// re-reading the disk's snapshot table, not trusting its HMP text output.
func (b *Backend) verifySnapshotPresence(spec *instance.Spec, name string, wantPresent bool, verb string) error {
	snaps, err := image.ListSnapshots(spec.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: verifying snapshot %q after %s it: %w", name, verb, err)
	}
	present := false
	for _, s := range snaps {
		if s.Name == name {
			present = true
			break
		}
	}
	if present != wantPresent {
		return fmt.Errorf("vm: %s snapshot %q on %s appeared to succeed but didn't take effect", verb, name, spec.Name)
	}
	return nil
}

// buildSeed (re)builds spec's cloud-init seed ISO from its current
// user-data, SSH keys, and mounts, overwriting v.SeedISOPath.
func (b *Backend) buildSeed(spec *instance.Spec) error {
	v := spec.VM
	dir := instanceDir(spec.ID)

	userData := v.CloudInitUserData
	if userData == "" && v.CloudInitName != "" {
		cfg, err := b.Source.GetCloudInit(v.CloudInitName)
		if err != nil {
			return fmt.Errorf("vm: resolving cloud-init config %q: %w", v.CloudInitName, err)
		}
		userData = cfg.Content
	}
	userData, err := seedUserData(v, userData)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}

	// Bumping Generation into instance-id forces cloud-init to treat a
	// reconfiguration as a new instance and re-run every module.
	metaData := fmt.Sprintf("instance-id: %s-gen%d\nlocal-hostname: %s\n", spec.ID, v.Generation, spec.Name)

	seedPath := filepath.Join(dir, "seed.iso")
	seed := cloudinit.Seed{UserData: userData, MetaData: metaData, NetworkConfig: bridgeNetworkConfig(v, v.MACAddress)}
	if err := b.Seed.Build(seed, seedPath); err != nil {
		return fmt.Errorf("vm: building cloud-init seed: %w", err)
	}
	v.SeedISOPath = seedPath
	return nil
}

// Start is idempotent for an already-running instance, but rejects a
// concurrent Start call for the same spec.ID rather than double-spawning.
func (b *Backend) Start(ctx context.Context, spec *instance.Spec) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Start called with a nil VMSpec")
	}
	v := spec.VM

	running, done, err := b.beginStart(spec.ID)
	if running || err != nil {
		return err // already running, Start is idempotent
	}
	defer done()

	arch := v.Arch
	if arch == "" {
		arch = "x86_64"
	}
	// An empty seed path (e.g. cleared by the store's record migration) gets a fresh seed.
	if v.SeedISOPath == "" {
		if err := b.buildSeed(spec); err != nil {
			return err
		}
	}

	dir := instanceDir(spec.ID)
	cfg := qemu.Config{
		Arch:          arch,
		CPUs:          v.CPUs,
		MemoryMiB:     v.MemoryMiB,
		DiskPath:      v.DiskPath,
		SeedISOPath:   v.SeedISOPath,
		QMPSocket:     filepath.Join(dir, "qmp.sock"),
		SerialLogPath: filepath.Join(dir, "console.log"),
		KVM:           kvmAvailable() && instance.HostArch() == arch,
		Disk:          qemu.ProbeDiskTuning(ctx, v.DiskPath),

		GuestAgentSocket: guestAgentSocket(spec.ID),
	}
	cfg.MaxCPUs, cfg.MaxMemoryMiB = hotplugHeadroom(arch, v.CPUs, v.MemoryMiB)
	if v.NetworkMode == "bridge" {
		// Attach to the intent's shared network via the backend's Networker
		// (Linux tap+bridge by default; see Networker's doc for why it's swappable).
		// A tap left by a QEMU that died while anvild was down would make Attach fail.
		_ = b.Networker.Detach(spec.ID)
		deviceName, err := b.Networker.Attach(spec.ID, v.BridgeInterface)
		if err != nil {
			return fmt.Errorf("vm: attaching to bridge %s: %w", v.BridgeInterface, err)
		}
		cfg.BridgeTapDevice = deviceName
		cfg.MACAddress = v.MACAddress
		v.SSHPort = 0
	} else {
		// Standalone instance: SLIRP with an SSH host-forward plus any
		// published ports.
		sshPort, err := allocateFreePort()
		if err != nil {
			return fmt.Errorf("vm: allocating SSH forward port: %w", err)
		}
		cfg.SLIRPHostForwards = []qemu.HostForward{{HostPort: sshPort, GuestPort: 22, Protocol: "tcp"}}
		for _, p := range v.Ports {
			proto := instance.Protocol(p.Protocol)
			cfg.SLIRPHostForwards = append(cfg.SLIRPHostForwards, qemu.HostForward{
				HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: proto,
			})
		}
		v.SSHPort = sshPort
	}

	// Undoes everything set up below, so a new failure path can't forget a piece.
	// killVirtiofsds is a no-op when none were started.
	var claims []hostdev.Claim
	unwind := func() {
		b.killVirtiofsds(spec.ID)
		if cfg.BridgeTapDevice != "" {
			_ = b.Networker.Detach(spec.ID)
		}
		if len(v.Devices) > 0 {
			b.releaseDevices(spec.ID, claims)
			b.unhold(spec.ID)
			removeRuntimeState(dir)
		}
	}

	claims, err = b.claimDevices(ctx, spec, &cfg)
	if err != nil {
		unwind()
		return err
	}
	rt := runtimeState{Devices: claims}
	if len(claims) > 0 {
		// Saved before QEMU runs, so a crash in between still lets Reconcile give them back.
		if err := saveRuntimeState(dir, rt); err != nil {
			unwind()
			return fmt.Errorf("vm: saving claimed devices: %w", err)
		}
	}

	mounts, err := b.startVirtiofsds(spec)
	if err != nil {
		unwind()
		return err
	}
	cfg.Mounts = mounts
	if spec.GUI {
		cfg.ToolsShare = b.startToolsShare(spec)
	}

	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		unwind()
		return fmt.Errorf("vm: spawning qemu: %w", err)
	}
	if err := proc.AttachQMP(ctx); err != nil {
		_ = proc.Stop(ctx, 0)
		_ = proc.Close()
		unwind()
		return fmt.Errorf("vm: attaching QMP: %w", err)
	}
	rt = runtimeState{
		Pid: proc.Pid(), QMPSocket: cfg.QMPSocket, StartedAt: time.Now(),
		BootMemoryMiB: v.MemoryMiB, MaxCPUs: cfg.MaxCPUs, MaxMemoryMiB: cfg.MaxMemoryMiB,
		Devices: claims,
	}
	if err := saveRuntimeState(dir, rt); err != nil {
		log.Printf("vm: failed to persist runtime state for %s: %v", spec.ID, err)
	}

	b.track(spec.ID, proc)
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, v.NetworkMode, proc)
	return nil
}

func (b *Backend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	dir := instanceDir(spec.ID)

	proc, ok, _ := b.beginStop(spec.ID)
	if !ok {
		// Not tracked as running in this daemon process.
		removeRuntimeState(dir)
		return nil
	}
	defer b.endStop(spec.ID)

	// A connected guest agent is tried before ACPI: it doesn't depend on the guest
	// handling the power button event. ACPI and then a hard stop remain as fallbacks.
	stopErr := b.gracefulStop(ctx, spec, proc, force, timeout)
	// Clean up unconditionally: the process is gone by now either way.
	networkMode := ""
	if spec.VM != nil {
		networkMode = spec.VM.NetworkMode
	}
	b.releaseStopped(spec.ID, networkMode, proc)

	if stopErr != nil {
		return fmt.Errorf("vm: stopping instance %s: %w", spec.ID, stopErr)
	}
	return nil
}

// Reconcile re-derives an instance's real running state from the OS
// after a daemon restart, rather than trusting the registry blindly.
func (b *Backend) Reconcile(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	if spec.VM == nil {
		return instance.StateStopped, nil
	}
	dir := instanceDir(spec.ID)

	rt, found, err := loadRuntimeState(dir)
	if err != nil {
		return instance.StateStopped, fmt.Errorf("vm: reading runtime state: %w", err)
	}
	if !found {
		// Nothing was ever persisted, or it was already cleaned up.
		return instance.StateStopped, nil
	}

	proc, err := qemu.Attach(qemu.Config{QMPSocket: rt.QMPSocket}, rt.Pid, spec.VM.DiskPath)
	if err != nil {
		b.removeLeftovers(spec)
		return instance.StateStopped, nil
	}

	b.track(spec.ID, proc)
	b.holdClaims(spec.ID, rt.Devices)

	if err := proc.AttachQMP(ctx); err != nil {
		// Left running untracked, a later Start would open the same disk in a second QEMU.
		b.mu.Lock()
		delete(b.running, spec.ID)
		b.mu.Unlock()
		_ = proc.Stop(ctx, 0)
		_ = proc.Close()
		b.removeLeftovers(spec)
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d was alive but QMP was unreachable at %s, killed it: %w", rt.Pid, rt.QMPSocket, err)
	}
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, spec.VM.NetworkMode, proc)
	return instance.StateRunning, nil
}

// removeLeftovers clears what a QEMU that's gone untracked can leave: its tap
// (persistent, so it outlives QEMU) and runtime state.
func (b *Backend) removeLeftovers(spec *instance.Spec) {
	if spec.VM != nil && spec.VM.NetworkMode == "bridge" {
		if err := b.Networker.Detach(spec.ID); err != nil {
			log.Printf("vm: removing %s's tap: %v", spec.ID, err)
		}
	}
	b.releaseRuntimeDevices(spec.ID)
	removeRuntimeState(instanceDir(spec.ID))
}

func (b *Backend) Delete(ctx context.Context, spec *instance.Spec) error {
	// Best-effort stop first.
	_ = b.Stop(ctx, spec, true, 0)
	b.removeLeftovers(spec)

	dir := instanceDir(spec.ID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("vm: removing instance dir: %w", err)
	}
	return nil
}

func (b *Backend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	proc, ok := b.lookup(spec.ID)
	if !ok {
		// Not tracked in this process's memory: treated as stopped.
		return instance.StateStopped, nil
	}
	if proc.QMP == nil {
		return instance.StateStarting, nil
	}
	qs, err := proc.QMP.QueryStatus(ctx)
	if err != nil {
		return instance.StateError, err
	}
	if qs.Running {
		return instance.StateRunning, nil
	}
	return instance.StateStopped, nil
}

// maxConsoleLineBytes caps how much of console.log one tailed line is assumed
// to need, so a tail never reads the whole file.
const maxConsoleLineBytes = 512

// sendInChunks streams the rest of f to send without holding it all in memory.
func sendInChunks(f *os.File, send func([]byte) error) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("vm: reading console log: %w", err)
		}
	}
}

// Logs streams spec's console.log, QEMU's capture of the guest's serial
// console. Returns cleanly with no output if the log doesn't exist yet.
func (b *Backend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Logs called with a nil VMSpec")
	}
	path := filepath.Join(instanceDir(spec.ID), "console.log")

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("vm: opening console log: %w", err)
	}
	defer f.Close()

	if tailLines > 0 {
		// console.log is append-only and never rotated, so a long-lived VM's
		// can reach gigabytes. Read back only enough to hold the wanted lines.
		limit := int64(tailLines) * maxConsoleLineBytes
		if fi, err := f.Stat(); err == nil && fi.Size() > limit {
			if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
				return fmt.Errorf("vm: seeking console log: %w", err)
			}
		}
		data, err := io.ReadAll(f)
		if err != nil {
			return fmt.Errorf("vm: reading console log: %w", err)
		}
		if data = tailLinesOf(data, tailLines); len(data) > 0 {
			if err := send(data); err != nil {
				return err
			}
		}
	} else if err := sendInChunks(f, send); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// console.log is append-only, so keep reading from the current offset.
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := send(buf[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("vm: reading console log: %w", err)
		}
	}
}

// checkNewPort rejects a port forward spec can't take: on a bridge, or one it already has.
func checkNewPort(spec *instance.Spec, port instance.PortMapping) (string, error) {
	if spec.VM.NetworkMode == "bridge" {
		return "", fmt.Errorf("vm: %s uses bridge networking; it has its own address, so no host-forwarded ports apply", spec.Name)
	}
	proto := instance.Protocol(port.Protocol)
	for _, p := range spec.VM.Ports {
		if p.HostPort == port.HostPort && instance.Protocol(p.Protocol) == proto {
			return "", fmt.Errorf("vm: %s already forwards host port %d/%s", spec.Name, port.HostPort, proto)
		}
	}
	return proto, nil
}

// portIndex returns the index of spec's port forward for hostPort/proto.
func portIndex(spec *instance.Spec, hostPort int, proto string) (int, error) {
	idx := slices.IndexFunc(spec.VM.Ports, func(p instance.PortMapping) bool {
		return p.HostPort == hostPort && instance.Protocol(p.Protocol) == proto
	})
	if idx == -1 {
		return -1, fmt.Errorf("vm: %s has no port forward for host port %d/%s (currently exposed: %s)",
			spec.Name, hostPort, proto, instance.FormatPorts(spec.VM.Ports))
	}
	return idx, nil
}

// AddPort adds a host-to-guest SLIRP port forward to spec, applied live
// over QMP (no restart, unlike Mount) if running, else persisted for Start.
func (b *Backend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: AddPort called with a nil VMSpec")
	}
	proto, err := checkNewPort(spec, port)
	if err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)
	if running && proc.QMP != nil {
		if err := proc.QMP.AddHostForward(ctx, qemu.NetdevID, qemu.HostForward{
			HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto,
		}); err != nil {
			return fmt.Errorf("vm: adding live port forward: %w", err)
		}
	}

	spec.VM.Ports = append(spec.VM.Ports, instance.PortMapping{HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto})
	return nil
}

// RemovePort removes a port forward previously added with AddPort or at
// launch, identified by hostPort/protocol.
func (b *Backend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RemovePort called with a nil VMSpec")
	}
	proto := instance.Protocol(protocol)
	idx, err := portIndex(spec, hostPort, proto)
	if err != nil {
		return err
	}

	proc, running := b.lookup(spec.ID)
	if running && proc.QMP != nil {
		if err := proc.QMP.RemoveHostForward(ctx, qemu.NetdevID, hostPort, proto); err != nil {
			return fmt.Errorf("vm: removing live port forward: %w", err)
		}
	}

	spec.VM.Ports = append(spec.VM.Ports[:idx], spec.VM.Ports[idx+1:]...)
	return nil
}

func tailLinesOf(data []byte, n int) []byte {
	// A log almost always ends in a newline, whose trailing empty field would
	// otherwise count as one of the n lines and return n-1 real ones.
	trailing := bytes.HasSuffix(data, []byte("\n"))
	body := bytes.TrimSuffix(data, []byte("\n"))
	lines := bytes.Split(body, []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := bytes.Join(lines, []byte("\n"))
	if trailing && len(out) > 0 {
		out = append(out, '\n')
	}
	return out
}

// seedUserData adds anvil's own setup to userData: SSH keys, mounts, extra hosts, the guest
// agent and Firecracker's needs. With nothing to add userData is kept as it is.
func seedUserData(v *instance.VMSpec, userData string) (string, error) {
	fc := v.HypervisorOf() == instance.VMHypervisorFirecracker
	if len(v.SSHPublicKeys) == 0 && len(v.Mounts) == 0 && v.Generation == 0 && len(v.ExtraHosts) == 0 && v.NoGuestAgent && !fc {
		if userData == "" {
			return "#cloud-config\n{}\n", nil
		}
		return userData, nil
	}
	doc, err := parseCloudConfig(userData)
	if err != nil {
		return "", err
	}
	addSSHKeys(doc, v.SSHPublicKeys)
	line := fstabLine
	if fc {
		line = func(m instance.Mount) string { return nfsFstabLine(v.Gateway, m) }
	}
	addMounts(doc, v.Mounts, v.Generation, line)
	addExtraHosts(doc, v.ExtraHosts)
	if !v.NoGuestAgent {
		addGuestAgent(doc)
	}
	if fc {
		addFirecrackerGuest(doc, !v.NoGuestAgent)
	}
	return encodeCloudConfig(doc)
}

// addSSHKeys appends keys to doc's top-level ssh_authorized_keys list.
func addSSHKeys(doc map[string]any, keys []string) {
	if len(keys) == 0 {
		return
	}
	existing, _ := doc["ssh_authorized_keys"].([]any)
	for _, k := range keys {
		existing = append(existing, k)
	}
	doc["ssh_authorized_keys"] = existing
}

// shQuote single-quotes s for safe interpolation into a POSIX shell command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// addExtraHosts adds one /etc/hosts entry per host via idempotent
// bootcmd lines, sorted by name for deterministic output.
func addExtraHosts(doc map[string]any, hosts map[string]string) {
	if len(hosts) == 0 {
		return
	}
	bootcmd, _ := doc["bootcmd"].([]any)
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		quoted := shQuote(fmt.Sprintf("%s %s", hosts[name], name))
		bootcmd = append(bootcmd, fmt.Sprintf(`grep -qxF %s /etc/hosts || echo %s >> /etc/hosts`, quoted, quoted))
	}
	doc["bootcmd"] = bootcmd
}

// parseCloudConfig decodes a "#cloud-config" user-data document; empty input gives an empty map.
func parseCloudConfig(userData string) (map[string]any, error) {
	body := strings.TrimPrefix(userData, "#cloud-config\n")
	doc := map[string]any{}
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return nil, fmt.Errorf("parsing existing cloud-init user-data: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func encodeCloudConfig(doc map[string]any) (string, error) {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}

// guestAgentPackage is the qemu-guest-agent package name, the same on every catalog distro.
const guestAgentPackage = "qemu-guest-agent"

// guestAgentStartCmd starts the agent right after install: the udev rule that normally starts it
// only fires on boot. The OpenRC branch covers Alpine.
const guestAgentStartCmd = "(systemctl enable qemu-guest-agent 2>/dev/null; systemctl start qemu-guest-agent) || " +
	"(rc-update add qemu-guest-agent default && rc-service qemu-guest-agent start) || true"

// addGuestAgent adds qemu-guest-agent to doc's "packages" and a runcmd that starts it.
func addGuestAgent(doc map[string]any) {
	packages, _ := doc["packages"].([]any)
	if !slices.Contains(packages, any(guestAgentPackage)) {
		packages = append(packages, guestAgentPackage)
	}
	doc["packages"] = packages
	runcmd, _ := doc["runcmd"].([]any)
	doc["runcmd"] = append(runcmd, guestAgentStartCmd)
}

// fcAgentPort is the guest vsock port qemu-guest-agent listens on in a Firecracker VM.
const fcAgentPort = 1024

// fcAgentUnit runs qemu-guest-agent on vsock: the distro's own unit only starts with the
// virtio-serial port QEMU provides.
var fcAgentUnit = fmt.Sprintf(`[Unit]
Description=QEMU guest agent on vsock (anvil)

[Service]
ExecStart=/bin/sh -c 'exec qemu-ga --method=vsock-listen --path=%d:%d'
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, firecracker.GuestCID, fcAgentPort)

// fcAgentStartCmd enables the vsock unit, or on OpenRC (Alpine) points the stock service at vsock.
const fcAgentStartCmd = "(systemctl daemon-reload && systemctl enable --now anvil-guest-agent) || " +
	"(rc-update add qemu-guest-agent default && rc-service qemu-guest-agent restart) || true"

// fcPoweroffCmd makes a systemd guest's poweroff a reboot: on x86_64 Firecracker only
// exits on a guest reboot, while a poweroff leaves its process running.
const fcPoweroffCmd = "[ -d /run/systemd/system ] && ln -sf /usr/lib/systemd/system/reboot.target /etc/systemd/system/poweroff.target || true"

// addFirecrackerGuest adds what a Firecracker guest needs on top of the common user-data:
// poweroff as reboot, and with agent the guest agent moved to vsock.
func addFirecrackerGuest(doc map[string]any, agent bool) {
	bootcmd, _ := doc["bootcmd"].([]any)
	doc["bootcmd"] = append(bootcmd, fcPoweroffCmd)
	if agent {
		files, _ := doc["write_files"].([]any)
		doc["write_files"] = append(files,
			map[string]any{"path": "/etc/systemd/system/anvil-guest-agent.service", "content": fcAgentUnit},
			map[string]any{"path": "/etc/conf.d/qemu-guest-agent",
				"content": fmt.Sprintf("GA_METHOD=\"vsock-listen\"\nGA_PATH=\"%d:%d\"\n", firecracker.GuestCID, fcAgentPort)},
		)
		runcmd, _ := doc["runcmd"].([]any)
		// The stock start command is for virtio-serial; this one replaces it.
		runcmd = slices.DeleteFunc(runcmd, func(c any) bool { return c == guestAgentStartCmd })
		doc["runcmd"] = append(runcmd, fcAgentStartCmd)
	}
}

// bridgeNetworkConfig returns a cloud-init network-config statically assigning
// v.StaticIP/v.Gateway to mac, or "" when v has no static address (SLIRP).
func bridgeNetworkConfig(v *instance.VMSpec, mac string) string {
	if v.StaticIP == "" {
		return ""
	}
	cfg := fmt.Sprintf(`network:
  version: 2
  ethernets:
    anvil0:
      match:
        macaddress: "%s"
      dhcp4: false
      addresses: [%s]
      gateway4: %s
`, mac, v.StaticIP, v.Gateway)
	if len(v.DNSServers) > 0 {
		cfg += fmt.Sprintf("      nameservers:\n        addresses: [%s]\n", strings.Join(v.DNSServers, ", "))
		if len(v.DNSSearch) > 0 {
			cfg += fmt.Sprintf("        search: [%s]\n", strings.Join(v.DNSSearch, ", "))
		}
	}
	return cfg
}

func kvmAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// recentPorts remembers ports allocateFreePort handed out lately, so VMs started
// in parallel never get the same port before QEMU has bound it.
var (
	recentPortsMu sync.Mutex
	recentPorts   = map[int]time.Time{}
)

const recentPortTTL = time.Minute

// allocateFreePort asks the OS for an ephemeral port by briefly binding
// to port 0, reading back what was assigned, then releasing it.
func allocateFreePort() (int, error) {
	recentPortsMu.Lock()
	defer recentPortsMu.Unlock()
	now := time.Now()
	for p, at := range recentPorts {
		if now.Sub(at) > recentPortTTL {
			delete(recentPorts, p)
		}
	}
	for range 16 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if _, taken := recentPorts[port]; !taken {
			recentPorts[port] = now
			return port, nil
		}
	}
	return 0, fmt.Errorf("vm: no free port found")
}
