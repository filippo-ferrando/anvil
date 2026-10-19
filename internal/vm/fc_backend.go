//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
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

	guestTracker

	mu       sync.Mutex
	running  map[string]*firecracker.Process // instance ID -> live process
	starting map[string]struct{}             // instance ID -> in-progress Start call
	stopping map[string]struct{}             // instance ID -> Stop in progress, so its exit isn't reported
	exitHook func(instanceID string, state instance.State, reason string)
}

var (
	_ instance.Backend        = (*FCBackend)(nil)
	_ instance.Reconciler     = (*FCBackend)(nil)
	_ instance.StatsProvider  = (*FCBackend)(nil)
	_ instance.GuestInspector = (*FCBackend)(nil)
	_ instance.ReadyWaiter    = (*FCBackend)(nil)
	_ instance.ExitNotifier   = (*FCBackend)(nil)
)

func NewFCBackend(qemuBackend *Backend, cacheDir string) *FCBackend {
	return &FCBackend{
		QEMU:      qemuBackend,
		Cache:     &image.FCCache{Dir: cacheDir, Vault: qemuBackend.Vault},
		Networker: network.LinuxBridge{},
		guestTracker: newGuestTracker(func(id string) *qemu.GuestAgent {
			return &qemu.GuestAgent{Dial: func(ctx context.Context) (net.Conn, error) {
				return firecracker.DialVsock(ctx, fcPaths(id).vsock, fcAgentPort)
			}}
		}, "reboot"), // a guest poweroff doesn't end firecracker on x86_64, a reboot does
		running:  make(map[string]*firecracker.Process),
		starting: make(map[string]struct{}),
		stopping: make(map[string]struct{}),
	}
}

// fcLookPath finds the firecracker binary; tests swap it to run Create without one.
var fcLookPath = exec.LookPath

type fcInstancePaths struct {
	firecracker.Paths
	vsock string
}

func fcPaths(instanceID string) fcInstancePaths {
	dir := instanceDir(instanceID)
	return fcInstancePaths{
		Paths: firecracker.Paths{
			APISocket:  filepath.Join(dir, "firecracker.sock"),
			ConfigFile: filepath.Join(dir, "firecracker.json"),
			ConsoleLog: filepath.Join(dir, "console.log"),
			LogFile:    filepath.Join(dir, "firecracker.log"),
		},
		vsock: filepath.Join(dir, "vsock.sock"),
	}
}

