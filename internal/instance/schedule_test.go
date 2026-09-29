package instance

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSetSnapshotScheduleValidates(t *testing.T) {
	m, _, reg := newPolicyManager(t,
		&Spec{ID: "01", Name: "web", Kind: KindVM, VM: &VMSpec{}},
		&Spec{ID: "02", Name: "nginx", Kind: KindContainer, Container: &ContainerSpec{}},
	)
	if err := m.SetSnapshotSchedule("web", 30*time.Second, 3); err == nil {
		t.Error("expected an interval under a minute to be refused")
	}
	if err := m.SetSnapshotSchedule("web", time.Hour, 0); err == nil {
		t.Error("expected keep 0 to be refused")
	}
	if err := m.SetSnapshotSchedule("nginx", time.Hour, 3); err == nil {
		t.Error("expected containers to be refused")
	}
	if err := m.SetSnapshotSchedule("web", 6*time.Hour, 8); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.GetByID("01"); s.VM.SnapshotSchedule == nil || s.VM.SnapshotSchedule.Every != 6*time.Hour || s.VM.SnapshotSchedule.Keep != 8 {
		t.Errorf("unexpected schedule %+v", s.VM.SnapshotSchedule)
	}
	if err := m.SetSnapshotSchedule("web", 0, 0); err != nil {
		t.Fatal(err)
	}
	if s, _ := reg.GetByID("01"); s.VM.SnapshotSchedule != nil {
		t.Error("expected every 0 to remove the schedule")
	}
}

func TestRunDueSnapshotsTakesAndPrunes(t *testing.T) {
	m, rb, reg := newPolicyManager(t,
		&Spec{ID: "01", Name: "web", Kind: KindVM, State: StateRunning, VM: &VMSpec{SnapshotSchedule: &SnapshotSchedule{Every: time.Hour, Keep: 2}}},
		&Spec{ID: "02", Name: "off", Kind: KindVM, State: StateStopped, VM: &VMSpec{SnapshotSchedule: &SnapshotSchedule{Every: time.Hour, Keep: 2}}},
	)
	rb.snaps = map[string][]Snapshot{"web": {{Name: "before-upgrade", CreatedAt: time.Now().Add(-48 * time.Hour)}}}

	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	for i := range 4 {
		now := start.Add(time.Duration(i) * time.Hour)
		m.runDueSnapshots(context.Background(), now)
		m.runDueSnapshots(context.Background(), now.Add(time.Minute)) // not due yet: no extra snapshot
		// The fake stamps CreatedAt with the wall clock; spread them out for pruning order.
		snaps := rb.snaps["web"]
		snaps[len(snaps)-1].CreatedAt = now
	}

	var auto []string
	manual := false
	for _, s := range rb.snaps["web"] {
		if strings.HasPrefix(s.Name, ScheduledSnapshotPrefix) {
			auto = append(auto, s.Name)
		}
		if s.Name == "before-upgrade" {
			manual = true
		}
	}
	want := []string{"auto-20260929-120000", "auto-20260929-130000"}
	if len(auto) != 2 || auto[0] != want[0] || auto[1] != want[1] {
		t.Errorf("expected the newest two scheduled snapshots %v, got %v", want, auto)
	}
	if !manual {
		t.Error("expected a hand-made snapshot never to be pruned")
	}
	if len(rb.snaps["off"]) != 0 {
		t.Error("expected a stopped VM to be skipped")
	}
	if s, _ := reg.GetByID("01"); !s.VM.SnapshotSchedule.LastRun.Equal(start.Add(3 * time.Hour)) {
		t.Errorf("expected LastRun recorded, got %s", s.VM.SnapshotSchedule.LastRun)
	}
}
