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
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/hostpath"
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
}

var (
	_ instance.Backend    = (*Backend)(nil)
	_ instance.Reconciler = (*Backend)(nil)
	_ instance.Mounter    = (*Backend)(nil)
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
		return b.adoptMigratedDisk(spec, dir, progress)
	}

	catalog, err := b.EffectiveCatalog()
	if err != nil {
		return err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return err
	}
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
// disk.qcow2, skipping catalog lookup, overlay creation, and reseeding.
func (b *Backend) adoptMigratedDisk(spec *instance.Spec, dir string, progress func(status string)) error {
	v := spec.VM
	if progress != nil {
		progress("adopting migrated disk")
	}

	diskPath := filepath.Join(dir, "disk.qcow2")
	if err := os.Rename(v.SourceDiskPath, diskPath); err != nil {
		// Rename can fail across filesystems; fall back to a copy.
		if err := copyFile(v.SourceDiskPath, diskPath); err != nil {
			return fmt.Errorf("vm: adopting migrated disk: %w", err)
		}
		_ = os.Remove(v.SourceDiskPath)
	}
	v.DiskPath = diskPath
	v.SourceDiskPath = ""
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

// PrepareImportedDisk ensures imageRef/arch's base image is present in this
// host's vault (downloading it if needed) and rebases diskPath's backing
// file onto it in place. diskPath is an imported qcow2 diff disk whose
// backing file still points at wherever it was exported from — this is
// what makes it adoptable on a different host. Satisfies
// internal/export.VMImporter.
func (b *Backend) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath string) error {
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
	cmd := exec.CommandContext(ctx, "qemu-img", "rebase", "-u", "-F", "qcow2", "-b", basePath, diskPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("vm: rebasing imported disk: %w: %s", err, out)
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
	userData, err = mergeMounts(userData, v.Mounts)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
	}
	userData, err = mergeExtraHosts(userData, v.ExtraHosts)
	if err != nil {
		return fmt.Errorf("vm: preparing cloud-init user-data: %w", err)
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

	dir := config.InstanceDir(spec.ID)
	cfg := qemu.Config{
		Arch:          v.Arch,
		CPUs:          v.CPUs,
		MemoryMiB:     v.MemoryMiB,
		DiskPath:      v.DiskPath,
		SeedISOPath:   v.SeedISOPath,
		QMPSocket:     filepath.Join(dir, "qmp.sock"),
		SerialLogPath: filepath.Join(dir, "console.log"),
		KVM:           kvmAvailable(),
	}
	if v.NetworkMode == "bridge" {
		// Intent member: attach to the intent's shared network via the
		// backend's Networker (Linux tap+bridge by default — see
		// Networker's doc for why this is swappable).
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
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			cfg.SLIRPHostForwards = append(cfg.SLIRPHostForwards, qemu.HostForward{
				HostPort: p.HostPort, GuestPort: p.GuestPort, Protocol: proto,
			})
		}
		v.SSHPort = sshPort
	}

	for _, m := range v.Mounts {
		cfg.Mounts = append(cfg.Mounts, qemu.Mount{HostPath: m.HostPath, Tag: m.Tag, ReadOnly: m.ReadOnly})
	}

	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		if cfg.BridgeTapDevice != "" {
			_ = b.Networker.Detach(spec.ID)
		}
		return fmt.Errorf("vm: spawning qemu: %w", err)
	}
	if err := proc.AttachQMP(ctx); err != nil {
		_ = proc.Stop(ctx, 0)
		if cfg.BridgeTapDevice != "" {
			_ = b.Networker.Detach(spec.ID)
		}
		return fmt.Errorf("vm: attaching QMP: %w", err)
	}
	if err := saveRuntimeState(dir, runtimeState{Pid: proc.Pid(), QMPSocket: cfg.QMPSocket, StartedAt: time.Now()}); err != nil {
		log.Printf("vm: failed to persist runtime state for %s: %v", spec.ID, err)
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.mu.Unlock()
	return nil
}

