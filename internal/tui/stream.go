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
	name   string  // the instance the stream is about, once the daemon named it
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

// taskSteps is a streamed task's progress lines.
type taskSteps struct {
	running bool
	lines   []string
	since   time.Time // when the last line's step began
}

func (t *taskSteps) start() { *t = taskSteps{running: true, since: time.Now()} }

func (t *taskSteps) add(status string) {
	t.since = stepStart(t.lines, status, t.since)
	t.lines = appendProgressLine(t.lines, status)
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
	case *anvilv1.LaunchProgress_Name:
		return instanceStep{name: e.Name}
	}
	return instanceStep{}
}

// lineStep is the step of a stream that only reports lines (export, import, migrate).
type lineStep = streamStep[struct{}]
