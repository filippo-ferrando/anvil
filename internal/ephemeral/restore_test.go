//go:build linux

package ephemeral

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseMemAvailable(t *testing.T) {
	got, err := parseMemAvailable([]byte("MemTotal:       16000000 kB\nMemFree:  100 kB\nMemAvailable:    2097152 kB\n"))
	if err != nil || got != 2048 {
		t.Errorf("got %d, %v; want 2048", got, err)
	}
	if _, err := parseMemAvailable([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Error("meminfo without MemAvailable was accepted")
	}
}

type timedClone struct{ fakeClone }

func (timedClone) RestorePhases() []Phase {
	return []Phase{{"network", 40 * time.Millisecond}, {"disk", 1500 * time.Microsecond}}
}

func TestPhaseSummary(t *testing.T) {
	if got := phaseSummary(312400*time.Microsecond, &fakeClone{}); got != "312ms" {
		t.Errorf("untimed clone: %q", got)
	}
	if got := phaseSummary(312*time.Millisecond, &timedClone{}); got != "312ms (network 40ms, disk 2ms)" {
		t.Errorf("timed clone: %q", got)
	}
}

func TestAdmit(t *testing.T) {
	t.Setenv(reserveEnv, "100")
	m := &Manager{clones: map[string]*cloneState{}}
	avail := int64(700)
	m.memAvailable = func() (int64, error) { return avail, nil }
	ctx := context.Background()

	// 700 MiB covers one 512 MiB clone plus the reserve, not a second one while it restores.
	release, err := m.admit(ctx, 512, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	admitted := make(chan error, 1)
	go func() {
		rel, err := m.admit(ctx, 512, false, func(s string) { steps = append(steps, s) })
		if err == nil {
			rel()
		}
		admitted <- err
	}()
	select {
	case err := <-admitted:
		t.Fatalf("second clone admitted while the first restores: %v", err)
	case <-time.After(3 * admitPoll):
	}
	release()
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0] != "waiting for memory" {
		t.Errorf("progress %v", steps)
	}

	// Nothing running and still too little: fail instead of waiting forever.
	avail = 300
	if _, err := m.admit(ctx, 512, false, func(string) {}); err == nil || !strings.Contains(err.Error(), reserveEnv) {
		t.Errorf("err = %v, want not enough memory", err)
	}

	// A host that can't tell admits.
	m.memAvailable = func() (int64, error) { return 0, errors.New("no meminfo") }
	if _, err := m.admit(ctx, 512, false, func(string) {}); err != nil {
		t.Error(err)
	}
}
