package tui

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRecvStreamChainsUntilEOF(t *testing.T) {
	evs := []string{"one", "two"}
	recv := func() (string, error) {
		if len(evs) == 0 {
			return "", io.EOF
		}
		ev := evs[0]
		evs = evs[1:]
		return ev, nil
	}
	cmd := recvStream(recv, func(ev string) lineStep { return lineStep{status: ev} },
		lineTaskStep(1))

	var got []string
	for cmd != nil {
		msg := cmd().(taskStepMsg)
		if msg.status != "" {
			got = append(got, msg.status)
		}
		if msg.done {
			if msg.next != nil || msg.err != nil {
				t.Fatalf("a clean end must have no next and no error: %+v", msg)
			}
			break
		}
		cmd = msg.next
	}
	if strings.Join(got, ",") != "one,two" {
		t.Errorf("got %v", got)
	}
}

func TestRecvStreamError(t *testing.T) {
	cmd := recvStream(func() (string, error) { return "", errors.New("broken") },
		func(string) lineStep { return lineStep{} },
		lineTaskStep(1))
	msg := cmd().(taskStepMsg)
	if !msg.done || msg.err == nil || msg.next != nil {
		t.Errorf("want a failed, finished step, got %+v", msg)
	}
}

func TestTaskStepsAdd(t *testing.T) {
	var st taskSteps
	st.start()
	st.add("copying: 10%")
	st.add("copying: 60%")
	if len(st.lines) != 1 || !strings.Contains(progressView(st.lines, st.running, st.since), "] 60%") {
		t.Errorf("updates with one key should redraw one barred line, got %q", st.lines)
	}
}
