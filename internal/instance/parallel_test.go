package instance

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestForEachParallelRunsAllAndJoinsErrors(t *testing.T) {
	specs := make([]*Spec, 10)
	for i := range specs {
		specs[i] = &Spec{Name: fmt.Sprintf("i%d", i)}
	}
	var ran, inFlight, peak atomic.Int32
	err := forEachParallel(specs, func(s *Spec) error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		ran.Add(1)
		if s.Name == "i3" || s.Name == "i7" {
			return errors.New("boom " + s.Name)
		}
		return nil
	})
	if ran.Load() != 10 {
		t.Errorf("expected all 10 specs to run, got %d", ran.Load())
	}
	if peak.Load() > maxParallelOps {
		t.Errorf("expected at most %d in flight, got %d", maxParallelOps, peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("expected specs to run in parallel, peak was %d", peak.Load())
	}
	if err == nil {
		t.Fatal("expected a joined error")
	}
	for _, want := range []string{"boom i3", "boom i7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %q", want, err.Error())
		}
	}
}

func TestForEachParallelEmpty(t *testing.T) {
	if err := forEachParallel(nil, func(*Spec) error { return errors.New("never") }); err != nil {
		t.Errorf("expected nil for no specs, got %v", err)
	}
}
