//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/ipam"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/network"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

// FCBackend runs VMs whose spec asks for --hypervisor firecracker. The catalog, image
// vault, cloud-init seed and console log handling are shared with the QEMU Backend.
type FCBackend struct {
	QEMU      *Backend
	Cache     *image.FCCache
	Networker Networker

	// Jailer, when set, is the jailer binary that runs firecracker chrooted in ChrootBase.
	Jailer     string
	ChrootBase string

	guestTracker

	vmTable[*firecracker.Process]

	// Standalone VMs only: their address on FCBridge, and the proxies standing in for
	// SLIRP's host forwards, keyed by proxyKey.
	ipam    fcIPAM
	proxies map[string]map[string]*portProxy // guarded by mu

	// nfs serves `anvil mount` shares; exported is which guest address each VM's are served to.
	nfs      *nfsHub
	exported map[string]netip.Addr // guarded by mu

	paths map[string]fcInstancePaths // running VMs' files, jailed or not; guarded by mu

	// Clones are the networks of ephemeral clones, each in its own netns.
	Clones *CloneNetworks
	clones map[string]*fcClone // running clones; guarded by mu

	// unjailedClones lets tests run clones without the jailer (which needs real root).
	unjailedClones bool
}

var (
	_ instance.Backend        = (*FCBackend)(nil)
	_ instance.Reconciler     = (*FCBackend)(nil)
	_ instance.StatsProvider  = (*FCBackend)(nil)
	_ instance.GuestInspector = (*FCBackend)(nil)
	_ instance.ReadyWaiter    = (*FCBackend)(nil)
	_ instance.ExitNotifier   = (*FCBackend)(nil)
	_ instance.PortForwarder  = (*FCBackend)(nil)
)

// NewFCBackend shares qemuBackend's catalog and vault; instances is where standalone
// addresses already in use are read from.
func NewFCBackend(qemuBackend *Backend, cacheDir string, instances Instances) *FCBackend {
	b := &FCBackend{
		QEMU:       qemuBackend,
		Cache:      &image.FCCache{Dir: cacheDir, Vault: qemuBackend.Vault},
		Networker:  network.LinuxBridge{},
		Jailer:     jailerExe(),
		ChrootBase: config.JailerDir(),
		vmTable:    newVMTable[*firecracker.Process](),
		ipam:       fcIPAM{instances: instances},
		proxies:    make(map[string]map[string]*portProxy),
		nfs:        newNFSHub(),
		exported:   make(map[string]netip.Addr),
		paths:      make(map[string]fcInstancePaths),
		Clones:     &CloneNetworks{Helper: hostdev.Client{Socket: config.DevHelperSocket()}},
	}
	b.guestTracker = newGuestTracker(func(id string) *qemu.GuestAgent {
		return &qemu.GuestAgent{Dial: func(ctx context.Context) (net.Conn, error) {
			return firecracker.DialVsock(ctx, b.pathsOf(id).vsock, fcAgentPort)
		}}
	}, "reboot") // a guest poweroff doesn't end firecracker on x86_64, a reboot does
	return b
}

// standalone reports whether v sits on FCBridge rather than an intent's bridge.
func standalone(v *instance.VMSpec) bool { return v.NetworkMode != "bridge" }

// fcLookPath finds the firecracker binary; tests swap it to run Create without one.
var fcLookPath = firecracker.LookPath

type fcInstancePaths struct {
	firecracker.Paths
	vsock string
}

// fcBootArgs is the guest kernel command line; Firecracker adds root= from the drive's partuuid.
// reboot=k makes a guest reboot end the firecracker process, panic=1 does the same on a panic,
// and memhp_default_state onlines virtio-mem memory as soon as it's plugged.
func fcBootArgs(extra string) string {
	return strings.TrimSpace("console=ttyS0 reboot=k panic=1 memhp_default_state=online_movable " + extra)
}