func (b *Backend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	dir := config.InstanceDir(spec.ID)

	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
	if !ok {
		// Not tracked as running in this daemon process.
		removeRuntimeState(dir)
		return nil
	}

	stopTimeout := timeout
	if force {
		stopTimeout = 0
	}
	stopErr := proc.Stop(ctx, stopTimeout)
	// Clean up unconditionally: the process is gone by now either way.
	_ = proc.Close()
	if spec.VM != nil && spec.VM.NetworkMode == "bridge" {
		// Best-effort cleanup; not worth failing Stop over.
		if err := b.Networker.Detach(spec.ID); err != nil {
			log.Printf("vm: failed to detach network device for %s: %v", spec.ID, err)
		}
	}
	removeRuntimeState(dir)

	b.mu.Lock()
	delete(b.running, spec.ID)
	b.mu.Unlock()

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
		// The process is alive but uncontrollable over QMP; report it as an error state.
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d is alive but QMP is unreachable at %s: %w", rt.Pid, rt.QMPSocket, err)
	}
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

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("vm: reading console log: %w", err)
	}
	if tailLines > 0 {
		data = tailLinesOf(data, tailLines)
	}
	if len(data) > 0 {
		if err := send(data); err != nil {
			return err
		}
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

// Mount shares hostPath into spec's guest at guestPath over 9p, adding a
// new mount and rebuilding the cloud-init seed.
func (b *Backend) Mount(ctx context.Context, spec *instance.Spec, hostPath, guestPath string, readOnly bool) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Mount called with a nil VMSpec")
	}
	v := spec.VM

	for _, m := range v.Mounts {
		if m.GuestPath == guestPath {
			return fmt.Errorf("vm: %s already has a mount at %s", spec.Name, guestPath)
		}
	}
	if info, err := os.Stat(hostPath); err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("vm: host path %s: %w%s", hostPath, err, hostpath.Hint(hostPath))
		}
		return fmt.Errorf("vm: host path %s: %w", hostPath, err)
	} else if !info.IsDir() {
		return fmt.Errorf("vm: host path %s is not a directory", hostPath)
	}

	tag := fmt.Sprintf("mount%d", v.NextMountIndex)
	v.NextMountIndex++
	v.Mounts = append(v.Mounts, instance.Mount{
		HostPath:  hostPath,
		GuestPath: guestPath,
		Tag:       tag,
		ReadOnly:  readOnly,
	})

	return b.reconfigureAndRestartIfRunning(ctx, spec)
}

// Umount removes a mount previously added with Mount, identified by its
// guest path.
func (b *Backend) Umount(ctx context.Context, spec *instance.Spec, guestPath string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Umount called with a nil VMSpec")
	}
	v := spec.VM

	idx := -1
	for i, m := range v.Mounts {
		if m.GuestPath == guestPath {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("vm: %s has no mount at %s", spec.Name, guestPath)
	}
	v.Mounts = append(v.Mounts[:idx], v.Mounts[idx+1:]...)

	return b.reconfigureAndRestartIfRunning(ctx, spec)
}

// reconfigureAndRestartIfRunning rebuilds spec's cloud-init seed and, if
// the instance is running, restarts QEMU to pick up the mount change.
func (b *Backend) reconfigureAndRestartIfRunning(ctx context.Context, spec *instance.Spec) error {
	spec.VM.Generation++
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
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) <= n {
		return data
	}
	return bytes.Join(lines[len(lines)-n:], []byte("\n"))
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

// mergeMounts adds mounts' fstab-shaped entries to existing's top-level
// "mounts" list, plus a bootcmd per mount to create its mountpoint first.
func mergeMounts(existing string, mounts []instance.Mount) (string, error) {
	if len(mounts) == 0 {
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

	existingMounts, _ := doc["mounts"].([]any)
	existingBootcmd, _ := doc["bootcmd"].([]any)
	var newBootcmd []any
	for _, m := range mounts {
		rw := "rw"
		if m.ReadOnly {
			rw = "ro"
		}
		// nofail avoids hanging boot on a removed 9p device.
		opts := fmt.Sprintf("trans=virtio,version=9p2000.L,%s,nofail", rw)
		existingMounts = append(existingMounts, []any{m.Tag, m.GuestPath, "9p", opts, "0", "0"})
		newBootcmd = append(newBootcmd, fmt.Sprintf("mkdir -p %s", shQuote(m.GuestPath)))
	}
	doc["mounts"] = existingMounts
	doc["bootcmd"] = append(newBootcmd, existingBootcmd...)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
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

// bridgeNetworkConfig returns a cloud-init network-config statically
// assigning v.StaticIP/v.Gateway to mac, or "" when not on a bridge.
func bridgeNetworkConfig(v *instance.VMSpec, mac string) string {
	if v.NetworkMode != "bridge" || v.StaticIP == "" {
		return ""
	}
	return fmt.Sprintf(`network:
  version: 2
  ethernets:
    anvil0:
      match:
        macaddress: "%s"
      dhcp4: false
      addresses: [%s]
      gateway4: %s
`, mac, v.StaticIP, v.Gateway)
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

// allocateFreePort asks the OS for an ephemeral port by briefly binding
// to port 0, reading back what was assigned, then releasing it.
func allocateFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
