//go:build linux

package vm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/qemu"
)

var _ instance.LiveResizer = (*Backend)(nil)

const (
	// maxHotplugCPUs caps the vCPU slots a VM boots with, whatever the host has.
	maxHotplugCPUs = 64
	// hotplugWait bounds how long a live change waits for the guest to take or release it.
	hotplugWait = 30 * time.Second
	// shrinkSettle is how long a memory shrink waits once the guest stops giving memory back.
	shrinkSettle = 5 * time.Second
)

// hotplugHeadroom is how far a VM can grow while running: up to the host's CPU count
// and memory. Only x86_64 (q35) supports adding vCPUs and virtio-mem here.
func hotplugHeadroom(arch string, cpus int, memMiB int64) (maxCPUs int, maxMemMiB int64) {
	if arch != "x86_64" {
		return 0, 0
	}
	if cpus <= 0 {
		cpus = 1
	}
	if memMiB <= 0 {
		memMiB = 1024
	}
	maxCPUs = max(cpus, min(runtime.NumCPU(), maxHotplugCPUs))
	maxMemMiB = max(memMiB, hostMemoryMiB())
	return maxCPUs, maxMemMiB
}

// hostMemoryMiB reads MemTotal from /proc/meminfo; 0 if it can't.
func hostMemoryMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, _ := strconv.ParseInt(fields[1], 10, 64)
			return kib / 1024
		}
	}
	return 0
}

func needsRestart(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), instance.ErrNeedsRestart)
}

// liveTarget returns spec's QEMU process and what it was started with.
func (b *Backend) liveTarget(spec *instance.Spec) (*qemu.Process, runtimeState, error) {
	b.mu.Lock()
	proc, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running || proc.QMP == nil {
		return nil, runtimeState{}, needsRestart("%s is not running", spec.Name)
	}
	rt, _, err := loadRuntimeState(instanceDir(spec.ID))
	if err != nil {
		return nil, runtimeState{}, fmt.Errorf("vm: reading %s's runtime state: %w", spec.Name, err)
	}
	return proc, rt, nil
}

// SetCPUsLive plugs or unplugs vCPUs of a running VM. Adding is immediate; removing waits
// for the guest to take each vCPU offline. The guest agent brings new vCPUs online.
func (b *Backend) SetCPUsLive(ctx context.Context, spec *instance.Spec, cpus int) (string, error) {
	proc, rt, err := b.liveTarget(spec)
	if err != nil {
		return "", err
	}
	if rt.MaxCPUs == 0 {
		return "", needsRestart("it was started without room for more vCPUs")
	}
	if cpus > rt.MaxCPUs {
		return "", needsRestart("it can take at most %d vCPUs until restarted", rt.MaxCPUs)
	}
	slots, err := proc.QMP.HotpluggableCPUs(ctx)
	if err != nil {
		return "", fmt.Errorf("vm: %w", err)
	}
	var present, free []qemu.HotpluggableCPU
	for _, s := range slots {
		if s.QOMPath != "" {
			present = append(present, s)
		} else {
			free = append(free, s)
		}
	}
	sortSlots(present)
	sortSlots(free)

	switch {
	case cpus > len(present):
		for _, slot := range free[:cpus-len(present)] {
			if err := proc.QMP.AddCPU(ctx, slot); err != nil {
				return "", fmt.Errorf("vm: %w", err)
			}
		}
		note := fmt.Sprintf("cpus set to %d live", cpus)
		if info, _ := b.GuestInfo(spec); info.AgentConnected {
			b.onlineGuestCPUs(ctx, spec.ID)
		}
		return note, nil

	case cpus < len(present):
		// The highest sockets go first; socket 0 (the boot vCPU) always stays.
		for i := len(present) - 1; i >= cpus; i-- {
			if present[i].Props.SocketID == 0 {
				continue
			}
			if err := proc.QMP.RemoveCPU(ctx, present[i].QOMPath); err != nil {
				return "", fmt.Errorf("vm: %w", err)
			}
		}
		got, err := b.waitForCPUs(ctx, proc, cpus)
		if err != nil {
			return "", err
		}
		if got != cpus {
			return "", needsRestart("the guest released only %d of %d vCPUs", len(present)-got, len(present)-cpus)
		}
		return fmt.Sprintf("cpus set to %d live", cpus), nil
	}
	return fmt.Sprintf("cpus already %d", cpus), nil
}

func sortSlots(s []qemu.HotpluggableCPU) {
	sort.Slice(s, func(i, j int) bool { return s[i].Props.SocketID < s[j].Props.SocketID })
}

// waitForCPUs polls until the VM holds want vCPUs or hotplugWait passes, and returns the count.
func (b *Backend) waitForCPUs(ctx context.Context, proc *qemu.Process, want int) (int, error) {
	deadline := time.Now().Add(hotplugWait)
	for {
		slots, err := proc.QMP.HotpluggableCPUs(ctx)
		if err != nil {
			return 0, fmt.Errorf("vm: %w", err)
		}
		n := 0
		for _, s := range slots {
			if s.QOMPath != "" {
				n++
			}
		}
		if n <= want || time.Now().After(deadline) {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// onlineGuestCPUs onlines new vCPUs through the agent, retrying while the guest kernel
// is still registering them. Best effort: many distros online them by themselves.
func (b *Backend) onlineGuestCPUs(ctx context.Context, instanceID string) {
	for range 10 {
		_ = b.agentFor(instanceID).Do(ctx, func(c *qemu.QGAConn) error {
			_, err := c.OnlineAllCPUs()
			return err
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// SetMemoryLive grows or shrinks a running VM's memory through its virtio-mem device,
// between the memory it booted with and the host's total. It waits for the guest to
// take or give back the memory.
func (b *Backend) SetMemoryLive(ctx context.Context, spec *instance.Spec, memoryMiB int64) (string, error) {
	proc, rt, err := b.liveTarget(spec)
	if err != nil {
		return "", err
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
	extraMiB -= extraMiB % qemu.VirtioMemBlockMiB
	want := extraMiB << 20
	if err := proc.QMP.SetVirtioMemRequested(ctx, want); err != nil {
		return "", fmt.Errorf("vm: %w", err)
	}
	agent := false
	if info, _ := b.GuestInfo(spec); info.AgentConnected {
		agent = true
	}

	start, lastChange, prev := time.Now(), time.Now(), int64(-1)
	var size int64
	for {
		size, err = proc.QMP.VirtioMemSize(ctx)
		if err != nil {
			return "", fmt.Errorf("vm: %w", err)
		}
		if size == want {
			break
		}
		if size != prev {
			prev, lastChange = size, time.Now()
		}
		if size < want && time.Since(start) > hotplugWait {
			return "", needsRestart("the guest took only %d of %d MiB (does its kernel have virtio-mem?)", size>>20, extraMiB)
		}
		// Memory still in use can't be given back; once the guest stops releasing, settle for what it freed.
		if size > want && time.Since(lastChange) > shrinkSettle {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	if agent {
		// Some distros leave added memory offline until told otherwise.
		_ = b.agentFor(spec.ID).Do(ctx, func(c *qemu.QGAConn) error { return c.OnlineAllMemory() })
	}
	note := fmt.Sprintf("memory set to %d MiB live", rt.BootMemoryMiB+extraMiB)
	if kept := (size - want) >> 20; kept > 0 {
		// The virtio-mem driver keeps retrying in the background.
		note += fmt.Sprintf("; the guest still holds %d MiB it can't free yet", kept)
	}
	return note, nil
}
