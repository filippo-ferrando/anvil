package tui

import (
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
		func(s lineStep) tea.Msg { return exportStreamMsg{s} })

	var got []string
	for cmd != nil {
		msg := cmd().(exportStreamMsg)
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
		func(s lineStep) tea.Msg { return importStreamMsg{s} })
	msg := cmd().(importStreamMsg)
	if !msg.done || msg.err == nil || msg.next != nil {
		t.Errorf("want a failed, finished step, got %+v", msg)
	}
}

func TestTranscriptFollow(t *testing.T) {
	var tr transcript
	tr.start()
	next := func() tea.Msg { return nil }
	if cmd := follow(&tr, lineStep{status: "copying: 10%", next: next}, nil); cmd == nil {
		t.Fatal("a running stream should chain its next receive")
	}
	follow(&tr, lineStep{status: "copying: 60%", next: next}, nil)
	if len(tr.lines) != 1 || !strings.Contains(tr.view("Exporting…"), "] 60%") {
		t.Errorf("updates with one key should redraw one barred line, got %q", tr.lines)
	}
	reload := func() tea.Msg { return nil }
	if cmd := follow(&tr, lineStep{done: true}, reload); cmd == nil || tr.running {
		t.Error("a clean end should stop running and return onDone")
	}
	if !strings.Contains(tr.view("Exporting…"), "dismiss") {
		t.Error("a finished transcript should offer to dismiss it")
	}
	if !tr.dismiss("esc") || tr.active() {
		t.Error("esc should clear a finished transcript")
	}

	tr.start()
	if cmd := follow(&tr, failed[struct{}](errors.New("disk full")), reload); cmd != nil || tr.running {
		t.Error("an error should end the stream without onDone")
	}
	if !strings.Contains(tr.lines[len(tr.lines)-1], "disk full") {
		t.Errorf("expected the error line, got %q", tr.lines)
	}
}
