package commands

import (
	"strings"
	"testing"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func TestSetNeedsAFlag(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"set", "web"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("expected a nothing-to-change error, got %v", err)
	}
}

func TestSnapshotScheduleNeedsEveryOrOff(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"snapshot", "schedule", "web"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--every is required") {
		t.Fatalf("expected an --every error, got %v", err)
	}
}

func TestPolicyLabels(t *testing.T) {
	if got := restartPolicyLabel(nil); got != "no" {
		t.Errorf("nil policy: got %q", got)
	}
	if got := restartPolicyLabel(&anvilv1.RestartPolicy{Mode: "on-failure", MaxRetries: 3}); got != "on-failure:3" {
		t.Errorf("got %q", got)
	}
	got := scheduleLabel(&anvilv1.SnapshotSchedule{EverySeconds: 6 * 3600, Keep: 8})
	if got != "every 6h0m0s, keep 8" {
		t.Errorf("got %q", got)
	}
}
