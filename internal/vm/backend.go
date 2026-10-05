//go:build linux

// Package vm implements backend.Backend for VM instances: resolving an
// image, building a disk overlay and cloud-init seed, and running QEMU.
package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
	"github.com/anvil-project/anvil/internal/vm/cloudinit"
	"github.com/anvil-project/anvil/internal/vm/image"
	"github.com/anvil-project/anvil/internal/vm/network"
	"github.com/anvil-project/anvil/internal/vm/qemu"
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

	mu       sync.Mutex
	running  map[string]*qemu.Process // instance ID -> live process
	starting map[string]struct{}      // instance ID -> in-progress Start call

	guestMu   sync.Mutex
	guests    map[string]*guestState // instance ID -> guest agent poller and last report
	guestHook func(instanceID string)

	// Guarded by mu, like running.
	virtiofs map[string]map[string]*qemu.Virtiofsd // instance ID -> mount tag -> its virtiofsd
	stopping map[string]struct{}                   // instance ID -> Stop in progress, so its exit isn't reported
	exitHook func(instanceID string, state instance.State)
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
		running:   make(map[string]*qemu.Process),
		starting:  make(map[string]struct{}),
		guests:    make(map[string]*guestState),
		virtiofs:  make(map[string]map[string]*qemu.Virtiofsd),
		stopping:  make(map[string]struct{}),
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
	return b.Catalog.WithMirrors(manifests)
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

	dir := config.InstanceDir(spec.ID)
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
		local, err := b.matchingLocalBase(v)
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
			if local, err = b.adoptSentBase(v, dir); err != nil {
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
		cmd := exec.CommandContext(ctx, "qemu-img", "rebase", "-u", "-F", "qcow2", "-b", basePath, diskPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("vm: rebasing migrated disk: %w: %s", err, out)
		}
		v.SourceDiskBaseSHA256 = ""
	}
	return nil
}

// matchingLocalBase returns this host's cached copy of v's base image, after
// checking it is byte-identical to the one the migrated delta was made against.
func (b *Backend) matchingLocalBase(v *instance.VMSpec) (string, error) {
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
func (b *Backend) adoptSentBase(v *instance.VMSpec, dir string) (string, error) {
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
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("vm: creating %s: %w", filepath.Dir(dest), err)
	}
	if err := moveFile(v.SourceBaseImagePath, dest); err != nil {
		return "", fmt.Errorf("vm: adopting the base image that came with the disk: %w", err)
	}
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
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", spec.VM.DiskPath, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: exporting disk: %w: %s", err, out)
	}
	return nil
}

// BaseImageChecksum returns the SHA256 of the base image spec's disk is an overlay on.
func (b *Backend) BaseImageChecksum(spec *instance.Spec) (string, error) {
	if spec.VM == nil {
		return "", fmt.Errorf("vm: BaseImageChecksum called with a nil VMSpec")
	}
	backing, err := image.BackingFile(spec.VM.DiskPath)
	if err != nil {
		return "", fmt.Errorf("vm: inspecting %s's disk: %w", spec.Name, err)
	}
	if backing == "" {
		return "", fmt.Errorf("vm: %s's disk has no base image", spec.Name)
	}
	return image.FileChecksum(backing)
}

// BaseImagePath returns the local path of the base image spec's disk is an overlay on.
func (b *Backend) BaseImagePath(spec *instance.Spec) (string, error) {
	if spec.VM == nil {
		return "", fmt.Errorf("vm: BaseImagePath called with a nil VMSpec")
	}
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
	backing, err := image.BackingFile(spec.VM.DiskPath)
	if err != nil {
		return fmt.Errorf("vm: inspecting %s's disk: %w", spec.Name, err)
	}
	if backing == "" {
		return fmt.Errorf("vm: %s's disk has no base image", spec.Name)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return fmt.Errorf("vm: creating export destination dir: %w", err)
	}
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", "-B", backing, "-F", "qcow2", spec.VM.DiskPath, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: exporting disk delta: %w: %s", err, out)
	}
	return nil
}

