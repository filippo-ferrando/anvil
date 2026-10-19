//go:build linux

package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var (
	_ instance.Snapshotter = (*FCBackend)(nil)
	_ instance.Forker      = (*FCBackend)(nil)
	_ instance.Resizer     = (*FCBackend)(nil)
	_ instance.LiveResizer = (*FCBackend)(nil)
)

// fcRootDrive is the drive ID of a Firecracker VM's disk.
const fcRootDrive = "rootfs"

// A Firecracker snapshot is a reflink copy of the disk, like a QCOW2 internal snapshot
// only covering the disk: restoring one always boots fresh.
func fcSnapshotPath(instanceID, name string) string {
	return filepath.Join(instanceDir(instanceID), "snapshots", name+".raw")
}

// copyDisk copies spec's disk to dest. A running VM is paused for the copy, which a
// reflink makes near instant; with the guest agent its filesystems are frozen around it.
func (b *FCBackend) copyDisk(ctx context.Context, spec *instance.Spec, dest string, progress func(string)) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running {
		say("copying disk")
		return image.CloneDisk(ctx, spec.VM.DiskPath, dest, 0)
	}

	frozen := false
	if info, _ := b.GuestInfo(spec); info.AgentConnected {
		say("freezing guest filesystems")
		if err := b.agentFor(spec.ID).Do(ctx, func(c *qemu.QGAConn) error { return c.FreezeFilesystems() }); err != nil {
			say(fmt.Sprintf("could not freeze guest filesystems (%v), copy is crash-consistent", err))
		} else {
			frozen = true
		}
	}
	// Thaw and resume even when ctx is gone, or the guest stays stuck.
	cleanup := context.WithoutCancel(ctx)
	defer func() {
		if frozen {
			if err := b.agentFor(spec.ID).Do(cleanup, func(c *qemu.QGAConn) error { return c.ThawFilesystems() }); err != nil {
				log.Printf("vm: thawing %s's filesystems: %v", spec.Name, err)
			}
		}
	}()
	if err := proc.API.Pause(ctx); err != nil {
		return fmt.Errorf("vm: pausing %s: %w", spec.Name, err)
	}
	say("copying disk while paused")
	copyErr := image.CloneDisk(ctx, spec.VM.DiskPath, dest, 0)
	if err := proc.API.Resume(cleanup); err != nil {
		return fmt.Errorf("vm: resuming %s: %w", spec.Name, err)
	}
	return copyErr
}

func (b *FCBackend) CreateSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: CreateSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	path := fcSnapshotPath(spec.ID, name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("vm: %s already has a snapshot named %q; delete it first", spec.Name, name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("vm: creating snapshot dir: %w", err)
	}
	if err := b.copyDisk(ctx, spec, path, nil); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("vm: creating snapshot %q: %w", name, err)
	}
	return nil
}

// RestoreSnapshot puts the disk back as it was; a running VM is stopped first and started again.
func (b *FCBackend) RestoreSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if spec.VM == nil {
		return fmt.Errorf("vm: RestoreSnapshot called with a nil VMSpec")
	}
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	path := fcSnapshotPath(spec.ID, name)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("vm: %s has no snapshot named %q", spec.Name, name)
	}

	b.mu.Lock()
	_, running := b.running[spec.ID]
	b.mu.Unlock()
	if running {
		if err := b.Stop(ctx, spec, false, 30*time.Second); err != nil {
			return fmt.Errorf("vm: stopping %s to restore snapshot %q: %w", spec.Name, name, err)
		}
	}
	// Copied next to the disk and renamed over it, so a failed copy leaves the disk whole.
	tmp := spec.VM.DiskPath + ".restore"
	applyErr := image.CloneDisk(ctx, path, tmp, 0)
	if applyErr == nil {
		applyErr = os.Rename(tmp, spec.VM.DiskPath)
	}
	_ = os.Remove(tmp)

	if running {
		if err := b.Start(ctx, spec); err != nil {
			if applyErr != nil {
				return fmt.Errorf("vm: restoring snapshot %q: %w (restarting %s afterward also failed: %v)", name, applyErr, spec.Name, err)
			}
			return fmt.Errorf("vm: restarting %s after restoring snapshot %q: %w", spec.Name, name, err)
		}
	}
	if applyErr != nil {
		return fmt.Errorf("vm: restoring snapshot %q: %w", name, applyErr)
	}
	return nil
}

func (b *FCBackend) DeleteSnapshot(ctx context.Context, spec *instance.Spec, name string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}
	if err := os.Remove(fcSnapshotPath(spec.ID, name)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vm: %s has no snapshot named %q", spec.Name, name)
		}
		return fmt.Errorf("vm: deleting snapshot %q: %w", name, err)
	}
	return nil
}

