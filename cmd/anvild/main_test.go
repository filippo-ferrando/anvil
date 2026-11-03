package main

import (
	"testing"
	"time"
)

func TestWaitTimeout(t *testing.T) {
	if !waitTimeout(func() {}, time.Second) {
		t.Error("a function that returns at once timed out")
	}
	block := make(chan struct{})
	defer close(block)
	if waitTimeout(func() { <-block }, 10*time.Millisecond) {
		t.Error("a blocked function reported done")
	}
}
