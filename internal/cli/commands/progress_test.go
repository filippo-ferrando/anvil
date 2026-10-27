package commands

import (
	"bytes"
	"errors"
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

func TestStreamStatusStopsAndFails(t *testing.T) {
	var out bytes.Buffer
	evs := []string{"a", "b", "stop", "c"}
	recv := func() (string, error) {
		if len(evs) == 0 {
			return "", io.EOF
		}
		ev := evs[0]
		evs = evs[1:]
		return ev, nil
	}
	err := streamStatus(&out, recv, func(l *liveLine, ev string) (bool, error) {
		if ev == "stop" {
			return true, nil
		}
		l.set(ev)
		return false, nil
	})
	if err != nil || out.String() != "a\nb\n" {
		t.Fatalf("stop: err=%v out=%q", err, out.String())
	}
	one := func() (string, error) { return "x", nil }
	err = streamStatus(&out, one, func(*liveLine, string) (bool, error) { return false, errors.New("boom") })
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want the handler's error, got %v", err)
	}
}

type fakeApplyStream struct {
	events []*anvilv1.IntentApplyProgress
}

func (f *fakeApplyStream) Recv() (*anvilv1.IntentApplyProgress, error) {
	if len(f.events) == 0 {
		return nil, io.EOF
	}
	ev := f.events[0]
	f.events = f.events[1:]
	return ev, nil
}

func TestStreamApplyPlainOutput(t *testing.T) {
	var out bytes.Buffer
	stream := &fakeApplyStream{events: []*anvilv1.IntentApplyProgress{
		{Event: &anvilv1.IntentApplyProgress_Plan{Plan: &anvilv1.IntentApplyPlan{IntentName: "web"}}},
		{Event: &anvilv1.IntentApplyProgress_Status{Status: "db: launching"}},
		{Event: &anvilv1.IntentApplyProgress_Intent{Intent: &anvilv1.Intent{Name: "web"}}},
	}}
	if _, err := streamApply(&out, stream, true); err != nil {
		t.Fatal(err)
	}
	want := "Plan for intent \"web\":\ndb: launching\nApplied intent \"web\" (0 members)\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

type fakeAddMembersStream struct {
	events []*anvilv1.IntentAddMembersProgress
}

func (f *fakeAddMembersStream) Recv() (*anvilv1.IntentAddMembersProgress, error) {
	if len(f.events) == 0 {
		return nil, io.EOF
	}
	ev := f.events[0]
	f.events = f.events[1:]
	return ev, nil
}

func memberEvent(role string, ev *anvilv1.LaunchProgress) *anvilv1.IntentAddMembersProgress {
	return &anvilv1.IntentAddMembersProgress{Event: &anvilv1.IntentAddMembersProgress_Member{
		Member: &anvilv1.IntentMemberProgress{Role: role, Launch: ev},
	}}
}

func TestStreamAddMembersPrintsEachLaunch(t *testing.T) {
	var out bytes.Buffer
	stream := &fakeAddMembersStream{events: []*anvilv1.IntentAddMembersProgress{
		memberEvent("db", statusEvent("booting")),
		memberEvent("db", &anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Instance{Instance: &anvilv1.Instance{Name: "shop-db", Id: "01A"}}}),
		memberEvent("web", statusEvent("starting")),
		{Event: &anvilv1.IntentAddMembersProgress_Error{Error: "image bad is broken"}},
	}}
	err := streamAddMembers(&out, stream)
	if err == nil || err.Error() != "image bad is broken" {
		t.Errorf("expected the final error, got %v", err)
	}
	want := "booting\nLaunched \"shop-db\" (01A)\nstarting\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestParseMemberFlagsOrderAndErrors(t *testing.T) {
	members, err := parseMemberFlags([]string{"db:debian-13"}, []string{"cache:redis:7"})
	if err != nil || len(members) != 2 || members[0].GetKind() != anvilv1.Kind_KIND_VM || members[1].GetImage() != "redis:7" {
		t.Fatalf("got %v, %v", members, err)
	}
	if _, err := parseMemberFlags(nil, []string{"redis"}); err == nil || !strings.HasPrefix(err.Error(), "--container ") {
		t.Errorf("expected a --container error, got %v", err)
	}
}
