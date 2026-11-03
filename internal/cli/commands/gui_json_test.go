package commands

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/gui/events"
)

func parseAll(t *testing.T, s string) []events.Event {
	t.Helper()
	var out []events.Event
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		e, ok := events.Parse([]byte(l))
		if !ok {
			t.Fatalf("not an event: %q", l)
		}
		out = append(out, e)
	}
	return out
}

func TestGUIOutputJSON(t *testing.T) {
	var out, progress bytes.Buffer
	o := guiOutput{json: true, out: &out, progress: &progress}
	fmt.Fprintln(o.steps(), "preparing dev")
	o.running("firefox", "dev", "firefox runs in dev")
	_ = o.finish(nil)
	_ = o.finish(errors.New("no such instance"))
	got := parseAll(t, out.String())
	want := []events.Event{
		{Event: events.Step, Text: "preparing dev"},
		{Event: events.Running, App: "firefox", Where: "dev", Text: "firefox runs in dev"},
		{Event: events.Done},
		{Event: events.Error, Message: "no such instance"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if progress.Len() != 0 {
		t.Errorf("human output with --json: %q", progress.String())
	}
}

func TestGUIOutputHuman(t *testing.T) {
	var out, progress bytes.Buffer
	o := guiOutput{out: &out, progress: &progress}
	fmt.Fprintln(o.steps(), "preparing dev")
	o.running("firefox", "dev", "firefox runs in dev")
	if err := o.finish(errors.New("x")); err == nil || err.Error() != "x" {
		t.Fatalf("finish = %v", err)
	}
	if out.Len() != 0 || progress.String() != "preparing dev\nanvil: firefox runs in dev\n" {
		t.Fatalf("out %q, progress %q", out.String(), progress.String())
	}
}

func TestGUIJSONFlag(t *testing.T) {
	root := NewRootCommand()
	for _, path := range [][]string{{"gui"}, {"gui", "export"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Flags().Lookup("json") == nil {
			t.Errorf("%v has no --json", path)
		}
	}
}
