package commands

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// launchProgressStream is the subset of InstanceService_LaunchClient that
// streamLaunchProgress needs.
type launchProgressStream interface {
	Recv() (*anvilv1.LaunchProgress, error)
}

// streamLaunchProgress prints each LaunchProgress event as it arrives, see streamProgress.
func streamLaunchProgress(out io.Writer, stream launchProgressStream) error {
	return streamProgress(out, stream, "Launched")
}

// spinnerInterval is how often the current line is redrawn on a terminal.
const spinnerInterval = 100 * time.Millisecond

// streamProgress prints a launch-like stream, ending with "<verb> <name>".
func streamProgress(out io.Writer, stream launchProgressStream, verb string) error {
	return streamStatus(out, stream.Recv, func(l *liveLine, ev *anvilv1.LaunchProgress) (bool, error) {
		return false, printLaunchEvent(l, ev, verb)
	})
}

// printLaunchEvent prints one launch event, an error event becomes the returned error.
func printLaunchEvent(l *liveLine, ev *anvilv1.LaunchProgress, verb string) error {
	switch e := ev.GetEvent().(type) {
	case *anvilv1.LaunchProgress_Status:
		l.set(e.Status)
	case *anvilv1.LaunchProgress_Error:
		return errors.New(e.Error)
	case *anvilv1.LaunchProgress_Instance:
		l.printf("%s %q (%s)\n", verb, e.Instance.GetName(), e.Instance.GetId())
		if ips := e.Instance.GetGuest().GetIpAddresses(); len(ips) > 0 {
			l.printf("IP: %s\n", strings.Join(ips, ", "))
		}
	}
	return nil
}

// streamAddMembers prints IntentService.AddMembers progress like one launch per member.
func streamAddMembers(out io.Writer, stream interface {
	Recv() (*anvilv1.IntentAddMembersProgress, error)
}) error {
	return streamStatus(out, stream.Recv, func(l *liveLine, ev *anvilv1.IntentAddMembersProgress) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.IntentAddMembersProgress_Member:
			return false, printLaunchEvent(l, e.Member.GetLaunch(), "Launched")
		case *anvilv1.IntentAddMembersProgress_Error:
			return false, errors.New(e.Error)
		}
		return false, nil
	})
}

// streamStatus prints a server stream until it ends, handle returns stop or an error. On a
// terminal, statuses sharing a key redraw one line, with a bar and a spinner on long steps.
func streamStatus[E any](out io.Writer, recv func() (E, error), handle func(l *liveLine, ev E) (stop bool, err error)) error {
	type recvResult struct {
		ev  E
		err error
	}
	events, done := make(chan recvResult), make(chan struct{})
	defer close(done)
	go func() {
		for {
			ev, err := recv()
			select {
			case events <- recvResult{ev, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	line := &liveLine{out: out, term: isTerminalWriter(out), width: terminalWidth(out)}
	defer line.finish()
	var tick <-chan time.Time
	if line.term {
		t := time.NewTicker(spinnerInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-tick:
			line.redraw()
		case r := <-events:
			if r.err == io.EOF {
				return nil
			}
			if r.err != nil {
				return r.err
			}
			if stop, err := handle(line, r.ev); stop || err != nil {
				return err
			}
		}
	}
}

// liveLine is the status line still being updated. Off a terminal, every status is
// simply printed on its own line.
type liveLine struct {
	out   io.Writer
	term  bool
	width int // 0 when unknown

	status, key string
	since       time.Time // when this key's step started
	frame       int
	shown       bool
}

func (l *liveLine) set(status string) {
	if !l.term {
		fmt.Fprintln(l.out, status)
		return
	}
	key := progress.Key(status)
	if l.shown && key != l.key {
		l.finish()
	}
	if !l.shown {
		l.since = time.Now()
	}
	l.status, l.key, l.shown = status, key, true
	l.redraw()
}

func (l *liveLine) redraw() {
	if !l.shown {
		return
	}
	l.frame++
	fmt.Fprint(l.out, "\r\033[K"+l.fit(progress.Live(l.status, time.Since(l.since), l.frame)))
}

// finish leaves the current step on screen in its final form and moves to a new line.
func (l *liveLine) finish() {
	if !l.shown {
		return
	}
	fmt.Fprint(l.out, "\r\033[K"+l.fit(progress.Done(l.status, time.Since(l.since)))+"\n")
	l.shown = false
}

// printf ends the current status line, then prints below it.
func (l *liveLine) printf(format string, args ...any) {
	l.finish()
	fmt.Fprintf(l.out, format, args...)
}

// fit cuts s to the terminal width: a wrapped line can't be redrawn in place.
func (l *liveLine) fit(s string) string {
	if r := []rune(s); l.width > 1 && len(r) >= l.width {
		return string(r[:l.width-2]) + "…"
	}
	return s
}

// isTerminalWriter reports whether w is a real terminal rather than a pipe/file redirect.
func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	width, _, err := term.GetSize(f.Fd())
	if err != nil {
		return 0
	}
	return width
}
