package store

import (
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func TestEventsFilterAndLimit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, name := range []string{"web", "db", "web", "web"} {
		if _, err := s.AppendEvent(instance.HistoryEvent{InstanceName: name, Action: "start"}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListEvents("", 0)
	if err != nil || len(all) != 4 || all[0].Seq != 1 || all[3].Seq != 4 {
		t.Fatalf("expected 4 events oldest first, got %+v, %v", all, err)
	}
	web, _ := s.ListEvents("web", 2)
	if len(web) != 2 || web[0].Seq != 3 || web[1].Seq != 4 {
		t.Errorf("expected web's newest 2 events, got %+v", web)
	}
}

func TestEventsTrimToMax(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.AppendEvent(instance.HistoryEvent{InstanceName: "old"}); err != nil {
		t.Fatal(err)
	}
	// Jump the sequence to the cap instead of appending MaxEvents events.
	if err := s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucketEvents).SetSequence(MaxEvents) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(instance.HistoryEvent{InstanceName: "new"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListEvents("", 0)
	if len(got) != 1 || got[0].InstanceName != "new" || got[0].Seq != MaxEvents+1 {
		t.Errorf("expected only the new event after the trim, got %+v", got)
	}
}

// The byte pre-check must not drop names JSON escapes, nor match a name inside another field.
func TestEventsFilterEscapedNames(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, ev := range []instance.HistoryEvent{
		{InstanceName: `we"b<1>`, Action: "start"},
		{InstanceName: "db", Message: `"InstanceName":"web"`},
		{InstanceName: "web-2"},
	} {
		if _, err := s.AppendEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.ListEvents(`we"b<1>`, 0); len(got) != 1 {
		t.Errorf("escaped name: got %+v", got)
	}
	if got, _ := s.ListEvents("web", 0); len(got) != 0 {
		t.Errorf("web: got %+v, want none", got)
	}
}
