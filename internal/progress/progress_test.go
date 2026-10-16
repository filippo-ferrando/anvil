package progress

import (
	"strings"
	"testing"
	"time"
)

func TestBar(t *testing.T) {
	got := Bar("pulling nginx:alpine: 45% (12.3 MiB / 27.0 MiB), 3/7 layers")
	if !strings.HasPrefix(got, "pulling nginx:alpine: [█████████░░░░░░░░░░░] 45%") {
		t.Errorf("Bar = %q", got)
	}
	if got := Bar("waiting for cloud-init: running"); got != "waiting for cloud-init: running" {
		t.Errorf("no percentage should mean no bar, got %q", got)
	}
	if got := Bar("done: 100%"); !strings.Contains(got, "["+strings.Repeat("█", barWidth)+"]") {
		t.Errorf("100%% should fill the bar, got %q", got)
	}
}

func TestLiveAndDone(t *testing.T) {
	if got := Live("starting", 500*time.Millisecond, 0); got != "starting" {
		t.Errorf("a fresh step needs no spinner, got %q", got)
	}
	if got := Live("waiting for cloud-init: running", 42*time.Second, 1); got != "waiting for cloud-init: running  ⠙ 42s" {
		t.Errorf("Live = %q", got)
	}
	if got := Done("waiting for cloud-init: running", 42*time.Second); got != "waiting for cloud-init: running  (42s)" {
		t.Errorf("Done = %q", got)
	}
	if Key("pulling redis:7: 10%") != "pulling redis:7" || Key("starting") != "starting" {
		t.Error("Key should cut at the first \": \"")
	}
}
