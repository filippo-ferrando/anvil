package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
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
		Autostart: true, UserStopped: true, GUI: true,
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
	if !out.Autostart || !out.UserStopped || !out.GUI || out.RestartPolicy != in.RestartPolicy {
		t.Errorf("policies not persisted: %+v", out)
	}
	if sc := out.VM.SnapshotSchedule; sc == nil || sc.Every != 6*time.Hour || sc.Keep != 8 || !sc.LastRun.Equal(last) {
		t.Errorf("schedule not persisted: %+v", out.VM.SnapshotSchedule)
	}
}

func TestPutInstanceRefusesTakenName(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutInstance(&instance.Spec{ID: "01", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInstance(&instance.Spec{ID: "02", Name: "web"}); err == nil {
		t.Fatal("expected a second instance named web to be refused")
	}
	if got, err := s.GetByName("web"); err != nil || got.ID != "01" {
		t.Errorf("name index moved: %+v, %v", got, err)
	}
}

func TestUpdateInstance(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutInstance(&instance.Spec{ID: "01", Name: "web", State: instance.StateStopped, Autostart: true}); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateInstance("01", func(sp *instance.Spec) error { sp.State = instance.StateRunning; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByID("01"); got.State != instance.StateRunning || !got.Autostart {
		t.Errorf("expected only State to change, got %+v", got)
	}
	if err := s.UpdateInstance("01", func(sp *instance.Spec) error { sp.Name = "other"; return nil }); err == nil {
		t.Error("expected a rename through UpdateInstance to be refused")
	}
	if err := s.DeleteByID("01"); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateInstance("01", func(*instance.Spec) error { return nil })
	if !errors.Is(err, instance.ErrNotFound) {
		t.Errorf("expected ErrNotFound for a deleted record, got %v", err)
	}
	if _, err := s.GetByID("01"); err == nil {
		t.Error("UpdateInstance recreated a deleted record")
	}
}
