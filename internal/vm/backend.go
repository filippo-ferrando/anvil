// Package vm implements backend.Backend for VM instances: resolving a
// catalog/mirror image, creating a QCOW2 overlay, building a NoCloud
// cloud-init seed, and spawning/supervising qemu-system-x86_64.
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

// Source is the subset of *store.Store's methods Backend needs to resolve
// --cloud-init-name and runtime VM mirrors — a narrow interface (rather
// than depending on *store.Store's full method set) so it's obvious at a
// glance what Backend actually reads from the registry.
type Source interface {
	ListMirrors(kindFilter store.MirrorKind) ([]store.Mirror, error)
	GetCloudInit(name string) (store.CloudInitConfig, error)
}

// Backend implements backend.Backend for instance.KindVM.
type Backend struct {
	Catalog *image.Catalog
	Vault   *image.Vault
	Seed    cloudinit.Builder
	Source  Source

	mu      sync.Mutex
	running map[string]*qemu.Process // instance ID -> live process; repopulated by Reconcile after a daemon restart
}

var (
	_ instance.Backend    = (*Backend)(nil)
	_ instance.Reconciler = (*Backend)(nil)
	_ instance.Mounter    = (*Backend)(nil)
)

func NewBackend(catalog *image.Catalog, vault *image.Vault, source Source) *Backend {
	return &Backend{
		Catalog: catalog,
		Vault:   vault,
		Seed:    cloudinit.NewBuilder(),
		Source:  source,
		running: make(map[string]*qemu.Process),
	}
}

// effectiveCatalog merges the base catalog with every enabled VM mirror
// currently in the registry. Mirror manifests are read from the store
// (cached at `anvil mirror add` time), not re-fetched over the network on
// every launch — see internal/store/mirrors.go and the plan's "Image
// mirrors" section.
func (b *Backend) effectiveCatalog() (*image.Catalog, error) {
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

func (b *Backend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Create called with a nil VMSpec")
	}
	v := spec.VM

	catalog, err := b.effectiveCatalog()
	if err != nil {
		return err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return err
	}
	v.DefaultUser = entry.DefaultUser

	dir := config.InstanceDir(spec.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}

	diskPath := filepath.Join(dir, "disk.qcow2")
	if err := b.Vault.OverlayFor(entry, diskPath, v.DiskGiB, progress); err != nil {
		return fmt.Errorf("vm: preparing disk: %w", err)
	}
	v.DiskPath = diskPath

	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.buildSeed(spec)
}

