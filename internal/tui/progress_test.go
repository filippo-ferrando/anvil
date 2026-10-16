package tui

import (
	"strings"
	"testing"
	"time"
)

func TestProgressViewAnimatesOnlyTheRunningStep(t *testing.T) {
	lines := []string{"pulling redis:7: done, 7 layers, 41.5 MiB", "waiting for cloud-init: running"}
	since := time.Now().Add(-30 * time.Second)

	got := progressView(lines, true, since)
	if !strings.Contains(got, "waiting for cloud-init: running  ") || !strings.Contains(got, "30s") {
		t.Errorf("the running step should show its time, got %q", got)
	}
	if strings.Contains(strings.Split(got, "\n")[0], "s  ") {
		t.Errorf("a finished step should not animate, got %q", got)
	}
	if got := progressView(lines, false, since); strings.Contains(got, "30s") {
		t.Errorf("nothing animates once the stream ended, got %q", got)
	}
}

func TestStepStartKeepsTimeWithinOneStep(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	lines := []string{"pulling redis:7: 10% (1.0 MiB / 10.0 MiB), 0/2 layers"}
	if got := stepStart(lines, "pulling redis:7: 50% (5.0 MiB / 10.0 MiB), 1/2 layers", since); !got.Equal(since) {
		t.Error("an update of the same step should keep its start time")
	}
	if got := stepStart(lines, "starting", since); time.Since(got) > time.Second {
		t.Error("a new step should start now")
	}
}