// resolve finds spec's catalog image, its Firecracker entry and the kernel for its arch.
func (b *FCBackend) resolve(v *instance.VMSpec) (image.DistroEntry, image.FCDistro, image.FCKernel, error) {
	catalog, err := b.QEMU.EffectiveCatalog()
	if err != nil {
		return image.DistroEntry{}, image.FCDistro{}, image.FCKernel{}, err
	}
	entry, err := catalog.Find(v.ImageRef, v.Arch)
	if err != nil {
		return image.DistroEntry{}, image.FCDistro{}, image.FCKernel{}, err
	}
	distro, err := catalog.FirecrackerDistro(entry.ID)
	if err != nil {
		return entry, image.FCDistro{}, image.FCKernel{}, err
	}
	kernel, err := catalog.FirecrackerKernel(entry.Arch)
	return entry, distro, kernel, err
}

func (b *FCBackend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) (err error) {
	if spec.VM == nil {
		return fmt.Errorf("vm: Create called with a nil VMSpec")
	}
	v := spec.VM
	if _, err := fcLookPath(firecracker.Binary); err != nil {
		return fmt.Errorf("vm: firecracker isn't installed on this host: %w", err)
	}
	if v.MACAddress == "" {
		v.MACAddress = instance.DefaultMAC(spec.ID)
	} else if _, err := net.ParseMAC(v.MACAddress); err != nil {
		return fmt.Errorf("vm: invalid MAC address %q: %w", v.MACAddress, err)
	}

	entry, _, kernel, err := b.resolve(v)
	if err != nil {
		return err
	}
	v.Arch = entry.Arch
	adopting := v.SourceDiskPath != ""
	if !adopting || v.DefaultUser == "" {
		v.DefaultUser = entry.DefaultUser
	}

	reseed := !adopting
	if standalone(v) {
		// A migrated or imported guest keeps the address configured inside its disk. When it
		// can't (a copy next to its original, another subnet, none recorded), it gets a new
		// one through a new seed, the way a fork does.
		keep := adopting && v.StaticIP != ""
		if keep {
			if kerr := b.keepAddress(spec); kerr != nil {
				if progress != nil {
					progress(fmt.Sprintf("can't keep address %s (%v), assigning a new one", ipam.StripCIDR(v.StaticIP), kerr))
				}
				keep = false
			}
		}
		if !keep {
			reseed = true
			if err = b.assignAddress(spec); err != nil {
				return err
			}
		}
		defer func() {
			if err != nil {
				b.ipam.release(spec.ID)
			}
		}()
	}

	dir := instanceDir(spec.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	// The kernel downloads on the first Firecracker launch; later launches find it cached.
	if _, err := b.Cache.Kernel(ctx, kernel, progress); err != nil {
		return fmt.Errorf("vm: preparing the firecracker kernel: %w", err)
	}
	if adopting {
		if err := b.adoptDisk(ctx, spec, dir, progress); err != nil || !reseed {
			return err
		}
		return b.QEMU.buildSeed(spec)
	}
	raw, err := b.Cache.RawBase(ctx, entry, progress)
	if err != nil {
		return fmt.Errorf("vm: preparing disk: %w", err)
	}
	if progress != nil {
		progress("creating disk")
	}
	diskPath := filepath.Join(dir, "disk.raw")
	// Without copy-on-write the disk is a full-size copy, too big to leave behind.
	defer func() {
		if err != nil {
			_ = os.Remove(diskPath)
		}
	}()
	if err := image.CloneDisk(ctx, raw, diskPath, v.DiskGiB); err != nil {
		return fmt.Errorf("vm: preparing disk: %w", err)
	}
	v.DiskPath = diskPath

	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.QEMU.buildSeed(spec)
}

// Start is idempotent for an already-running instance, but rejects a
// concurrent Start call for the same spec.ID rather than double-spawning.
func (b *FCBackend) Start(ctx context.Context, spec *instance.Spec) error {
	return b.start(ctx, spec, nil)
}

// start boots spec; with netns set it runs jailed in that clone netns, on its tap, with
// no bridge, guest filter or proxies.
func (b *FCBackend) start(ctx context.Context, spec *instance.Spec, netns *CloneNet) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Start called with a nil VMSpec")
	}
	v := spec.VM

	running, done, err := b.beginStart(spec.ID)
	if running || err != nil {
		return err
	}
	defer done()

	_, distro, kernel, err := b.resolve(v)
	if err != nil {
		return err
	}
	kernelPath, err := b.Cache.Kernel(ctx, kernel, nil)
	if err != nil {
		return fmt.Errorf("vm: preparing the firecracker kernel: %w", err)
	}
	partUUID, err := image.RootPartUUID(v.DiskPath, v.Arch)
	if err != nil {
		return fmt.Errorf("vm: finding the root partition: %w", err)
	}

	paths := fcPathsFor(spec.ID, b.newJail(spec.ID))
	if netns != nil {
		if paths, err = b.clonePaths(spec.ID, netns.NetNS); err != nil {
			return err
		}
	}
	if err := prepareJail(spec.ID, paths); err != nil {
		return err
	}
	jailOK := false
	defer func() {
		if !jailOK {
			removeJail(paths)
		}
	}()
	// Inside a jail firecracker only sees files linked into it, by their in-jail paths.
	vmKernel, err := jailFile(paths, kernelPath, "vmlinux", true)
	if err != nil {
		return err
	}
	vmDisk, err := jailFile(paths, v.DiskPath, "disk.raw", false)
	if err != nil {
		return err
	}
	cfg := firecracker.Config{
		BootSource:    firecracker.BootSource{KernelImagePath: vmKernel, BootArgs: fcBootArgs(distro.KernelArgs)},
		MachineConfig: firecracker.MachineConfig{VCPUCount: max(v.CPUs, 1), MemSizeMiB: v.MemoryMiB},
		Drives: []firecracker.Drive{
			{DriveID: fcRootDrive, PathOnHost: vmDisk, IsRootDevice: true, PartUUID: partUUID, Discard: true},
		},
		Vsock:   &firecracker.Vsock{GuestCID: firecracker.GuestCID, UDSPath: paths.InVMM(paths.vsock)},
		Entropy: &struct{}{},
	}
	hotplugMiB := fcHotplugMiB(v.Arch, v.CPUs, v.MemoryMiB)
	if v.AnvilGuest {
		hotplugMiB = 0 // a snapshot's memory file would cover the whole region
	}
	if hotplugMiB > 0 {
		cfg.MemoryHotplug = &firecracker.MemoryHotplug{TotalSizeMiB: hotplugMiB}
	}
	if v.SeedISOPath != "" {
		seed, err := jailFile(paths, v.SeedISOPath, "seed.iso", true)
		if err != nil {
			return err
		}
		cfg.Drives = append(cfg.Drives, firecracker.Drive{DriveID: "seed", PathOnHost: seed, IsReadOnly: true})
	}
	// A missing tools dir or a failed pack only costs GUI support, so the VM still boots.
	if spec.GUI && toolsAvailable() {
		iso, err := b.toolsISO()
		if err == nil {
			iso, err = jailFile(paths, iso, "gui-tools.iso", true)
		}
		if err != nil {
			log.Printf("vm: %s: %v", spec.Name, err)
		} else {
			cfg.Drives = append(cfg.Drives, firecracker.Drive{DriveID: "guitools", PathOnHost: iso, IsReadOnly: true})
		}
	}

	// Unlike the GUI tools, a VM asking for anvil-guest is no use without it.
	if v.AnvilGuest {
		iso, err := b.guestISO()
		if err == nil {
			iso, err = jailFile(paths, iso, "guest.iso", true)
		}
		if err != nil {
			return err
		}
		cfg.Drives = append(cfg.Drives, firecracker.Drive{DriveID: "guest", PathOnHost: iso, IsReadOnly: true})
	}

	unwind := func() {
		b.closeProxies(spec.ID)
		b.unexportMounts(spec.ID)
		dropGuestFilter(spec.ID)
		_ = b.Networker.Detach(spec.ID)
	}
	if netns != nil {
		cfg.NetworkInterfaces = []firecracker.NetworkInterface{{IfaceID: "eth0", HostDevName: netns.Tap, GuestMAC: v.MACAddress}}
		v.SSHPort = 0
	} else if err := b.attachNetwork(spec, &cfg, unwind); err != nil {
		return err
	}

	// A vsock socket left by an earlier run makes firecracker refuse the config.
	_ = os.Remove(paths.vsock)
	if err := cfg.WriteFile(paths.ConfigFile); err != nil {
		unwind()
		return fmt.Errorf("vm: %w", err)
	}
	proc, err := firecracker.Spawn(ctx, paths.Paths)
	if err != nil {
		unwind()
		return fmt.Errorf("vm: spawning firecracker: %w", err)
	}
	if err := proc.WaitAPI(ctx); err != nil {
		_ = proc.Stop(ctx, 0)
		_ = proc.Close()
		unwind()
		return fmt.Errorf("vm: %w", err)
	}
	jailOK = true
	rt := runtimeState{Pid: proc.Pid(), StartedAt: time.Now(), BootMemoryMiB: v.MemoryMiB, MaxMemoryMiB: v.MemoryMiB + hotplugMiB,
		Jailed: paths.Jail != nil}
	if paths.Jail != nil {
		rt.JailExec = paths.Jail.ExecFile
	}
	if err := saveRuntimeState(instanceDir(spec.ID), rt); err != nil {
		log.Printf("vm: failed to persist runtime state for %s: %v", spec.ID, err)
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.paths[spec.ID] = paths
	b.mu.Unlock()
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, proc)
	return nil
}

