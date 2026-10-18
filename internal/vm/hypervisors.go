//go:build linux

package vm

import (
	"context"
	"fmt"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// Hypervisors is the one instance.KindVM backend the Manager sees. It hands each
// call to QEMU or Firecracker based on spec.VM.Hypervisor.
type Hypervisors struct {
	QEMU        *Backend
	Firecracker instance.Backend // nil until the Firecracker backend lands
}

var (
	_ instance.Backend         = (*Hypervisors)(nil)
	_ instance.Reconciler      = (*Hypervisors)(nil)
	_ instance.Mounter         = (*Hypervisors)(nil)
	_ instance.PortForwarder   = (*Hypervisors)(nil)
	_ instance.Snapshotter     = (*Hypervisors)(nil)
	_ instance.Forker          = (*Hypervisors)(nil)
	_ instance.StatsProvider   = (*Hypervisors)(nil)
	_ instance.GuestInspector  = (*Hypervisors)(nil)
	_ instance.ReadyWaiter     = (*Hypervisors)(nil)
	_ instance.ExitNotifier    = (*Hypervisors)(nil)
	_ instance.GUIPreparer     = (*Hypervisors)(nil)
	_ instance.GuestFileReader = (*Hypervisors)(nil)
	_ instance.Resizer         = (*Hypervisors)(nil)
	_ instance.LiveResizer     = (*Hypervisors)(nil)
)

func NewHypervisors(qemu *Backend) *Hypervisors {
	return &Hypervisors{QEMU: qemu}
}

// all returns every configured hypervisor backend, for calls that aren't about one spec.
func (h *Hypervisors) all() []instance.Backend {
	out := []instance.Backend{h.QEMU}
	if h.Firecracker != nil {
		out = append(out, h.Firecracker)
	}
	return out
}

// backendFor picks spec's hypervisor backend.
func (h *Hypervisors) backendFor(spec *instance.Spec) (instance.Backend, error) {
	if spec.VM == nil {
		return nil, fmt.Errorf("vm: spec has no VMSpec")
	}
	switch hv := spec.VM.HypervisorOf(); hv {
	case instance.VMHypervisorQEMU:
		return h.QEMU, nil
	case instance.VMHypervisorFirecracker:
		if h.Firecracker == nil {
			return nil, fmt.Errorf("vm: firecracker support isn't implemented yet")
		}
		return h.Firecracker, nil
	default:
		return nil, fmt.Errorf("vm: unknown hypervisor %q (want qemu or firecracker)", hv)
	}
}

// capability returns spec's backend as T, or an error naming the missing operation.
func capability[T any](h *Hypervisors, spec *instance.Spec, op string) (T, error) {
	var zero T
	b, err := h.backendFor(spec)
	if err != nil {
		return zero, err
	}
	c, ok := b.(T)
	if !ok {
		return zero, fmt.Errorf("vm: %s isn't supported on %s", op, spec.VM.HypervisorOf())
	}
	return c, nil
}

func (h *Hypervisors) Create(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	b, err := h.backendFor(spec)
	if err != nil {
		return err
	}
	return b.Create(ctx, spec, progress)
}

func (h *Hypervisors) Start(ctx context.Context, spec *instance.Spec) error {
	b, err := h.backendFor(spec)
	if err != nil {
		return err
	}
	return b.Start(ctx, spec)
}

func (h *Hypervisors) Stop(ctx context.Context, spec *instance.Spec, force bool, timeout time.Duration) error {
	b, err := h.backendFor(spec)
	if err != nil {
		return err
	}
	return b.Stop(ctx, spec, force, timeout)
}

func (h *Hypervisors) Delete(ctx context.Context, spec *instance.Spec) error {
	b, err := h.backendFor(spec)
	if err != nil {
		return err
	}
	return b.Delete(ctx, spec)
}

func (h *Hypervisors) Status(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	b, err := h.backendFor(spec)
	if err != nil {
		return instance.StateError, err
	}
	return b.Status(ctx, spec)
}

func (h *Hypervisors) Logs(ctx context.Context, spec *instance.Spec, follow bool, tailLines int, send func([]byte) error) error {
	b, err := h.backendFor(spec)
	if err != nil {
		return err
	}
	return b.Logs(ctx, spec, follow, tailLines, send)
}

func (h *Hypervisors) Reconcile(ctx context.Context, spec *instance.Spec) (instance.State, error) {
	r, err := capability[instance.Reconciler](h, spec, "reconcile")
	if err != nil {
		return instance.StateStopped, err
	}
	return r.Reconcile(ctx, spec)
}

func (h *Hypervisors) Mount(ctx context.Context, spec *instance.Spec, hostPath, guestPath string, readOnly bool) error {
	m, err := capability[instance.Mounter](h, spec, "mount")
	if err != nil {
		return err
	}
	return m.Mount(ctx, spec, hostPath, guestPath, readOnly)
}

func (h *Hypervisors) Umount(ctx context.Context, spec *instance.Spec, guestPath string) error {
	m, err := capability[instance.Mounter](h, spec, "umount")
	if err != nil {
		return err
	}
	return m.Umount(ctx, spec, guestPath)
}

func (h *Hypervisors) AddPort(ctx context.Context, spec *instance.Spec, port instance.PortMapping) error {
	pf, err := capability[instance.PortForwarder](h, spec, "port forwarding")
	if err != nil {
		return err
	}
	return pf.AddPort(ctx, spec, port)
}

func (h *Hypervisors) RemovePort(ctx context.Context, spec *instance.Spec, hostPort int, protocol string) error {
	pf, err := capability[instance.PortForwarder](h, spec, "port forwarding")
	if err != nil {
		return err
	}
	return pf.RemovePort(ctx, spec, hostPort, protocol)
}

func (h *Hypervisors) CreateSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	s, err := capability[instance.Snapshotter](h, spec, "snapshots")
	if err != nil {
		return err
	}
	return s.CreateSnapshot(ctx, spec, name)
}