// fcBootArgs is the guest kernel command line; Firecracker adds root= from the drive's partuuid.
// reboot=k makes a guest reboot end the firecracker process, panic=1 does the same on a panic.
func fcBootArgs(extra string) string {
	return strings.TrimSpace("console=ttyS0 reboot=k panic=1 " + extra)
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

func (b *FCBackend) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: Create called with a nil VMSpec")
	}
	v := spec.VM
	if v.SourceDiskPath != "" {
		return fmt.Errorf("vm: migrating or importing a firecracker VM isn't supported yet")
	}
	if _, err := fcLookPath(firecracker.Binary); err != nil {
		return fmt.Errorf("vm: firecracker isn't installed on this host: %w", err)
	}
	if v.MACAddress != "" {
		if _, err := net.ParseMAC(v.MACAddress); err != nil {
			return fmt.Errorf("vm: invalid MAC address %q: %w", v.MACAddress, err)
		}
	}
	v.MACAddress = v.MAC(spec.ID)

	entry, _, kernel, err := b.resolve(v)
	if err != nil {
		return err
	}
	v.Arch = entry.Arch
	v.DefaultUser = entry.DefaultUser

	dir := instanceDir(spec.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	// The kernel downloads on the first Firecracker launch; later launches find it cached.
	if _, err := b.Cache.Kernel(ctx, kernel, progress); err != nil {
		return fmt.Errorf("vm: preparing the firecracker kernel: %w", err)
	}
	raw, err := b.Cache.RawBase(ctx, entry, progress)
	if err != nil {
		return fmt.Errorf("vm: preparing disk: %w", err)
	}
	if progress != nil {
		progress("creating disk")
	}
	diskPath := filepath.Join(dir, "disk.raw")
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
	if spec.VM == nil {
		return fmt.Errorf("vm: Start called with a nil VMSpec")
	}
	v := spec.VM

	b.mu.Lock()
	if _, ok := b.running[spec.ID]; ok {
		b.mu.Unlock()
		return nil
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

	paths := fcPaths(spec.ID)
	cfg := firecracker.Config{
		BootSource:    firecracker.BootSource{KernelImagePath: kernelPath, BootArgs: fcBootArgs(distro.KernelArgs)},
		MachineConfig: firecracker.MachineConfig{VCPUCount: max(v.CPUs, 1), MemSizeMiB: v.MemoryMiB},
		Drives: []firecracker.Drive{
			{DriveID: "rootfs", PathOnHost: v.DiskPath, IsRootDevice: true, PartUUID: partUUID, Discard: true},
		},
		Vsock:   &firecracker.Vsock{GuestCID: firecracker.GuestCID, UDSPath: paths.vsock},
		Entropy: &struct{}{},
	}
	if v.SeedISOPath != "" {
		cfg.Drives = append(cfg.Drives, firecracker.Drive{DriveID: "seed", PathOnHost: v.SeedISOPath, IsReadOnly: true})
	}

	tapped := false
	if v.NetworkMode == "bridge" {
		tap, err := b.Networker.Attach(spec.ID, v.BridgeInterface)
		if err != nil {
			return fmt.Errorf("vm: attaching to bridge %s: %w", v.BridgeInterface, err)
		}
		tapped = true
		cfg.NetworkInterfaces = []firecracker.NetworkInterface{{IfaceID: "eth0", HostDevName: tap, GuestMAC: v.MAC(spec.ID)}}
		v.SSHPort = 0
	}
	// ponytail: a standalone VM has no NIC until the per-host bridge of phase 5 lands.
	unwind := func() {
		if tapped {
			_ = b.Networker.Detach(spec.ID)
		}
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
	rt := runtimeState{Pid: proc.Pid(), StartedAt: time.Now(), BootMemoryMiB: v.MemoryMiB}
	if err := saveRuntimeState(instanceDir(spec.ID), rt); err != nil {
		log.Printf("vm: failed to persist runtime state for %s: %v", spec.ID, err)
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.mu.Unlock()
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, v.NetworkMode, proc)
	return nil
}

func (b *FCBackend) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	if ok {
		b.stopping[spec.ID] = struct{}{}
	}
	b.mu.Unlock()
	if !ok {
		removeRuntimeState(instanceDir(spec.ID))
		return nil
	}
	defer func() {
		b.mu.Lock()
		delete(b.stopping, spec.ID)
		b.mu.Unlock()
	}()

	guest, _ := b.GuestInfo(spec)
	b.unwatchGuest(spec.ID)

	// The guest agent first, then Ctrl+Alt+Del, then a kill, all within timeout.
	var stopErr error
	start := time.Now()
	graceful := !force && timeout > 0
	if !graceful || !guest.AgentConnected || !b.agentShutdown(ctx, spec.ID, proc.Exited(), timeout) {
		stopTimeout := timeout - time.Since(start)
		if stopTimeout < minACPITimeout && timeout >= minACPITimeout {
			stopTimeout = minACPITimeout
		}
		if !graceful {
			stopTimeout = 0
		}
		stopErr = proc.Stop(ctx, stopTimeout)
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

func (b *FCBackend) SetExitHook(hook func(instanceID string, state instance.State, reason string)) {
	b.mu.Lock()
	b.exitHook = hook
	b.mu.Unlock()
}

// watchExit reports an exit not caused by Stop (guest reboot or poweroff, crash, kill).
func (b *FCBackend) watchExit(instanceID, networkMode string, proc *firecracker.Process) {
	<-proc.Exited()

	b.mu.Lock()
	_, stopping := b.stopping[instanceID]
	current := b.running[instanceID] == proc
	hook := b.exitHook
	b.mu.Unlock()
	if stopping || !current {
		return
	}

	state, reason := instance.StateStopped, "exited on its own (guest powered off or rebooted)"
	if !proc.ExitedCleanly() {
		state, reason = instance.StateError, "firecracker exited on its own ("+proc.ExitStatus()+")"
	}
	log.Printf("vm: %s exited on its own, now %s", instanceID, state)
	b.releaseStopped(instanceID, networkMode, proc)
	if hook != nil {
		hook(instanceID, state, reason)
	}
}

// releaseStopped frees everything a VM held while it ran, once firecracker is gone.
func (b *FCBackend) releaseStopped(instanceID, networkMode string, proc *firecracker.Process) {
	b.unwatchGuest(instanceID)
	_ = proc.Close()
	if networkMode == "bridge" {
		if err := b.Networker.Detach(instanceID); err != nil {
			log.Printf("vm: failed to detach network device for %s: %v", instanceID, err)
		}
	}
	removeRuntimeState(instanceDir(instanceID))

	b.mu.Lock()
	if b.running[instanceID] == proc {
		delete(b.running, instanceID)
	}
	b.mu.Unlock()
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
	proc, err := firecracker.Attach(fcPaths(spec.ID).Paths, rt.Pid)
	if err != nil {
		removeRuntimeState(dir)
		return instance.StateStopped, nil
	}
	if err := proc.WaitAPI(ctx); err != nil {
		return instance.StateError, fmt.Errorf("vm: reconciled pid %d is alive but its API is unreachable: %w", rt.Pid, err)
	}

	b.mu.Lock()
	b.running[spec.ID] = proc
	b.mu.Unlock()
	b.watchGuest(spec, proc.Exited())
	go b.watchExit(spec.ID, spec.VM.NetworkMode, proc)
	return instance.StateRunning, nil
}

func (b *FCBackend) Delete(ctx context.Context, spec *instance.Spec) error {
	_ = b.Stop(ctx, spec, true, 0)
	if err := os.RemoveAll(instanceDir(spec.ID)); err != nil {
		return fmt.Errorf("vm: removing instance dir: %w", err)
	}
	return nil
}

func (b *FCBackend) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
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
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("vm: %s is not running", spec.Name)
	}
	return b.waitReady(ctx, spec, proc.Exited(), progress)
}

func (b *FCBackend) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	if spec.VM == nil {
		return instance.Stats{}, fmt.Errorf("vm: Stats called with a nil VMSpec")
	}
	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
	if !ok {
		return instance.Stats{}, fmt.Errorf("vm: %s isn't running", spec.Name)
	}
	return sampleStats(ctx, spec, proc.Pid(), b.Networker, spec.VM.NetworkMode == "bridge")
}
