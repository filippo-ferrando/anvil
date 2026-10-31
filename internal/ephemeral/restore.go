package ephemeral

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Phase is one step of a clone restore and the time it took.
type Phase struct {
	Name string
	Took time.Duration
}

// Timed is a Clone that reports the phases of its restore.
type Timed interface {
	RestorePhases() []Phase
}

// reserveEnv is the memory, in MiB, admission control keeps free for the host.
const reserveEnv = "ANVIL_EPH_RESERVE_MIB"

const defaultReserveMiB = 1024

// admitPoll is how often a clone waiting for memory checks again.
const admitPoll = 250 * time.Millisecond

// restore hands out a pooled clone of spec's warm snapshot, or else restores clone id from
// it once the host has the memory, and reports how long it took. A pooled clone has its own id.
func (m *Manager) restore(ctx context.Context, e Engine, id string, spec WarmSpec, progress func(string)) (Clone, error) {
	start := time.Now()
	if c := m.takePooled(ctx, spec.Key); c != nil {
		progress("took a pooled clone in " + roundMS(time.Since(start)).String())
		return c, nil
	}
	return m.restoreFresh(ctx, e, id, spec, progress, true)
}

// restoreFresh restores clone id once the host has the memory for it; evict lets it delete
// pooled clones for that memory.
func (m *Manager) restoreFresh(ctx context.Context, e Engine, id string, spec WarmSpec, progress func(string), evict bool) (Clone, error) {
	release, err := m.admit(ctx, spec.MemoryMiB, evict, progress)
	if err != nil {
		return nil, err
	}
	defer release()
	progress("restoring a clone")
	start := time.Now()
	clone, err := e.RestoreClone(ctx, id, spec, m.Dir(spec.Key))
	if err != nil {
		return nil, err
	}
	msg := "restored in " + phaseSummary(time.Since(start), clone)
	log.Printf("ephemeral: %s %s", id, msg)
	progress(msg)
	return clone, nil
}

// phaseSummary is "312ms (network 40ms, disk 120ms, ...)", the phases when clone has them.
func phaseSummary(total time.Duration, clone Clone) string {
	s := roundMS(total).String()
	t, ok := clone.(Timed)
	if !ok {
		return s
	}
	var parts []string
	for _, p := range t.RestorePhases() {
		parts = append(parts, p.Name+" "+roundMS(p.Took).String())
	}
	if len(parts) == 0 {
		return s
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}

func roundMS(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return d.Round(time.Microsecond)
	}
	return d.Round(time.Millisecond)
}

// admit waits until the host's available memory covers a clone of mib plus the reserve,
// counting clones still restoring, whose memory doesn't show yet. release ends the count.
// With evict, pooled clones are deleted first: they are idle, a run isn't.
func (m *Manager) admit(ctx context.Context, mib int64, evict bool, progress func(string)) (release func(), err error) {
	reserve := reserveMiB()
	waited := false
	for {
		busy := m.running() > 0
		m.admitMu.Lock()
		avail, err := m.memAvailable()
		if err != nil { // can't tell on this host: no limit
			m.admitMu.Unlock()
			return func() {}, nil
		}
		if avail-m.restoring >= mib+reserve {
			m.restoring += mib
			m.admitMu.Unlock()
			return func() {
				m.admitMu.Lock()
				m.restoring -= mib
				m.admitMu.Unlock()
			}, nil
		}
		busy = busy || m.restoring > 0
		m.admitMu.Unlock()
		if evict && m.evictPooled() {
			time.Sleep(admitPoll) // for MemAvailable to show the freed memory
			continue
		}
		if !busy {
			return nil, fmt.Errorf("not enough memory for a %d MiB clone: %d MiB available, %d MiB kept for the host (%s)",
				mib, avail, reserve, reserveEnv)
		}
		if !waited {
			progress("waiting for memory")
			waited = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(admitPoll):
		}
	}
}

func (m *Manager) running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.clones)
}

// reserveMiB reads reserveEnv, falling back to the default on a missing or bad value.
func reserveMiB() int64 {
	if v, err := strconv.ParseInt(os.Getenv(reserveEnv), 10, 64); err == nil && v >= 0 {
		return v
	}
	return defaultReserveMiB
}

// hostMemAvailable is MemAvailable from /proc/meminfo, in MiB.
func hostMemAvailable() (int64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemAvailable(data)
}

func parseMemAvailable(meminfo []byte) (int64, error) {
	sc := bufio.NewScanner(bytes.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 && f[0] == "MemAvailable:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("meminfo: %w", err)
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("meminfo: no MemAvailable")
}