// PrepareImportedDisk rebases an imported disk's backing file onto this
// host's own base image, since it still points at its original export host.
// A non-empty baseSHA256 must match that base image, or the disk is rejected.
func (b *Backend) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath, baseSHA256 string) error {
	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return err
	}
	entry, err := catalog.Find(imageRef, arch)
	if err != nil {
		return err
	}
	basePath, err := b.Vault.Ensure(ctx, entry, nil)
	if err != nil {
		return fmt.Errorf("vm: preparing imported disk's base image: %w", err)
	}
	if baseSHA256 != "" {
		sum, err := image.FileChecksum(basePath)
		if err != nil {
			return fmt.Errorf("vm: hashing base image %s: %w", basePath, err)
		}
		if sum != baseSHA256 {
			return fmt.Errorf("vm: local base image %s differs from the one this bundle was made against "+
				"(sha256 %s, want %s), importing it would corrupt the disk", entry.ID, sum, baseSHA256)
		}
	}
	cmd := exec.CommandContext(ctx, "qemu-img", "rebase", "-u", "-F", "qcow2", "-b", basePath, diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: rebasing imported disk: %w: %s", err, out)
	}
	return nil
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

	dir := config.InstanceDir(dest.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	diskPath := filepath.Join(dir, "disk.qcow2")

	b.mu.Lock()
	proc, running := b.running[source.ID]
	b.mu.Unlock()
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
		cmd := exec.CommandContext(ctx, "qemu-img", "convert",
			"-U",
			"-O", "qcow2",
			"-o", "backing_file="+backing+",backing_fmt=qcow2",
			source.VM.DiskPath, diskPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("vm: forking disk: %w: %s", err, out)
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

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()

	if running && proc.QMP != nil {
		if _, err := proc.QMP.SaveVM(ctx, name); err != nil {
			return fmt.Errorf("vm: creating snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, true, "creating")
	}

	cmd := exec.CommandContext(ctx, "qemu-img", "snapshot", "-c", name, spec.VM.DiskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: creating snapshot %q: %w: %s", name, err, out)
	}
	return nil
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

	b.mu.Lock()
	_, running := b.running[spec.ID]
	b.mu.Unlock()

	if running {
		if err := b.Stop(ctx, spec, false, 30*time.Second); err != nil {
			return fmt.Errorf("vm: stopping %s to restore snapshot %q: %w", spec.Name, name, err)
		}
	}

	cmd := exec.CommandContext(ctx, "qemu-img", "snapshot", "-a", name, spec.VM.DiskPath)
	out, applyErr := cmd.CombinedOutput()

	if running {
		if err := b.Start(ctx, spec); err != nil {
			if applyErr != nil {
				return fmt.Errorf("vm: restoring snapshot %q: %w: %s (restarting %s afterward also failed: %v)",
					name, applyErr, out, spec.Name, err)
			}
			return fmt.Errorf("vm: restarting %s after restoring snapshot %q: %w", spec.Name, name, err)
		}
	}
	if applyErr != nil {
		return fmt.Errorf("vm: restoring snapshot %q: %w: %s", name, applyErr, out)
	}
	return nil
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

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()

	if running && proc.QMP != nil {
		if _, err := proc.QMP.DeleteVMSnapshot(ctx, name); err != nil {
			return fmt.Errorf("vm: deleting snapshot %q: %w", name, err)
		}
		return b.verifySnapshotPresence(spec, name, false, "deleting")
	}

	cmd := exec.CommandContext(ctx, "qemu-img", "snapshot", "-d", name, spec.VM.DiskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: deleting snapshot %q: %w: %s", name, err, out)
	}
	return nil
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
	dir := config.InstanceDir(spec.ID)

	userData := v.CloudInitUserData
	if userData == "" && v.CloudInitName != "" {
		cfg, err := b.Source.GetCloudInit(v.CloudInitName)
		if err != nil {
			return fmt.Errorf("vm: resolving cloud-init config %q: %w", v.CloudInitName, err)
		}
		userData = cfg.Content
	}
	userData, err := mergeSSHKeys(userData, v.SSHPublicKeys)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}
	userData, err = mergeMounts(userData, v.Mounts, v.Generation)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}
	userData, err = mergeExtraHosts(userData, v.ExtraHosts)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}
	if !v.NoGuestAgent {
		userData, err = mergeGuestAgent(userData)
		if err != nil {
			return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
		}
	}

	// Bumping Generation into instance-id forces cloud-init to treat a
	// reconfiguration as a new instance and re-run every module.
	metaData := fmt.Sprintf("instance-id: %s-gen%d\nlocal-hostname: %s\n", spec.ID, v.Generation, spec.Name)

	seedPath := filepath.Join(dir, "seed.iso")
	seed := cloudinit.Seed{UserData: userData, MetaData: metaData, NetworkConfig: bridgeNetworkConfig(v, macFromInstanceID(spec.ID))}
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

	b.mu.Lock()
	if _, ok := b.running[spec.ID]; ok {
		b.mu.Unlock()
		return nil // already running, Start is idempotent
	}
	if _, ok := b.starting[spec.ID]; ok {
		b.mu.Unlock()
		return fmt.Errorf("vm: %s is already being started by a concurrent call", spec.ID)
	}
	b.starting[spec.ID] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.starting, spec.ID)
		b.mu.Unlock()
	}()

	arch := v.Arch
	if arch == "" {
		arch = "x86_64"
	}
	if err := b.upgradeLegacyMounts(spec); err != nil {
		return err
	}

	dir := config.InstanceDir(spec.ID)
	cfg := qemu.Config{
		Arch:          arch,
		CPUs:          v.CPUs,
		MemoryMiB:     v.MemoryMiB,
		DiskPath:      v.DiskPath,
		SeedISOPath:   v.SeedISOPath,
		QMPSocket:     filepath.Join(dir, "qmp.sock"),
		SerialLogPath: filepath.Join(dir, "console.log"),
		KVM:           kvmAvailable() && qemu.HostArch() == arch,
		Disk:          qemu.ProbeDiskTuning(ctx, v.DiskPath),

		GuestAgentSocket: guestAgentSocket(spec.ID),
	}
	cfg.MaxCPUs, cfg.MaxMemoryMiB = hotplugHeadroom(arch, v.CPUs, v.MemoryMiB)
	if v.NetworkMode == "bridge" {
		// Attach to the intent's shared network via the backend's Networker
		// (Linux tap+bridge by default; see Networker's doc for why it's swappable).
		deviceName, err := b.Networker.Attach(spec.ID, v.BridgeInterface)
		if err != nil {
			return fmt.Errorf("vm: attaching to bridge %s: %w", v.BridgeInterface, err)
		}
		cfg.BridgeTapDevice = deviceName
		cfg.MACAddress = macFromInstanceID(spec.ID)
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
	unwind := func() {
		b.killVirtiofsds(spec.ID)
		if cfg.BridgeTapDevice != "" {
			_ = b.Networker.Detach(spec.ID)
		}
	}

	mounts, err := b.startVirtiofsds(spec)
	if err != nil {
		unwind()
		return err
	}
	cfg.Mounts = mounts

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
	rt := runtimeState{
		Pid: proc.Pid(), QMPSocket: cfg.QMPSocket, StartedAt: time.Now(),
		BootMemoryMiB: v.MemoryMiB, MaxCPUs: cfg.MaxCPUs, MaxMemoryMiB: cfg.MaxMemoryMiB,
	}
	if err := saveRuntimeState(dir, rt); err != nil {
		log.Printf("vm: failed to persist runtime state for %s: %v", spec.ID, err)
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.mu.Unlock()
	b.watchGuest(spec, proc)
	go b.watchExit(spec.ID, v.NetworkMode, proc)
	return nil
}

// minACPITimeout is how long ACPI still gets after a guest agent shutdown request ran out of time.
const minACPITimeout = 5 * time.Second

func (b *Backend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	dir := config.InstanceDir(spec.ID)

	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	if ok {
		b.stopping[spec.ID] = struct{}{}
	}
	b.mu.Unlock()
	if !ok {
		// Not tracked as running in this daemon process.
		removeRuntimeState(dir)
		return nil
	}
	defer func() {
		b.mu.Lock()
		delete(b.stopping, spec.ID)
		b.mu.Unlock()
	}()

	guest, _ := b.GuestInfo(spec)
	b.unwatchGuest(spec.ID)

	// A connected guest agent is tried before ACPI: it doesn't depend on the guest
	// handling the power button event. ACPI and then a hard stop remain as fallbacks.
	var stopErr error
	start := time.Now()
	graceful := !force && timeout > 0
	if !graceful || !guest.AgentConnected || !b.agentShutdown(ctx, spec.ID, proc, timeout) {
		stopTimeout := timeout - time.Since(start)
		if stopTimeout < minACPITimeout && timeout >= minACPITimeout {
			stopTimeout = minACPITimeout
		}
		if !graceful {
			stopTimeout = 0
		}
		stopErr = proc.Stop(ctx, stopTimeout)
	}
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
	dir := config.InstanceDir(spec.ID)

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
		removeRuntimeState(dir)
		return instance.StateStopped, nil
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.mu.Unlock()

	if err := proc.AttachQMP(ctx); err != nil {
		// The process is alive but uncontrollable over QMP. Drop it again, or
		// Status would report it as starting forever and nothing would watch it.
		b.mu.Lock()
		delete(b.running, spec.ID)
		b.mu.Unlock()
		_ = proc.Close()
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d is alive but QMP is unreachable at %s: %w", rt.Pid, rt.QMPSocket, err)
	}
	b.watchGuest(spec, proc)
	go b.watchExit(spec.ID, spec.VM.NetworkMode, proc)
	return instance.StateRunning, nil
}

func (b *Backend) Delete(ctx context.Context, spec *instance.Spec) error {
	// Best-effort stop first.
	_ = b.Stop(ctx, spec, true, 0)

	dir := config.InstanceDir(spec.ID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("vm: removing instance dir: %w", err)
	}
	return nil
}

func (b *Backend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
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
	path := filepath.Join(config.InstanceDir(spec.ID), "console.log")

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

// AddPort adds a host-to-guest SLIRP port forward to spec, applied live
// over QMP (no restart, unlike Mount) if running, else persisted for Start.
func (b *Backend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: AddPort called with a nil VMSpec")
	}
	if spec.VM.NetworkMode == "bridge" {
		return fmt.Errorf("vm: %s uses bridge networking; it has its own address, so no host-forwarded ports apply", spec.Name)
	}
	proto := instance.Protocol(port.Protocol)
	for _, p := range spec.VM.Ports {
		if p.HostPort == port.HostPort && instance.Protocol(p.Protocol) == proto {
			return fmt.Errorf("vm: %s already forwards host port %d/%s", spec.Name, port.HostPort, proto)
		}
	}

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
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

	idx := -1
	for i, p := range spec.VM.Ports {
		if p.HostPort == hostPort && instance.Protocol(p.Protocol) == proto {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("vm: %s has no port forward for host port %d/%s (currently exposed: %s)",
			spec.Name, hostPort, proto, instance.FormatPorts(spec.VM.Ports))
	}

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if running && proc.QMP != nil {
		if err := proc.QMP.RemoveHostForward(ctx, qemu.NetdevID, hostPort, proto); err != nil {
			return fmt.Errorf("vm: removing live port forward: %w", err)
		}
	}

	spec.VM.Ports = append(spec.VM.Ports[:idx], spec.VM.Ports[idx+1:]...)
	return nil
}

// reconfigureAndRestartIfRunning rebuilds spec's cloud-init seed and, if
// the instance is running, restarts QEMU to pick up the mount change.
func (b *Backend) reconfigureAndRestartIfRunning(ctx context.Context, spec *instance.Spec) error {
	spec.VM.Generation++
	spec.VM.MountFS = mountFSVirtiofs // the rebuilt seed rewrites the guest's fstab for virtiofs
	if err := b.buildSeed(spec); err != nil {
		return err
	}

	b.mu.Lock()
	_, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running {
		return nil
	}

	if err := b.Stop(ctx, spec, false, 30*time.Second); err != nil {
		return fmt.Errorf("vm: stopping %s to apply mount change: %w", spec.Name, err)
	}
	if err := b.Start(ctx, spec); err != nil {
		return fmt.Errorf("vm: restarting %s after mount change: %w", spec.Name, err)
	}
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

// mergeSSHKeys adds keys to existing's top-level ssh_authorized_keys
// list, returning a complete "#cloud-config\n..." document.
func mergeSSHKeys(existing string, keys []string) (string, error) {
	if len(keys) == 0 {
		if existing == "" {
			return "#cloud-config\n{}\n", nil
		}
		return existing, nil
	}

	body := strings.TrimPrefix(existing, "#cloud-config\n")
	doc := map[string]any{}
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return "", fmt.Errorf("parsing existing cloud-init user-data: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	existingKeys, _ := doc["ssh_authorized_keys"].([]any)
	for _, k := range keys {
		existingKeys = append(existingKeys, k)
	}
	doc["ssh_authorized_keys"] = existingKeys

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}

// shQuote single-quotes s for safe interpolation into a POSIX shell command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mergeExtraHosts adds one /etc/hosts entry per host via idempotent
// bootcmd lines, sorted by name for deterministic output.
func mergeExtraHosts(existing string, hosts map[string]string) (string, error) {
	if len(hosts) == 0 {
		return existing, nil
	}

	body := strings.TrimPrefix(existing, "#cloud-config\n")
	doc := map[string]any{}
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return "", fmt.Errorf("parsing existing cloud-init user-data: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	existingBootcmd, _ := doc["bootcmd"].([]any)
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		line := fmt.Sprintf("%s %s", hosts[name], name)
		quoted := shQuote(line)
		existingBootcmd = append(existingBootcmd, fmt.Sprintf(`grep -qxF %s /etc/hosts || echo %s >> /etc/hosts`, quoted, quoted))
	}
	doc["bootcmd"] = existingBootcmd

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
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

// mergeGuestAgent adds qemu-guest-agent to existing's "packages" and a runcmd that starts it.
func mergeGuestAgent(existing string) (string, error) {
	body := strings.TrimPrefix(existing, "#cloud-config\n")
	doc := map[string]any{}
	if strings.TrimSpace(body) != "" {
		if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
			return "", fmt.Errorf("parsing existing cloud-init user-data: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	packages, _ := doc["packages"].([]any)
	if !slices.Contains(packages, any(guestAgentPackage)) {
		packages = append(packages, guestAgentPackage)
	}
	doc["packages"] = packages
	runcmd, _ := doc["runcmd"].([]any)
	doc["runcmd"] = append(runcmd, guestAgentStartCmd)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}

// bridgeNetworkConfig returns a cloud-init network-config statically
// assigning v.StaticIP/v.Gateway to mac, or "" when not on a bridge.
func bridgeNetworkConfig(v *instance.VMSpec, mac string) string {
	if v.NetworkMode != "bridge" || v.StaticIP == "" {
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

// macFromInstanceID derives a deterministic MAC address from id, within
// QEMU's locally-administered OUI (52:54:00).
func macFromInstanceID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
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
