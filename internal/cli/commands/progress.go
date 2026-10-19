package commands

import (
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

// hypervisorCheck fails a launch asking for want when the launched instance reports another
// hypervisor: an older anvild drops the field and runs QEMU.
type hypervisorCheck struct {
	launchProgressStream
	want anvilv1.VmHypervisor
}

func (s hypervisorCheck) Recv() (*anvilv1.LaunchProgress, error) {
	ev, err := s.launchProgressStream.Recv()
	if inst := ev.GetInstance(); inst != nil && s.want == anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER &&
		inst.GetVm().GetHypervisor() != s.want {
		return nil, fmt.Errorf("anvild launched %s with QEMU: it predates firecracker support; upgrade it, then `anvil delete %s` and launch again",
			inst.GetName(), inst.GetName())
	}
	return ev, err
}

// streamLaunchProgress prints each LaunchProgress event as it arrives, see streamProgress.
func streamLaunchProgress(out io.Writer, stream launchProgressStream) error {
	return streamProgress(out, stream, "Launched")
}

// spinnerInterval is how often the current line is redrawn on a terminal.
const spinnerInterval = 100 * time.Millisecond

// streamProgress prints a launch-like stream, ending with "<verb> <name>". On a terminal,
// updates sharing a key redraw one line, with a bar and, during a long step, a spinner.
func streamProgress(out io.Writer, stream launchProgressStream, verb string) error {
	type recvResult struct {
		ev  *anvilv1.LaunchProgress
		err error
	}
	events, done := make(chan recvResult), make(chan struct{})
	defer close(done)
	go func() {
		for {
			ev, err := stream.Recv()
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
				line.finish()
				return nil
			}
			if r.err != nil {
				line.finish()
				return r.err
			}
			switch e := r.ev.GetEvent().(type) {
			case *anvilv1.LaunchProgress_Status:
				line.set(e.Status)
			case *anvilv1.LaunchProgress_Error:
				line.finish()
				return fmt.Errorf("%s", e.Error)
			case *anvilv1.LaunchProgress_Instance:
				line.finish()
				fmt.Fprintf(out, "%s %q (%s)\n", verb, e.Instance.GetName(), e.Instance.GetId())
				if ips := e.Instance.GetGuest().GetIpAddresses(); len(ips) > 0 {
					fmt.Fprintf(out, "IP: %s\n", strings.Join(ips, ", "))
				}
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