// attachNetwork puts spec's VM on its bridge (anvil-fc0 for a standalone one), with the
// guest filter, NFS exports and SSH proxy a standalone VM gets. unwind undoes it all.
func (b *FCBackend) attachNetwork(spec *instance.Spec, cfg *firecracker.Config, unwind func()) error {
	v := spec.VM
	bridge := v.BridgeInterface
	if standalone(v) {
		if v.StaticIP == "" {
			return fmt.Errorf("vm: %s was created without a network address, recreate it", spec.Name)
		}
		if err := ensureFCNetwork(); err != nil {
			return err
		}
		bridge = FCBridge
	}
	// A tap left by a firecracker that died while anvild was down would make Attach fail.
	_ = b.Networker.Detach(spec.ID)
	tap, err := b.Networker.Attach(spec.ID, bridge)
	if err != nil {
		return fmt.Errorf("vm: attaching to bridge %s: %w", bridge, err)
	}
	cfg.NetworkInterfaces = []firecracker.NetworkInterface{{IfaceID: "eth0", HostDevName: tap, GuestMAC: v.MACAddress}}
	if standalone(v) {
		if err := runNft(fcGuestFilter(spec.ID, tap, v.MACAddress, ipam.StripCIDR(v.StaticIP))); err != nil {
			unwind()
			return err
		}
	}
	if err := b.exportMounts(spec); err != nil {
		unwind()
		return err
	}
	if standalone(v) {
		if err := b.startProxies(spec, 0); err != nil {
			unwind()
			return err
		}
	} else {
		v.SSHPort = 0
	}
	return nil
}

