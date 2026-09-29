package instance

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// ScheduledSnapshotPrefix starts the name of every snapshot the scheduler takes. Only
// those are pruned, so hand-made snapshots are never deleted by a schedule.
const ScheduledSnapshotPrefix = "auto-"

// MinSnapshotInterval is the shortest schedule interval allowed.
const MinSnapshotInterval = time.Minute

// schedulerTick is how often the scheduler checks for due snapshots.
const schedulerTick = 30 * time.Second

// SetSnapshotSchedule sets name's snapshot schedule; every 0 removes it.
func (m *Manager) SetSnapshotSchedule(name string, every time.Duration, keep int) error {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return err
	}
	if spec.VM == nil {
		return fmt.Errorf("instance: snapshot schedules only apply to VMs")
	}
	if _, ok := m.backends[spec.Kind].(Snapshotter); !ok {
		return fmt.Errorf("instance: %s instances don't support snapshots", spec.Kind)
	}
	switch {
	case every == 0:
		spec.VM.SnapshotSchedule = nil
	case every < MinSnapshotInterval:
		return fmt.Errorf("instance: snapshot interval must be at least %s", MinSnapshotInterval)
	case keep < 1:
		return fmt.Errorf("instance: keep must be at least 1")
	default:
		var last time.Time
		if spec.VM.SnapshotSchedule != nil {
			last = spec.VM.SnapshotSchedule.LastRun
		}
		spec.VM.SnapshotSchedule = &SnapshotSchedule{Every: every, Keep: keep, LastRun: last}
	}
	return m.registry.PutInstance(spec)
}

// RunSnapshotScheduler takes due scheduled snapshots until ctx ends.
func (m *Manager) RunSnapshotScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerTick)
	defer ticker.Stop()
	for {
		m.runDueSnapshots(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runDueSnapshots snapshots every running VM whose schedule is due at now. A stopped
// VM's disk doesn't change, so it's skipped until it runs again.
func (m *Manager) runDueSnapshots(ctx context.Context, now time.Time) {
	specs, err := m.registry.List(KindVM)
	if err != nil {
		log.Printf("instance: listing VMs for scheduled snapshots: %v", err)
		return
	}
	for _, spec := range specs {
		sched := spec.VM.SnapshotSchedule
		if sched == nil || spec.State != StateRunning || now.Sub(sched.LastRun) < sched.Every {
			continue
		}
		if err := m.takeScheduledSnapshot(ctx, spec, now); err != nil {
			log.Printf("instance: scheduled snapshot of %s: %v", spec.Name, err)
		}
		// Recorded even after a failure, so a broken VM is retried next interval, not every tick.
		m.markSnapshotRun(spec.ID, now)
	}
}

func (m *Manager) takeScheduledSnapshot(ctx context.Context, spec *Spec, now time.Time) error {
	sn, ok := m.backends[spec.Kind].(Snapshotter)
	if !ok {
		return fmt.Errorf("instance: %s instances don't support snapshots", spec.Kind)
	}
	name := ScheduledSnapshotPrefix + now.UTC().Format("20060102-150405")
	if err := sn.CreateSnapshot(ctx, spec, name); err != nil {
		return err
	}
	snaps, err := sn.ListSnapshots(ctx, spec)
	if err != nil {
		return fmt.Errorf("listing snapshots to prune: %w", err)
	}
	for _, old := range snapshotsToPrune(snaps, spec.VM.SnapshotSchedule.Keep) {
		if err := sn.DeleteSnapshot(ctx, spec, old); err != nil {
			return fmt.Errorf("pruning %s: %w", old, err)
		}
	}
	return nil
}

// snapshotsToPrune returns the scheduled snapshots beyond the newest keep, oldest first.
func snapshotsToPrune(snaps []Snapshot, keep int) []string {
	var auto []Snapshot
	for _, s := range snaps {
		if strings.HasPrefix(s.Name, ScheduledSnapshotPrefix) {
			auto = append(auto, s)
		}
	}
	sort.Slice(auto, func(i, j int) bool { return auto[i].CreatedAt.Before(auto[j].CreatedAt) })
	var out []string
	for i := 0; i < len(auto)-keep; i++ {
		out = append(out, auto[i].Name)
	}
	return out
}

// markSnapshotRun records a schedule run on a fresh copy of the record, so settings
// changed while the snapshot ran aren't overwritten.
func (m *Manager) markSnapshotRun(id string, at time.Time) {
	spec, err := m.registry.GetByID(id)
	if err != nil || spec.VM == nil || spec.VM.SnapshotSchedule == nil {
		return
	}
	spec.VM.SnapshotSchedule.LastRun = at
	if err := m.registry.PutInstance(spec); err != nil {
		log.Printf("instance: recording snapshot run for %s: %v", spec.Name, err)
	}
}
