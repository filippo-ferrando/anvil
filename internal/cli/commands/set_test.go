package commands

import (
	"strings"
	"testing"
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
