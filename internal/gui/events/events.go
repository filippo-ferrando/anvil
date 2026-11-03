// Package events is the newline-delimited JSON that `anvil gui --json` writes on stdout,
// read back by the TUI.
package events

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// Event kinds.
const (
	Step     = "step"     // Text: a progress step
	Exported = "exported" // Path: the desktop entry written by `anvil gui export`
	Detached = "detached" // Log, Offset: the background run's log and where its events start
	Running  = "running"  // App, Where: the app started, Text says so in words
	Error    = "error"    // Message: why the command failed
	Done     = "done"     // the command ended without an error
)

// Event is one line of `anvil gui --json` output.
type Event struct {
	Event   string `json:"event"`
	Text    string `json:"text,omitempty"`
	Path    string `json:"path,omitempty"`
	Log     string `json:"log,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	App     string `json:"app,omitempty"`
	Where   string `json:"where,omitempty"`
	Message string `json:"message,omitempty"`
}

// Encode writes e to w as a single line.
func Encode(w io.Writer, e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// Parse reads one output line; ok is false for a line that isn't an event, like app output.
func Parse(line []byte) (e Event, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return Event{}, false
	}
	if err := json.Unmarshal(line, &e); err != nil || e.Event == "" {
		return Event{}, false
	}
	return e, true
}

// String is the event as a status line, empty for done.
func (e Event) String() string {
	switch e.Event {
	case Exported:
		return "exported " + e.Path
	case Detached:
		return "started in the background, log: " + e.Log
	case Error:
		return e.Message
	}
	return e.Text
}

// StepWriter turns every line written to it into a step event on W.
type StepWriter struct {
	W   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (s *StepWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(bytes.TrimSpace(bytes.TrimRight(s.buf[:i], "\r")))
		s.buf = s.buf[i+1:]
		if line == "" {
			continue
		}
		if err := Encode(s.W, Event{Event: Step, Text: line}); err != nil {
			return len(p), err
		}
	}
}