// buildSeed (re)builds spec's cloud-init seed ISO from its current
// CloudInitUserData/CloudInitName, SSHPublicKeys, and Mounts, overwriting
// whatever's at v.SeedISOPath. Called once by Create, and again by
// Mount/Umount whenever the mount list changes after creation — since
// there's no way to hot-add a 9p share to a live QEMU instance (see
// qemu.Mount's doc comment), the only way a mount change ever reaches the
// guest is through a rebuilt seed plus a restart.
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

	// Generation is bumped by Mount/Umount before calling buildSeed again —
	// baking it into instance-id means cloud-init treats a post-creation
	// reconfiguration as a "new" instance and re-applies every module
	// (mounts, ssh keys, everything), sidestepping any uncertainty about
	// which modules' default frequency would or wouldn't rerun on a plain
	// reboot that reused the same instance-id.
	metaData := fmt.Sprintf("instance-id: %s-gen%d\nlocal-hostname: %s\n", spec.ID, v.Generation, spec.Name)

	seedPath := filepath.Join(dir, "seed.iso")
	seed := cloudinit.Seed{UserData: userData, MetaData: metaData, NetworkConfig: bridgeNetworkConfig(v)}
	if err := b.Seed.Build(seed, seedPath); err != nil {
		return fmt.Errorf("vm: building cloud-init seed: %w", err)
	}
	v.SeedISOPath = seedPath
	return nil
}

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
	b.mu.Unlock()

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
		// An intent member: attach a tap device to the intent's shared
		// bridge instead of SLIRP. There's no host-forwarded SSH port in
		// this mode — the guest has its own address on the bridge
		// (v.StaticIP), reachable directly; see internal/cli/commands/ssh.go's
		// resolveSSHTarget for how the CLI picks between the two modes.
		tapName := network.TapName(spec.ID)
		if err := network.CreateTap(tapName, v.BridgeInterface); err != nil {
			return fmt.Errorf("vm: attaching to bridge %s: %w", v.BridgeInterface, err)
		}
		cfg.BridgeTapDevice = tapName
		v.SSHPort = 0
	} else {
		// SLIRP with an SSH host-forward, the default for a standalone
		// instance (not part of any intent).
		sshPort, err := allocateFreePort()
		if err != nil {
			return fmt.Errorf("vm: allocating SSH forward port: %w", err)
		}
		cfg.SLIRPHostForwards = []qemu.HostForward{{HostPort: sshPort, GuestPort: 22, Protocol: "tcp"}}
		v.SSHPort = sshPort
	}

	for _, m := range v.Mounts {
		cfg.Mounts = append(cfg.Mounts, qemu.Mount{HostPath: m.HostPath, Tag: m.Tag, ReadOnly: m.ReadOnly})
	}

	proc, err := qemu.Spawn(ctx, cfg, filepath.Join(dir, "qemu.log"))
	if err != nil {
		if cfg.BridgeTapDevice != "" {
			_ = network.DeleteTap(cfg.BridgeTapDevice) // don't leak the tap device we just created
		}
		return fmt.Errorf("vm: spawning qemu: %w", err)
	}
	if err := proc.AttachQMP(ctx); err != nil {
		_ = proc.Stop(ctx, 0)
		if cfg.BridgeTapDevice != "" {
			_ = network.DeleteTap(cfg.BridgeTapDevice)
		}
		return fmt.Errorf("vm: attaching QMP: %w", err)
	}
	if err := saveRuntimeState(dir, runtimeState{Pid: proc.Pid(), QMPSocket: cfg.QMPSocket, StartedAt: time.Now()}); err != nil {
		// Not fatal to Start itself (the instance is genuinely running),
		// but Reconcile won't be able to find it after a daemon restart —
		// worth a log line, not worth failing the launch over.
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
		// Not tracked as running in this daemon process. Could be already
		// stopped, or a process from a previous daemon run that Reconcile
		// hasn't (yet) been asked to check — Stop doesn't reconcile on its
		// own, that only happens at daemon startup (see Manager.Reconcile).
		removeRuntimeState(dir)
		return nil
	}

	stopTimeout := timeout
	if force {
		stopTimeout = 0
	}
	if err := proc.Stop(ctx, stopTimeout); err != nil {
		return fmt.Errorf("vm: stopping instance %s: %w", spec.ID, err)
	}
	_ = proc.Close()
	if spec.VM != nil && spec.VM.NetworkMode == "bridge" {
		// Best-effort: a tap device left behind is a leak worth avoiding,
		// but not worth failing Stop over.
		if err := network.DeleteTap(network.TapName(spec.ID)); err != nil {
			log.Printf("vm: failed to delete tap device for %s: %v", spec.ID, err)
		}
	}
	removeRuntimeState(dir)

	b.mu.Lock()
	delete(b.running, spec.ID)
	b.mu.Unlock()
	return nil
}

// Reconcile implements instance.Reconciler: after a daemon restart, this
// backend's b.running map starts out empty even for VMs the registry
// still thinks are running, since that map only lives in this process's
// memory. Reconcile re-derives the truth from the OS instead of trusting
// the registry blindly — see runtime.go's runtimeState (pid + QMP socket,
// persisted outside bbolt) and qemu.Attach (pid liveness + cmdline match).
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
		// Nothing was ever persisted (or Stop/Delete already cleaned it
		// up) — nothing to reconcile, the registry's "running" was stale.
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
		// The process is genuinely alive (Attach already verified that),
		// we just can't control it over QMP. Still track it — Stop's
		// SIGKILL escalation doesn't need QMP — but report this as an
		// error state rather than silently claiming it's healthy.
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d is alive but QMP is unreachable at %s: %w", rt.Pid, rt.QMPSocket, err)
	}
	return instance.StateRunning, nil
}

