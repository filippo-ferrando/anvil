//go:build linux

// Package vm implements backend.Backend for VM instances: resolving an
// image, building a disk overlay and cloud-init seed, and running QEMU.
package vm

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/cloudinit"
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
		// SSH is only for local clients (and --remote, which runs on this host), so it binds localhost.
		cfg.SLIRPHostForwards = []qemu.HostForward{{HostAddr: "127.0.0.1", HostPort: sshPort, GuestPort: 22, Protocol: "tcp"}}
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
	if stopErr != nil && !exitedNow(proc.Exited()) {
		// Its tap and runtime state stay while it may still run; watchExit releases them once it's gone.
		return fmt.Errorf("vm: stopping instance %s: %w", spec.ID, stopErr)
	}
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
	// A failed stop is fine only once QEMU is gone: its dir can't go while it still runs.
	if err := b.Stop(ctx, spec, true, 0); err != nil && b.tracked(spec.ID) {
		return err
	}
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

// exitedNow reports whether exited is already closed.
func exitedNow(exited <-chan struct{}) bool {
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

func kvmAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}