func (b *FCBackend) ListSnapshots(ctx context.Context, spec *instance.Spec) ([]instance.Snapshot, error) {
	entries, err := os.ReadDir(filepath.Dir(fcSnapshotPath(spec.ID, "x")))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vm: listing snapshots: %w", err)
	}
	var out []instance.Snapshot
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".raw")
		if !ok || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, instance.Snapshot{Name: name, CreatedAt: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Fork copies source's disk for dest (paused for the copy if source runs) and gives dest
// its own address and seed. The fork always starts standalone.
func (b *FCBackend) Fork(ctx context.Context, source, dest *instance.Spec, progress func(status string)) (err error) {
	if source.VM == nil || dest.VM == nil {
		return fmt.Errorf("vm: Fork called with a nil VMSpec")
	}
	dir := instanceDir(dest.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("vm: creating instance dir: %w", err)
	}
	diskPath := filepath.Join(dir, "disk.raw")
	if err := b.copyDisk(ctx, source, diskPath, progress); err != nil {
		_ = os.Remove(diskPath)
		return fmt.Errorf("vm: forking disk: %w", err)
	}
	dest.VM.DiskPath = diskPath
	if standalone(dest.VM) {
		if err := b.assignAddress(dest); err != nil {
			return err
		}
		defer func() {
			if err != nil {
				b.ipam.release(dest.ID)
			}
		}()
	}
	if progress != nil {
		progress("building cloud-init seed")
	}
	return b.QEMU.buildSeed(dest)
}

// assignAddress gives a standalone spec a free address on FCBridge, its gateway and DNS.
func (b *FCBackend) assignAddress(spec *instance.Spec) error {
	subnet, gateway, err := fcNetwork()
	if err != nil {
		return err
	}
	addr, err := b.ipam.allocate(spec.ID, subnet)
	if err != nil {
		return err
	}
	v := spec.VM
	v.StaticIP, v.Gateway = addr, gateway.String()
	v.DNSServers, v.DNSSearch = []string{gateway.String()}, nil
	return nil
}

// ResizeDisk grows the disk file. A running VM sees the new size right away, and with the
// guest agent its root partition and filesystem grow too; otherwise at the next boot.
func (b *FCBackend) ResizeDisk(ctx context.Context, spec *instance.Spec, newGiB int64) (string, error) {
	if spec.VM == nil || spec.VM.DiskPath == "" {
		return "", fmt.Errorf("vm: %s has no disk to resize", spec.Name)
	}
	info, err := os.Stat(spec.VM.DiskPath)
	if err != nil {
		return "", fmt.Errorf("vm: inspecting %s's disk: %w", spec.Name, err)
	}
	newBytes := newGiB * bytesPerGiB
	if newBytes <= info.Size() {
		return "", fmt.Errorf("vm: disk can only grow: %s is already %.1f GiB", spec.Name, float64(info.Size())/bytesPerGiB)
	}
	if err := os.Truncate(spec.VM.DiskPath, newBytes); err != nil {
		return "", fmt.Errorf("vm: resizing disk: %w", err)
	}

	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running {
		return fmt.Sprintf("disk grown to %d GiB; the guest filesystem grows at the next boot", newGiB), nil
	}
	if err := proc.API.UpdateDrive(ctx, fcRootDrive, spec.VM.DiskPath); err != nil {
		return "", fmt.Errorf("vm: telling the guest about the new disk size: %w", err)
	}
	note := fmt.Sprintf("disk grown live to %d GiB", newGiB)
	if info, _ := b.GuestInfo(spec); info.AgentConnected {
		if _, err := b.guestExec(ctx, spec.ID, growGuestFSScript); err == nil {
			return note + "; guest partition and filesystem grown", nil
		}
	}
	return note + "; the guest filesystem grows at the next boot", nil
}

// SetCPUsLive always waits for the next start: firecracker has no vCPU hotplug.
func (b *FCBackend) SetCPUsLive(ctx context.Context, spec *instance.Spec, cpus int) (string, error) {
	return "", needsRestart("firecracker can't change vCPUs of a running VM")
}

// fcMemSlotMiB is firecracker's default virtio-mem slot size; the hotplug region is a whole number of slots.
const fcMemSlotMiB = 128

// fcMemBlockMiB is firecracker's default virtio-mem block size, the step memory changes by.
const fcMemBlockMiB = 2

// fcHotplugMiB is the virtio-mem region a VM boots with: room up to the host's memory.
func fcHotplugMiB(arch string, cpus int, bootMiB int64) int64 {
	_, maxMiB := hotplugHeadroom(arch, cpus, bootMiB)
	extra := maxMiB - bootMiB
	if extra <= 0 {
		return 0
	}
	return (extra + fcMemSlotMiB - 1) / fcMemSlotMiB * fcMemSlotMiB
}

// SetMemoryLive plugs or unplugs virtio-mem memory of a running VM, between what it
// booted with and the region it booted with on top.
func (b *FCBackend) SetMemoryLive(ctx context.Context, spec *instance.Spec, memoryMiB int64) (string, error) {
	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running {
		return "", needsRestart("%s is not running", spec.Name)
	}
	rt, _, err := loadRuntimeState(instanceDir(spec.ID))
	if err != nil {
		return "", fmt.Errorf("vm: reading %s's runtime state: %w", spec.Name, err)
	}
	if rt.BootMemoryMiB == 0 || rt.MaxMemoryMiB <= rt.BootMemoryMiB {
		return "", needsRestart("it was started without room for more memory")
	}
	if memoryMiB < rt.BootMemoryMiB {
		return "", needsRestart("it can't go below the %d MiB it booted with while running", rt.BootMemoryMiB)
	}
	if memoryMiB > rt.MaxMemoryMiB {
		return "", needsRestart("it can take at most %d MiB until restarted", rt.MaxMemoryMiB)
	}
	extraMiB := memoryMiB - rt.BootMemoryMiB
	extraMiB -= extraMiB % fcMemBlockMiB
	if err := proc.API.SetMemoryHotplug(ctx, extraMiB); err != nil {
		return "", fmt.Errorf("vm: %w", err)
	}
	return b.settleVirtioMem(ctx, spec, rt.BootMemoryMiB, extraMiB, func(ctx context.Context) (int64, error) {
		st, err := proc.API.MemoryHotplug(ctx)
		return st.PluggedSizeMiB, err
	})
}
