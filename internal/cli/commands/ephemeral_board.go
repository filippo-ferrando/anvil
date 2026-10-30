package commands

import (
	"fmt"
	"io"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// statusBoard keeps one live status line per clone at the bottom of a terminal, with a
// spinner and the step's time, while output scrolls above it.
type statusBoard struct {
	out   io.Writer // the terminal the board is drawn on
	width int       // 0 when unknown

	mu      sync.Mutex
	order   []string
	steps   map[string]*boardStep
	drawn   int // board lines on screen now
	frame   int
	stopped bool
}

type boardStep struct {
	status string
	since  time.Time
}

func newStatusBoard(out io.Writer, order []string) *statusBoard {
	return &statusBoard{out: out, width: terminalWidth(out), order: order, steps: map[string]*boardStep{}}
}

// start redraws the board on every spinner tick until the returned func is called.
func (b *statusBoard) start() (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(spinnerInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				b.mu.Lock()
				b.frame++
				b.redraw()
				b.mu.Unlock()
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// set makes status distro's current step. A finished step that took a while stays above
// the board through above, in its final form.
func (b *statusBoard) set(distro, status string, above func(distro, line string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.clear()
	old := b.steps[distro]
	switch {
	case old == nil:
		b.steps[distro] = &boardStep{status: status, since: time.Now()}
	case progress.Key(old.status) != progress.Key(status):
		if elapsed := time.Since(old.since); elapsed >= time.Second {
			above(distro, progress.Done(old.status, elapsed))
		}
		b.steps[distro] = &boardStep{status: status, since: time.Now()}
	default:
		old.status = status
	}
	b.draw()
}

// done takes distro off the board.
func (b *statusBoard) done(distro string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clear()
	delete(b.steps, distro)
	b.draw()
}

// print runs write with the board off screen, then puts it back below what write printed.
func (b *statusBoard) print(write func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clear()
	write()
	b.draw()
}

// stop takes the board off screen for good.
func (b *statusBoard) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clear()
	b.stopped = true
}

func (b *statusBoard) redraw() {
	b.clear()
	b.draw()
}

// clear moves up over the drawn board and erases it.
func (b *statusBoard) clear() {
	if b.drawn > 0 {
		fmt.Fprintf(b.out, "\x1b[%dA\r\x1b[J", b.drawn)
		b.drawn = 0
	}
}

func (b *statusBoard) draw() {
	if b.stopped {
		return
	}
	width := 0
	for _, d := range b.order {
		width = max(width, len(d))
	}
	for _, d := range b.order {
		st := b.steps[d]
		if st == nil {
			continue
		}
		line := fmt.Sprintf("[%-*s] %s", width, d, progress.Live(st.status, time.Since(st.since), b.frame))
		// A wrapped line would break the count of lines to move up over.
		if r := []rune(line); b.width > 1 && len(r) >= b.width {
			line = string(r[:b.width-2]) + "…"
		}
		fmt.Fprintln(b.out, "\x1b[2m"+line+"\x1b[0m")
		b.drawn++
	}
}
