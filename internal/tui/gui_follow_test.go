package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func collect() (*[]string, func(string)) {
	var got []string
	return &got, func(l string) { got = append(got, l) }
}

func TestFollowGUILog_LastRunUntilAppRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	old := "--- 2026-01-01 anvil gui\nold line\nanvil: old failure\n"
	if err := os.WriteFile(path, []byte(old+"--- 2026-11-02 anvil gui\nrestoring the clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * logPoll / 2)
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		defer f.Close()
		_, _ = f.WriteString("starting the app\nanvil: firefox runs in c1, deleted when it exits\napp output\n")
	}()
	got, emit := collect()
	if err := followGUILog(context.Background(), path, emit); err != nil {
		t.Fatal(err)
	}
	want := []string{"restoring the clone", "starting the app", "firefox runs in c1, deleted when it exits"}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %q, want %q", *got, want)
	}
}

func TestFollowGUILog_Failure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("--- now\nbuilding\nanvil: no such distro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, emit := collect()
	err := followGUILog(context.Background(), path, emit)
	if err == nil || err.Error() != "no such distro" {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(*got, []string{"building"}) {
		t.Fatalf("got %q", *got)
	}
}

func TestFollowGUILog_StopsWithContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("--- now\nwaiting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*logPoll)
	defer cancel()
	if err := followGUILog(ctx, path, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

func TestRunFollowed_StreamsThenFollowsLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("--- now\nanvil: app runs in c1, deleted when it exits\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := [][]string{
		{"-c", "echo building warm; echo Exported x.desktop"},
		{"-c", "echo '" + detachedPrefix + path + "'"},
	}
	got, emit := collect()
	if err := runFollowed(context.Background(), "sh", calls, emit); err != nil {
		t.Fatal(err)
	}
	want := []string{"building warm", "Exported x.desktop", detachedPrefix + path, "app runs in c1, deleted when it exits"}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %q, want %q", *got, want)
	}
}

func TestRunFollowed_FailureStopsCalls(t *testing.T) {
	calls := [][]string{
		{"-c", "echo step; echo 'anvil: build failed' >&2; exit 1"},
		{"-c", "echo never"},
	}
	got, emit := collect()
	err := runFollowed(context.Background(), "sh", calls, emit)
	if err == nil || err.Error() != "build failed" {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(*got, ",") != "step" {
		t.Fatalf("got %q", *got)
	}
}

func TestFollowCalls_Messages(t *testing.T) {
	next := followCalls(context.Background(), "sh", [][]string{{"-c", "echo one; echo two"}}, lineTaskStep(7))
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