func (b *FCBackend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	proc, ok, starting := b.beginStop(spec.ID)
	if !ok && starting {
		return fmt.Errorf("vm: %s is being started, try again once it's up", spec.Name)
	}
	if !ok {
		removeRuntimeState(instanceDir(spec.ID))
		return nil
	}
	defer b.endStop(spec.ID)

	// The guest agent first, then Ctrl+Alt+Del, then a kill, all within timeout.
	stopErr := b.gracefulStop(ctx, spec, proc, force, timeout)
	if stopErr != nil && !hasExited(proc) {
		// Its tap and jail stay while it may still run; watchExit releases them once it's gone.
		return fmt.Errorf("vm: stopping instance %s: %w", spec.ID, stopErr)
	}
	b.releaseStopped(spec.ID, proc)

	if stopErr != nil {
		return fmt.Errorf("vm: stopping instance %s: %w", spec.ID, stopErr)
	}
	return nil
}

// watchExit reports an exit not caused by Stop (guest reboot or poweroff, crash, kill).
func (b *FCBackend) watchExit(instanceID string, proc *firecracker.Process) {
	<-proc.Exited()

	hook, own := b.ownExit(instanceID, proc)
	if !own {
		return
	}

	state, reason := instance.StateStopped, "exited on its own (guest powered off or rebooted)"
	if proc.ExitStatus() == "unknown" {
		reason = "exited on its own; started by an earlier anvild, so its exit status is unknown"
	} else if !proc.ExitedCleanly() {
		state, reason = instance.StateError, "firecracker exited on its own ("+proc.ExitStatus()+")"
	}
	log.Printf("vm: %s exited on its own, now %s", instanceID, state)
	b.releaseStopped(instanceID, proc)
	if hook != nil {
		hook(instanceID, state, reason)
	}
}