func (b *Backend) Delete(ctx context.Context, spec *instance.Spec) error {
	// Best-effort stop first; Delete shouldn't fail just because the
	// instance was already stopped or untracked.
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
		// Not tracked in this process's memory. As long as Manager.Reconcile
		// ran at daemon startup, this is a legitimate "actually stopped" —
		// it only stayed wrong if reconciliation itself hasn't run yet
		// (e.g. this method gets called mid-reconciliation somehow).
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

// Logs streams spec's console.log: QEMU's own capture of the guest's
// serial console (see qemu.Config.SerialLogPath in Start), which is where
// boot messages and cloud-init's own output land — not application logs
// from inside the guest OS, there's no way to see those without actually
// SSHing in (see `anvil shell`/`exec`). If the instance was never started,
// or console.log hasn't been created yet, this returns cleanly with no
// output rather than an error.
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

	// f's read offset is now at EOF (from the io.ReadAll above); since
	// console.log is append-only (QEMU keeps the same fd open for the
	// whole VM lifetime), just keep reading from here as more gets written,
	// no need to reopen or re-seek.
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
// new qemu.Mount and rebuilding the cloud-init seed. See
// reconfigureAndRestartIfRunning and qemu.Mount's doc comment for why this
// restarts the guest OS instead of hot-plugging: there isn't a hot-plug
// path for a 9p share, verified empirically against a real QEMU build.
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

// reconfigureAndRestartIfRunning rebuilds spec's cloud-init seed (the only
// way a mount change actually reaches the guest — see buildSeed) and, if
// the instance is currently running, restarts QEMU so its command line
// picks up the new/removed 9p fsdev+device pair.
//
// This is a real reboot of the guest OS, not a transparent hot-plug:
// checked empirically (spawn a real qemu-system-x86_64, ask QMP's
// qom-list-types for anything implementing "fsdev-backend" or that's
// otherwise user-creatable and fs-shaped — nothing exists) rather than
// assumed, there is genuinely no QMP path to attach a new 9p share to an
// already-running QEMU process. Anything running inside the guest is
// interrupted; the persisted disk itself is untouched.
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

// mergeSSHKeys adds keys to existing's top-level ssh_authorized_keys list,
// returning a complete "#cloud-config\n..." document. existing may be empty
// (no cloud-init customization at all) or an arbitrary cloud-config
// document (from --cloud-init or the saved library).
//
// This parses and re-serializes existing as YAML rather than
// string-concatenating "ssh_authorized_keys:\n  - key\n" onto the end of
// it — a real bug, not a hypothetical: the empty-cloud-config default used
// to be the literal string "#cloud-config\n{}\n", and appending more
// block-style YAML after a flow-style "{}" isn't valid YAML at all, so
// cloud-init silently parsed only the "{}" and dropped every injected key.
// The same class of bug would hit a real user-supplied --cloud-init file
// too, e.g. one that doesn't end in a trailing newline.
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

// mergeMounts adds mounts' fstab-shaped entries to existing's top-level
// "mounts" list (cloud-init's `mounts` config module — see
// https://cloudinit.readthedocs.io — reads these as
// [device, mountpoint, fstype, options, ...] tuples), plus a `bootcmd` per
// mount to create its mountpoint directory first: cloud-init's mounts
// module doesn't create missing mountpoints itself, and bootcmd runs
// early enough (cloud-init's Local/Network stage) to happen before mounts
// runs (Config stage). Same parse/merge/re-serialize approach as
// mergeSSHKeys, for the same reason: string-concatenating onto arbitrary
// existing YAML isn't safe.
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
		// nofail matters here specifically: it's not confirmed whether
		// cloud-init's mounts module prunes an fstab entry whose tag no
		// longer appears in a later boot's user-data (an `anvil umount`,
		// say) — if it leaves a stale entry behind, nofail is what stops
		// that from hanging boot waiting on a 9p device that's genuinely
		// gone from the qemu command line.
		opts := fmt.Sprintf("trans=virtio,version=9p2000.L,%s,nofail", rw)
		existingMounts = append(existingMounts, []any{m.Tag, m.GuestPath, "9p", opts, "0", "0"})
		newBootcmd = append(newBootcmd, fmt.Sprintf("mkdir -p %s", m.GuestPath))
	}
	doc["mounts"] = existingMounts
	doc["bootcmd"] = append(newBootcmd, existingBootcmd...)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("re-encoding cloud-init user-data: %w", err)
	}
	return "#cloud-config\n" + string(out), nil
}

// bridgeNetworkConfig returns a cloud-init NoCloud network-config (v2,
// netplan-shaped) that statically assigns v.StaticIP/v.Gateway, or "" when
// v isn't joining an intent's bridged network (v.NetworkMode != "bridge",
// the default) — SLIRP's own built-in DHCP needs no guest-side
// configuration at all, so there's nothing to generate in that case.
//
// Matches on "en*" rather than a fixed device name like "eth0": a virtio
// NIC under QEMU's q35 machine type typically gets a systemd predictable
// name like "enp0s3" on a modern cloud image, not "eth0", and there's
// only ever one NIC to match here anyway. Not verified against a real
// guest boot yet — see PLAN.md's M4 notes.
func bridgeNetworkConfig(v *instance.VMSpec) string {
	if v.NetworkMode != "bridge" || v.StaticIP == "" {
		return ""
	}
	return fmt.Sprintf(`network:
  version: 2
  ethernets:
    anvil0:
      match:
        name: "en*"
      dhcp4: false
      addresses: [%s]
      gateway4: %s
`, v.StaticIP, v.Gateway)
}

func kvmAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

// allocateFreePort asks the OS for an ephemeral port by briefly binding to
// port 0, reading back what was assigned, then releasing it. This has an
// inherent (if narrow) race — another process could claim the port before
// qemu binds it — accepted for v1; see the plan's networking section for
// the bridged-networking path intents will use instead once implemented.
func allocateFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
