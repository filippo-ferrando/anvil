// Package backoff spaces out the tries of a poll: short at first, for what is usually
// ready almost at once, then longer, so a long wait doesn't cost many more tries.
package backoff

import "time"

// Backoff hands out the waits between tries, doubling from first up to max.
type Backoff struct {
	next, max time.Duration
}

func New(first, max time.Duration) *Backoff {
	return &Backoff{next: min(first, max), max: max}
}

// Next is the wait before the next try.
func (b *Backoff) Next() time.Duration {
	d := b.next
	b.next = min(b.next*2, b.max)
	return d
}
