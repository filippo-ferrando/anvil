package instance

import (
	"testing"
	"time"
)

func TestRestartTrackerForget(t *testing.T) {
	var tr restartTracker
	tr.started("a")
	tr.started("b")
	tr.mu.Lock()
	tr.attempts["a"] = 2
	tr.timers["a"] = time.AfterFunc(time.Hour, func() {})
	tr.mu.Unlock()

	tr.forget("a")

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if _, ok := tr.lastStart["a"]; ok {
		t.Error("lastStart still holds the forgotten instance")
	}
	if _, ok := tr.attempts["a"]; ok {
		t.Error("attempts still holds the forgotten instance")
	}
	if _, ok := tr.timers["a"]; ok {
		t.Error("timers still holds the forgotten instance")
	}
	if _, ok := tr.lastStart["b"]; !ok {
		t.Error("forget dropped another instance")
	}
}
