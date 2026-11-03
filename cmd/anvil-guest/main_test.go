//go:build linux

package main

import (
	"fmt"
	"syscall"
	"testing"
)

func TestTemporary(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.EMFILE, syscall.ECONNABORTED} {
		if !temporary(fmt.Errorf("accept: %w", e)) {
			t.Errorf("%v isn't retried", e)
		}
	}
	for _, e := range []syscall.Errno{syscall.EBADF, syscall.EINVAL} {
		if temporary(e) {
			t.Errorf("%v is retried", e)
		}
	}
}
