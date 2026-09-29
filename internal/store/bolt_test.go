package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
)

func TestInstancePoliciesRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	last := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	in := &instance.Spec{
		ID: "01", Name: "web", Kind: instance.KindVM, State: instance.StateStopped,
		Autostart: true, UserStopped: true,
		RestartPolicy: instance.RestartPolicy{Mode: instance.RestartOnFailure, MaxRetries: 3},
		VM:            &instance.VMSpec{SnapshotSchedule: &instance.SnapshotSchedule{Every: 6 * time.Hour, Keep: 8, LastRun: last}},
	}
	if err := s.PutInstance(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.GetByID("01")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Autostart || !out.UserStopped || out.RestartPolicy != in.RestartPolicy {
		t.Errorf("policies not persisted: %+v", out)
	}
	if sc := out.VM.SnapshotSchedule; sc == nil || sc.Every != 6*time.Hour || sc.Keep != 8 || !sc.LastRun.Equal(last) {
		t.Errorf("schedule not persisted: %+v", out.VM.SnapshotSchedule)
	}
}
