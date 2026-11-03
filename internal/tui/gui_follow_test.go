package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"git.fferrando.cc/rdfilippo/anvil/internal/gui/events"
)

func collect() (*[]string, func(string)) {
	var got []string
	return &got, func(l string) { got = append(got, l) }
}

// ev is e as one `anvil gui --json` output line.
func ev(e events.Event) string {
	var b strings.Builder
	_ = events.Encode(&b, e)
	return b.String()
}

func step(text string) string { return ev(events.Event{Event: events.Step, Text: text}) }

func running(text string) string { return ev(events.Event{Event: events.Running, Text: text}) }

// echoArgs is a `sh -c` call printing lines.
func echoArgs(lines ...string) []string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + strings.TrimSpace(l) + "'; ")
	}
	return []string{"-c", b.String()}
}

func TestFollowGUILog_FromOffsetUntilAppRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	old := "--- 2026-01-01 anvil gui\n" + step("old line") + ev(events.Event{Event: events.Error, Message: "old failure"}) + "--- now\n"
	if err := os.WriteFile(path, []byte(old+step("restoring the clone")), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * logPoll / 2)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		defer f.Close()
		_, _ = f.WriteString(step("starting the app") + "waypipe noise\n" + running("firefox runs in c1, deleted when it exits") + step("after"))
	}()
	got, emit := collect()
	if err := followGUILog(context.Background(), path, int64(len(old)), emit); err != nil {
		t.Fatal(err)
	}
	want := []string{"restoring the clone", "starting the app", "firefox runs in c1, deleted when it exits"}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %q, want %q", *got, want)
	}
}

func TestFollowGUILog_Failure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	data := step("building") + "anvil: no such distro\n" + ev(events.Event{Event: events.Error, Message: "no such distro"})
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got, emit := collect()
	err := followGUILog(context.Background(), path, 0, emit)
	if err == nil || err.Error() != "no such distro" {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(*got, []string{"building"}) {
		t.Fatalf("got %q", *got)
	}
}

func TestFollowGUILog_DoneEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(step("preparing")+ev(events.Event{Event: events.Done})), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := followGUILog(context.Background(), path, 0, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestFollowGUILog_StopsWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(step("waiting")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*logPoll)
	defer cancel()
	if err := followGUILog(ctx, path, 0, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestRunFollowed_StreamsThenFollowsLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("--- now\n"+running("app runs in c1")), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := [][]string{
		echoArgs(step("building warm"), "not an event", ev(events.Event{Event: events.Exported, Path: "x.desktop"}), ev(events.Event{Event: events.Done})),
		echoArgs(ev(events.Event{Event: events.Detached, Log: path, Offset: int64(len("--- now\n"))}), ev(events.Event{Event: events.Done})),
	}
	got, emit := collect()
	if err := runFollowed(context.Background(), "sh", calls, emit); err != nil {
		t.Fatal(err)
	}
	want := []string{"building warm", "exported x.desktop", "started in the background, log: " + path, "app runs in c1"}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %q, want %q", *got, want)
	}
}

func TestRunFollowed_FailureStopsCalls(t *testing.T) {
	fail := echoArgs(step("step"), ev(events.Event{Event: events.Error, Message: "build failed"}))
	fail[1] += "echo 'anvil: build failed' >&2; exit 1"
	calls := [][]string{fail, echoArgs(step("never"))}
	got, emit := collect()
	err := runFollowed(context.Background(), "sh", calls, emit)
	if err == nil || err.Error() != "build failed" {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(*got, ",") != "step" {
		t.Fatalf("got %q", *got)
	}
}

func TestRunFollowed_FailureWithoutEvent(t *testing.T) {
	calls := [][]string{{"-c", "echo 'anvil: unknown flag: --bad' >&2; exit 1"}}
	err := runFollowed(context.Background(), "sh", calls, func(string) {})
	if err == nil || err.Error() != "anvil: unknown flag: --bad" {
		t.Fatalf("err = %v", err)
	}
}

func TestFollowCalls_Messages(t *testing.T) {
	next := followCalls(context.Background(), "sh", [][]string{echoArgs(step("one"), step("two"))}, lineTaskStep(7))
	var lines []string
	for {
		msg := next().(taskStepMsg)
		if msg.id != 7 {
			t.Fatalf("id = %d", msg.id)
		}
		if msg.done {
			if msg.err != nil {
				t.Fatal(msg.err)
			}
			break
		}
		lines, next = append(lines, msg.status), msg.next
	}
	if !reflect.DeepEqual(lines, []string{"one", "two"}) {
		t.Fatalf("got %q", lines)
	}
}

func TestFollowCalls_CancelledEndsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	next := followCalls(ctx, "sh", [][]string{{"-c", "true"}}, lineTaskStep(1))
	for {
		msg := next().(taskStepMsg)
		if msg.done {
			if status.Code(msg.err) != codes.Canceled {
				t.Fatalf("err = %v", msg.err)
			}
			return
		}
		next = msg.next
	}
}

func TestRunFollowed_CancelStopsCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := echoArgs(step("started"), step("waiting"))
	first[1] += "sleep 30"
	calls := [][]string{first, echoArgs(step("never"))}
	var got []string
	emit := func(l string) {
		got = append(got, l)
		cancel()
	}
	start := time.Now()
	if err := runFollowed(ctx, "sh", calls, emit); err != nil {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("cancel did not kill the running call")
	}
	if slices.Contains(got, "never") {
		t.Fatalf("got %q, the second call ran after cancel", got)
	}
}
