package tui

import (
	"errors"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// streamStep is one event off a server stream, turned into what a screen shows.
type streamStep[R any] struct {
	next   tea.Cmd // receives the following event, nil once the stream ended
	status string  // a progress or result line, "" for none
	result R       // the value some events carry, e.g. the launched instance
	err    error
	done   bool
}

// instanceStep is the step of a stream that ends with an instance (launch, fork, wait).
type instanceStep = streamStep[*anvilv1.Instance]

// failed is the step that ends a stream with err.
func failed[R any](err error) streamStep[R] {
	return streamStep[R]{err: err, done: true}
}

// recvStream receives one event from recv: parse turns it into a step and wrap into the
// screen's own message. io.EOF ends the stream cleanly.
func recvStream[E, R any](recv func() (E, error), parse func(E) streamStep[R], wrap func(streamStep[R]) tea.Msg) tea.Cmd {
	var next tea.Cmd
	next = func() tea.Msg {
		ev, err := recv()
		if errors.Is(err, io.EOF) {
			return wrap(streamStep[R]{done: true})
		}
		if err != nil {
			return wrap(failed[R](err))
		}
		s := parse(ev)
		if !s.done {
			s.next = next
		}
		return wrap(s)
	}
	return next
}

// transcript is a streamed operation's progress, shown in place of its screen until dismissed.
type transcript struct {
	running bool
	lines   []string
	since   time.Time // when the last line's step began
}

func (t *transcript) start() { *t = transcript{running: true, since: time.Now()} }

// active reports whether the transcript is on screen.
func (t transcript) active() bool { return t.running || len(t.lines) > 0 }

func (t *transcript) add(status string) {
	t.since = stepStart(t.lines, status, t.since)
	t.lines = appendProgressLine(t.lines, status)
}

func (t transcript) view(title string) string {
	s := styleTitle.Render(" "+title+" ") + "\n\n" + progressView(t.lines, t.running, t.since)
	if !t.running {
		s += "\n" + helpBar("esc", "dismiss")
	}
	return s
}

// dismiss clears a finished transcript on esc, enter or q. It reports whether t was showing.
func (t *transcript) dismiss(key string) bool {
	if !t.active() {
		return false
	}
	switch key {
	case "esc", "enter", "q":
		t.lines = nil
	}
	return true
}

// follow applies s to t and returns the Cmd for the next event; onDone runs once the
// stream ends without an error.
func follow[R any](t *transcript, s streamStep[R], onDone tea.Cmd) tea.Cmd {
	if s.status != "" {
		t.add(s.status)
	}
	switch {
	case s.err != nil:
		t.running = false
		t.lines = append(t.lines, errLine(s.err))
		return nil
	case s.done:
		t.running = false
		return onDone
	}
	return s.next
}

// parseLaunchProgress maps the LaunchProgress events shared by Launch, Fork and WaitReady.
func parseLaunchProgress(ev *anvilv1.LaunchProgress) instanceStep {
	switch e := ev.GetEvent().(type) {
	case *anvilv1.LaunchProgress_Status:
		return instanceStep{status: e.Status}
	case *anvilv1.LaunchProgress_Error:
		return failed[*anvilv1.Instance](errors.New(e.Error))
	case *anvilv1.LaunchProgress_Instance:
		return instanceStep{result: e.Instance, done: true}
	}
	return instanceStep{}
}

// lineStep is the step of a stream that only reports lines (export, import, migrate).
type lineStep = streamStep[struct{}]
