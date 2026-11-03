//go:build unix

package commands

import (
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestWatchResize(t *testing.T) {
	calls := make(chan struct{}, 8)
	var after atomic.Bool
	stop := watchResize(func() {
		if after.Load() {
			t.Error("resize called after stop")
		}
		calls <- struct{}{}
	})
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatal("no resize after SIGWINCH")
	}
	stop()
	stop() // a second call is a no-op
	after.Store(true)
	// SIGWINCH is ignored by default, so this is harmless once stopped.
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGWINCH)
	time.Sleep(50 * time.Millisecond)
}
