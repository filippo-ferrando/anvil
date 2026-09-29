package commands

import (
	"strings"
	"testing"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func TestFormatWatchEvent(t *testing.T) {
	at := time.Date(2026, 9, 29, 18, 4, 5, 0, time.UTC)
	updated := formatWatchEvent(at, &anvilv1.WatchEvent{
		Type:     anvilv1.WatchEventType_WATCH_EVENT_TYPE_UPDATED,
		Instance: &anvilv1.Instance{Name: "web", Kind: anvilv1.Kind_KIND_VM, State: anvilv1.State_STATE_RUNNING},
	})
	for _, want := range []string{"18:04:05", "web", "Running"} {
		if !strings.Contains(updated, want) {
			t.Errorf("expected %q in %q", want, updated)
		}
	}
	deleted := formatWatchEvent(at, &anvilv1.WatchEvent{
		Type:     anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED,
		Instance: &anvilv1.Instance{Name: "db"},
	})
	if !strings.Contains(deleted, "db") || !strings.Contains(deleted, "Removed") {
		t.Errorf("unexpected deleted line %q", deleted)
	}
}