func (h *Hypervisors) RestoreSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	s, err := capability[instance.Snapshotter](h, spec, "snapshots")
	if err != nil {
		return err
	}
	return s.RestoreSnapshot(ctx, spec, name)
}

func (h *Hypervisors) DeleteSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	s, err := capability[instance.Snapshotter](h, spec, "snapshots")
	if err != nil {
		return err
	}
	return s.DeleteSnapshot(ctx, spec, name)
}

func (h *Hypervisors) ListSnapshots(ctx context.Context, spec *instance.Spec) ([]instance.Snapshot, error) {
	s, err := capability[instance.Snapshotter](h, spec, "snapshots")
	if err != nil {
		return nil, err
	}
	return s.ListSnapshots(ctx, spec)
}

// Fork runs on source's hypervisor; the Manager's clone gives dest the same one.
func (h *Hypervisors) Fork(ctx context.Context, source, dest *instance.Spec, progress func(status string)) error {
	f, err := capability[instance.Forker](h, source, "fork")
	if err != nil {
		return err
	}
	return f.Fork(ctx, source, dest, progress)
}

func (h *Hypervisors) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	s, err := capability[instance.StatsProvider](h, spec, "stats")
	if err != nil {
		return instance.Stats{}, err
	}
	return s.Stats(ctx, spec)
}

func (h *Hypervisors) GuestInfo(spec *instance.Spec) (instance.GuestInfo, bool) {
	g, err := capability[instance.GuestInspector](h, spec, "guest info")
	if err != nil {
		return instance.GuestInfo{}, false
	}
	return g.GuestInfo(spec)
}

func (h *Hypervisors) SetGuestChangeHook(hook func(instanceID string)) {
	for _, b := range h.all() {
		if g, ok := b.(instance.GuestInspector); ok {
			g.SetGuestChangeHook(hook)
		}
	}
}

func (h *Hypervisors) SetExitHook(hook func(instanceID string, state instance.State, reason string)) {
	for _, b := range h.all() {
		if en, ok := b.(instance.ExitNotifier); ok {
			en.SetExitHook(hook)
		}
	}
}

