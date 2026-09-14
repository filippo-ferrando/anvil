package vm

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/vm/image"
	"github.com/anvil-project/anvil/internal/vm/network"
)

// statsSampleWindow bounds how long Stats waits between its two live
// samples when computing CPU/network rates.
const statsSampleWindow = 200 * time.Millisecond

// clockTicksPerSec is Linux's USER_HZ, used to convert /proc/<pid>/stat's
// utime/stime fields into seconds. 100 on every mainstream distro/kernel.
const clockTicksPerSec = 100

var _ instance.StatsProvider = (*Backend)(nil)

// Stats samples spec's live resource usage: CPU and (for a bridged VM)
// network throughput are computed from two short samples taken statsSampleWindow
// apart; memory and disk space are single point-in-time reads.
func (b *Backend) Stats(ctx context.Context, spec *instance.Spec) (instance.Stats, error) {
	if spec.VM == nil {
		return instance.Stats{}, fmt.Errorf("vm: Stats called with a nil VMSpec")
	}
	v := spec.VM

	b.mu.Lock()
	proc, ok := b.running[spec.ID]
	b.mu.Unlock()
	if !ok {
		return instance.Stats{}, fmt.Errorf("vm: %s isn't running", spec.Name)
	}
	pid := proc.Pid()

	tapName := ""
	if v.NetworkMode == "bridge" {
		tapName = network.TapName(spec.ID)
	}

	cpu0, err := readProcCPUTicks(pid)
	if err != nil {
		return instance.Stats{}, fmt.Errorf("vm: reading CPU usage: %w", err)
	}
	var rx0, tx0 uint64
	if tapName != "" {
		rx0, tx0, _ = network.TapStats(tapName)
	}
	t0 := time.Now()

	select {
	case <-time.After(statsSampleWindow):
	case <-ctx.Done():
		return instance.Stats{}, ctx.Err()
	}

	cpu1, err := readProcCPUTicks(pid)
	if err != nil {
		return instance.Stats{}, fmt.Errorf("vm: reading CPU usage: %w", err)
	}
	elapsed := time.Since(t0).Seconds()

	vcpus := v.CPUs
	if vcpus <= 0 {
		vcpus = 1
	}
	var cpuPercent float64
	if elapsed > 0 {
		cpuSeconds := float64(cpu1-cpu0) / clockTicksPerSec
		cpuPercent = cpuSeconds / elapsed / float64(vcpus) * 100
	}

	memUsed, _ := readRSSBytes(pid)

	var diskUsed, diskTotal int64
	if v.DiskPath != "" {
		diskUsed, diskTotal, _ = image.DiskUsage(v.DiskPath)
	}

	stats := instance.Stats{
		CPUPercent:     cpuPercent,
		MemUsedBytes:   memUsed,
		MemLimitBytes:  v.MemoryMiB * 1024 * 1024,
		DiskUsedBytes:  diskUsed,
		DiskTotalBytes: diskTotal,
		UptimeSeconds:  vmUptimeSeconds(spec.ID),
		Address:        vmAddress(v),
	}

	if tapName != "" {
		if rx1, tx1, err := network.TapStats(tapName); err == nil {
			stats.NetAvailable = true
			if elapsed > 0 {
				stats.NetRxBytesPerSec = float64(rx1-rx0) / elapsed
				stats.NetTxBytesPerSec = float64(tx1-tx0) / elapsed
			}
		}
	}

	return stats, nil
}

// vmUptimeSeconds reads how long ago instanceID's process was started, 0 if unknown.
func vmUptimeSeconds(instanceID string) int64 {
	rt, found, err := loadRuntimeState(config.InstanceDir(instanceID))
	if err != nil || !found || rt.StartedAt.IsZero() {
		return 0
	}
	if d := time.Since(rt.StartedAt); d > 0 {
		return int64(d.Seconds())
	}
	return 0
}

// vmAddress returns v's best-known reachable address: its bridge IP, or
// the SLIRP-forwarded loopback port.
func vmAddress(v *instance.VMSpec) string {
	if v.NetworkMode == "bridge" && v.StaticIP != "" {
		ip, _, _ := strings.Cut(v.StaticIP, "/")
		return ip
	}
	if v.SSHPort > 0 {
		return fmt.Sprintf("127.0.0.1:%d", v.SSHPort)
	}
	return ""
}

// readProcCPUTicks returns pid's total (user+system) CPU ticks consumed so far.
func readProcCPUTicks(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The comm field (2nd, parenthesized) can itself contain spaces/parens,
	// so split after its closing ')' rather than on whitespace throughout.
	idx := strings.LastIndex(string(data), ")")
	if idx == -1 {
		return 0, fmt.Errorf("vm: unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(string(data)[idx+1:])
	// fields[0] is stat's field 3 (state); utime is field 14, stime is field 15.
	if len(fields) < 13 {
		return 0, fmt.Errorf("vm: /proc/%d/stat has too few fields", pid)
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("vm: parsing /proc/%d/stat", pid)
	}
	return utime + stime, nil
}

// readRSSBytes returns pid's resident set size in bytes.
func readRSSBytes(pid int) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "VmRSS:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("vm: VmRSS not found in /proc/%d/status", pid)
}
