//go:build linux

package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestFollowFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console.log")
	w, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var mu sync.Mutex
	var got bytes.Buffer
	received := func() int {
		mu.Lock()
		defer mu.Unlock()
		return got.Len()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- followFile(ctx, f, path, func(b []byte) error {
			mu.Lock()
			defer mu.Unlock()
			got.Write(b)
			return nil
		})
	}()
	waitFor := func(n int, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for received() < n {
			if time.Now().After(deadline) {
				t.Fatalf("got %d of %d bytes after %s", received(), n, within)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// A burst of boot output comes through at once, not 4 KiB per tick.
	burst := bytes.Repeat([]byte("[    1.234] some boot message from the guest kernel\n"), 4000)
	if _, err := w.Write(burst); err != nil {
		t.Fatal(err)
	}
	waitFor(len(burst), time.Second)
	// A single line shows up well before the polling fallback's interval.
	start := time.Now()
	if _, err := w.Write([]byte("login: \n")); err != nil {
		t.Fatal(err)
	}
	waitFor(len(burst)+8, time.Second)
	if took := time.Since(start); took > followPoll/2 {
		t.Errorf("a new line took %s to arrive", took)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("followFile = %v after cancel", err)
	}
}