// releaseStopped frees everything a VM held while it ran, once firecracker is gone.
func (b *FCBackend) releaseStopped(instanceID string, proc *firecracker.Process) {
	b.unwatchGuest(instanceID)
	_ = proc.Close()
	b.closeProxies(instanceID)
	b.unexportMounts(instanceID)
	dropGuestFilter(instanceID)
	if err := b.Networker.Detach(instanceID); err != nil {
		log.Printf("vm: failed to detach network device for %s: %v", instanceID, err)
	}
	removeRuntimeState(instanceDir(instanceID))

	b.mu.Lock()
	paths, ok := b.paths[instanceID]
	if b.running[instanceID] == proc {
		delete(b.running, instanceID)
		delete(b.paths, instanceID)
	}
	b.mu.Unlock()
	if ok {
		removeJail(paths)
	}
}

// Reconcile re-attaches to a firecracker process an earlier anvild started.
func (b *FCBackend) Reconcile(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	if spec.VM == nil {
		return instance.StateStopped, nil
	}
	dir := instanceDir(spec.ID)
	rt, found, err := loadRuntimeState(dir)
	if err != nil {
		return instance.StateStopped, fmt.Errorf("vm: reading runtime state: %w", err)
	}
	if !found {
		return instance.StateStopped, nil
	}
	var jail *firecracker.Jail
	if rt.Jailed {
		jail = &firecracker.Jail{ID: spec.ID, ChrootBase: b.ChrootBase, UID: os.Getuid(), GID: os.Getgid(), ExecFile: rt.JailExec}
	}
	paths := fcPathsFor(spec.ID, jail)
	proc, err := firecracker.Attach(paths.Paths, rt.Pid)
	if err != nil {
		b.removeLeftovers(spec.ID)
		return instance.StateStopped, nil
	}
	if err := proc.WaitAPI(ctx); err != nil {
		// Left running untracked, a later Start would open the same disk in a second firecracker.
		_ = proc.Stop(ctx, 0)
		b.removeLeftovers(spec.ID)
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d was alive but its API was unreachable, killed it: %w", rt.Pid, err)
	}
	b.mu.Lock()
	b.paths[spec.ID] = paths
	b.mu.Unlock()

	// The proxies died with the previous anvild; the SSH port stays the same when it's still free.
	if standalone(spec.VM) {
		if err := ensureFCNetwork(); err != nil {
			log.Printf("vm: %s: %v", spec.Name, err)
		}
		if err := b.startProxies(spec, spec.VM.SSHPort); err != nil {
			log.Printf("vm: %s: restoring port forwards: %v", spec.Name, err)
		}
	}
	if err := b.exportMounts(spec); err != nil {
		log.Printf("vm: %s: restoring mounts: %v", spec.Name, err)
	}
	b.track(spec.ID, proc)
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, proc)
	return instance.StateRunning, nil
}

func hasExited(proc *firecracker.Process) bool {
	select {
	case <-proc.Exited():
		return true
	default:
		return false
	}
}

// removeLeftovers clears what a firecracker that's gone untracked can leave: its tap, jail
// (whose disk hard link keeps the disk's space in use) and runtime state.
func (b *FCBackend) removeLeftovers(instanceID string) {
	dropGuestFilter(instanceID)
	if err := b.Networker.Detach(instanceID); err != nil {
		log.Printf("vm: removing %s's tap: %v", instanceID, err)
	}
	jails, _ := filepath.Glob(filepath.Join(b.ChrootBase, "*", instanceID))
	for _, dir := range jails {
		_ = os.RemoveAll(dir)
	}
	removeRuntimeState(instanceDir(instanceID))
}

func (b *FCBackend) Delete(ctx context.Context, spec *instance.Spec) error {
	if err := b.Stop(ctx, spec, true, 0); err != nil && b.tracked(spec.ID) {
		return err
	}
	b.removeLeftovers(spec.ID)
	b.ipam.release(spec.ID)
	if err := os.RemoveAll(instanceDir(spec.ID)); err != nil {
		return fmt.Errorf("vm: removing instance dir: %w", err)
	}
	return nil
}

