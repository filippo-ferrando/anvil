package backoff

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	b := New(time.Millisecond, 10*time.Millisecond)
	var got []time.Duration
	for range 6 {
		got = append(got, b.Next())
	}
	want := []time.Duration{1, 2, 4, 8, 10, 10}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Fatalf("waits %v, want %v ms", got, want)
		}
	}
	if d := New(time.Second, time.Millisecond).Next(); d != time.Millisecond {
		t.Errorf("first above max = %s", d)
	}
}