func (h *Hypervisors) WaitReady(ctx context.Context, spec *instance.Spec, progress func(status string)) error {
	w, err := capability[instance.ReadyWaiter](h, spec, "waiting for cloud-init")
	if err != nil {
		return err
	}
	return w.WaitReady(ctx, spec, progress)
}

func (h *Hypervisors) PrepareGUI(ctx context.Context, spec *instance.Spec, x11 bool, progress func(status string)) (instance.GUISession, error) {
	g, err := capability[instance.GUIPreparer](h, spec, "GUI apps")
	if err != nil {
		return instance.GUISession{}, err
	}
	return g.PrepareGUI(ctx, spec, x11, progress)
}

func (h *Hypervisors) ListGuestDir(ctx context.Context, spec *instance.Spec, dir string) ([]string, error) {
	r, err := capability[instance.GuestFileReader](h, spec, "reading guest files")
	if err != nil {
		return nil, err
	}
	return r.ListGuestDir(ctx, spec, dir)
}

func (h *Hypervisors) ReadGuestFile(ctx context.Context, spec *instance.Spec, path string) ([]byte, bool, error) {
	r, err := capability[instance.GuestFileReader](h, spec, "reading guest files")
	if err != nil {
		return nil, false, err
	}
	return r.ReadGuestFile(ctx, spec, path)
}

func (h *Hypervisors) ResizeDisk(ctx context.Context, spec *instance.Spec, newGiB int64) (string, error) {
	r, err := capability[instance.Resizer](h, spec, "disk resize")
	if err != nil {
		return "", err
	}
	return r.ResizeDisk(ctx, spec, newGiB)
}

func (h *Hypervisors) SetCPUsLive(ctx context.Context, spec *instance.Spec, cpus int) (string, error) {
	r, err := capability[instance.LiveResizer](h, spec, "live CPU changes")
	if err != nil {
		return "", err
	}
	return r.SetCPUsLive(ctx, spec, cpus)
}

func (h *Hypervisors) SetMemoryLive(ctx context.Context, spec *instance.Spec, memoryMiB int64) (string, error) {
	r, err := capability[instance.LiveResizer](h, spec, "live memory changes")
	if err != nil {
		return "", err
	}
	return r.SetMemoryLive(ctx, spec, memoryMiB)
}

// ListCatalog lists the QEMU catalog, which every hypervisor shares distro ids with.
func (h *Hypervisors) ListCatalog() ([]image.DistroEntry, error) {
	return h.QEMU.ListCatalog()
}

// The disk export and import calls below are QEMU-only until phase 8 of the Firecracker plan.

func (h *Hypervisors) ExportDisk(ctx context.Context, spec *instance.Spec, destPath string) error {
	if err := h.qemuOnly(spec, "export"); err != nil {
		return err
	}
	return h.QEMU.ExportDisk(ctx, spec, destPath)
}

func (h *Hypervisors) BaseImageChecksum(spec *instance.Spec) (string, error) {
	if err := h.qemuOnly(spec, "export"); err != nil {
		return "", err
	}
	return h.QEMU.BaseImageChecksum(spec)
}

func (h *Hypervisors) BaseImagePath(spec *instance.Spec) (string, error) {
	if err := h.qemuOnly(spec, "export"); err != nil {
		return "", err
	}
	return h.QEMU.BaseImagePath(spec)
}

func (h *Hypervisors) ExportDiskDelta(ctx context.Context, spec *instance.Spec, destPath string) error {
	if err := h.qemuOnly(spec, "export"); err != nil {
		return err
	}
	return h.QEMU.ExportDiskDelta(ctx, spec, destPath)
}

func (h *Hypervisors) PrepareImportedDisk(ctx context.Context, imageRef, arch, diskPath, baseSHA256 string) error {
	return h.QEMU.PrepareImportedDisk(ctx, imageRef, arch, diskPath, baseSHA256)
}

func (h *Hypervisors) qemuOnly(spec *instance.Spec, op string) error {
	if spec.VM != nil && spec.VM.HypervisorOf() != instance.VMHypervisorQEMU {
		return fmt.Errorf("vm: %s isn't supported on %s yet", op, spec.VM.HypervisorOf())
	}
	return nil
}
