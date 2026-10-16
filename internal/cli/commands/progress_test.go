package commands

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

type fakeStream struct{ events []*anvilv1.LaunchProgress }

func (f *fakeStream) Recv() (*anvilv1.LaunchProgress, error) {
	if len(f.events) == 0 {
		return nil, io.EOF
	}
	ev := f.events[0]
	f.events = f.events[1:]
	return ev, nil
}

func statusEvent(s string) *anvilv1.LaunchProgress {
	return &anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Status{Status: s}}
}

func TestStreamProgressPlainOutput(t *testing.T) {
	var out bytes.Buffer
	stream := &fakeStream{events: []*anvilv1.LaunchProgress{
		statusEvent("pulling redis:7: 10% (1.0 MiB / 10.0 MiB), 0/2 layers"),
		statusEvent("starting"),
		{Event: &anvilv1.LaunchProgress_Instance{Instance: &anvilv1.Instance{Name: "cache", Id: "01X"}}},
	}}
	if err := streamProgress(&out, stream, "Launched"); err != nil {
		t.Fatal(err)
	}
	// Off a terminal: every status on its own line, no bars or escape codes for logs and pipes.
	want := "pulling redis:7: 10% (1.0 MiB / 10.0 MiB), 0/2 layers\nstarting\nLaunched \"cache\" (01X)\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestLiveLineRedrawsOneKeyInPlace(t *testing.T) {
	var out bytes.Buffer
	l := &liveLine{out: &out, term: true, width: 80}
	l.set("pulling redis:7: 10% (1.0 MiB / 10.0 MiB), 0/2 layers")
	l.set("pulling redis:7: 60% (6.0 MiB / 10.0 MiB), 1/2 layers")
	if strings.Contains(out.String(), "\n") {
		t.Errorf("updates with one key must stay on one line, got %q", out.String())
	}
	if !strings.Contains(out.String(), "] 60%") {
		t.Errorf("expected a bar in front of the percentage, got %q", out.String())
	}

	l.since = time.Now().Add(-5 * time.Second) // as if the next step had run a while
	l.set("starting")
	if n := strings.Count(out.String(), "\n"); n != 1 {
		t.Errorf("a new key should end the previous line once, got %d newlines", n)
	}
	l.since = time.Now().Add(-12 * time.Second)
	l.redraw()
	if !strings.HasSuffix(out.String(), "starting  "+string([]rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")[l.frame%10])+" 12s") {
		t.Errorf("a long step should show a spinner and its time, got %q", out.String())
	}
	l.finish()
	if !strings.HasSuffix(out.String(), "starting  (12s)\n") {
		t.Errorf("a finished step should keep its time, got %q", out.String())
	}
}

func TestLiveLineFitsTheTerminal(t *testing.T) {
	l := &liveLine{width: 10}
	if got := l.fit("waiting for cloud-init: running"); len([]rune(got)) != 9 || !strings.HasSuffix(got, "…") {
		t.Errorf("fit = %q", got)
	}
}