func (b *FCBackend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	proc, ok := b.lookup(spec.ID)
	if !ok {
		return instance.StateStopped, nil
	}
	info, err := proc.API.Info(ctx)
	if err != nil {
		return instance.StateError, err
	}
	if info.State == firecracker.StateRunning {
		return instance.StateRunning, nil
	}
	return instance.StateStopped, nil
}

// Logs streams the serial console, the same console.log a QEMU VM has.
func (b *FCBackend) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	return b.QEMU.Logs(ctx, spec, follow, tailLines, send)
}

func (b *FCBackend) WaitReady(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	proc, ok := b.lookup(spec.ID)
	if !ok {
		return fmt.Errorf("vm: %s is not running", spec.Name)
	}
	return b.waitReady(ctx, spec, proc.Exited(), progress)
}

func (b *FCBackend) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	if spec.VM == nil {
		return instance.Stats{}, fmt.Errorf("vm: Stats called with a nil VMSpec")
	}
	proc, ok := b.lookup(spec.ID)
	if !ok {
		return instance.Stats{}, fmt.Errorf("vm: %s isn't running", spec.Name)
	}
	return sampleStats(ctx, spec, proc.Pid(), b.Networker, true)
}

func proxyKey(hostPort int, protocol string) string {
	return fmt.Sprintf("%d/%s", hostPort, instance.Protocol(protocol))
}

// startProxies forwards SSH (from sshPort, 0 for any free port, into v.SSHPort) and every
// published port to the guest's address.
func (b *FCBackend) startProxies(spec *instance.Spec, sshPort int) error {
	v := spec.VM
	guest := ipam.StripCIDR(v.StaticIP)
	ssh, err := startPortProxy("tcp", sshPort, net.JoinHostPort(guest, "22"))
	if err != nil && sshPort != 0 {
		ssh, err = startPortProxy("tcp", 0, net.JoinHostPort(guest, "22"))
	}
	if err != nil {
		return fmt.Errorf("vm: forwarding SSH: %w", err)
	}
	v.SSHPort = ssh.HostPort
	set := map[string]*portProxy{"ssh": ssh}
	for _, p := range v.Ports {
		pp, err := startPortProxy(instance.Protocol(p.Protocol), p.HostPort, net.JoinHostPort(guest, strconv.Itoa(p.GuestPort)))
		if err != nil {
			for _, open := range set {
				open.Close()
			}
			return err
		}
		set[proxyKey(p.HostPort, p.Protocol)] = pp
	}
	b.mu.Lock()
	b.proxies[spec.ID] = set
	b.mu.Unlock()
	return nil
}

func (b *FCBackend) closeProxies(instanceID string) {
	b.mu.Lock()
	set := b.proxies[instanceID]
	delete(b.proxies, instanceID)
	b.mu.Unlock()
	for _, p := range set {
		p.Close()
	}
}

// AddPort publishes a host port into a standalone VM, live through a new proxy if it runs.
func (b *FCBackend) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: AddPort called with a nil VMSpec")
	}
	proto, err := checkNewPort(spec, port)
	if err != nil {
		return err
	}
	// Under mu, so a stop can't drop the VM's proxy set between the check and the insert.
	b.mu.Lock()
	defer b.mu.Unlock()
	if set, running := b.proxies[spec.ID]; running {
		target := net.JoinHostPort(ipam.StripCIDR(spec.VM.StaticIP), strconv.Itoa(port.GuestPort))
		pp, err := startPortProxy(proto, port.HostPort, target)
		if err != nil {
			return fmt.Errorf("vm: adding live port forward: %w", err)
		}
		set[proxyKey(port.HostPort, proto)] = pp
	}
	spec.VM.Ports = append(spec.VM.Ports, instance.PortMapping{HostPort: port.HostPort, GuestPort: port.GuestPort, Protocol: proto})
	return nil
}

func (b *FCBackend) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RemovePort called with a nil VMSpec")
	}
	proto := instance.Protocol(protocol)
	idx, err := portIndex(spec, hostPort, proto)
	if err != nil {
		return err
	}
	b.mu.Lock()
	key := proxyKey(hostPort, proto)
	pp := b.proxies[spec.ID][key]
	delete(b.proxies[spec.ID], key)
	b.mu.Unlock()
	if pp != nil {
		pp.Close()
	}
	spec.VM.Ports = slices.Delete(spec.VM.Ports, idx, idx+1)
	return nil
}
