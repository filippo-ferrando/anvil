package commands

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestSplitRunArgs(t *testing.T) {
	dir := t.TempDir()
	parse := func(args ...string) (string, string, []string, error) {
		var got struct {
			first, path string
			command     []string
			err         error
		}
		cmd := &cobra.Command{Use: "x", RunE: func(cmd *cobra.Command, args []string) error {
			got.first, got.path, got.command, got.err = splitRunArgs(cmd, args)
			return nil
		}}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return got.first, got.path, got.command, got.err
	}
	if d, p, c, err := parse("ubuntu", dir, "--", "make", "-j4"); err != nil || d != "ubuntu" || p != dir || !slices.Equal(c, []string{"make", "-j4"}) {
		t.Errorf("with a path = %q %q %v %v", d, p, c, err)
	}
	if d, p, c, err := parse("ubuntu", "--", "uname", dir); err != nil || d != "ubuntu" || p != "" || !slices.Equal(c, []string{"uname", dir}) {
		t.Errorf("a path after -- belongs to the command: %q %q %v %v", d, p, c, err)
	}
	for _, bad := range [][]string{{"ubuntu", "uname"}, {"ubuntu", "--"}, {"ubuntu", filepath.Join(dir, "missing"), "--", "ls"}, {"a", "b", "c", "--", "ls"}} {
		if _, _, _, err := parse(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestRunPrinterLines(t *testing.T) {
	var stdout, stderr bytes.Buffer
	p := newRunPrinter(&stdout, &stderr, []string{"ubuntu", "alpine"}, false)
	out := func(d, data string, errStream bool) *anvilv1.RunEvent {
		st := anvilv1.OutputStream_OUTPUT_STREAM_STDOUT
		if errStream {
			st = anvilv1.OutputStream_OUTPUT_STREAM_STDERR
		}
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: d, Stream: st, Data: []byte(data)}}}
	}
	exit := func(d string, code int32, kept bool, msg string) *anvilv1.RunEvent {
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: &anvilv1.RunExit{Distro: d, CloneId: "eph-X", ExitCode: code, DurationMs: 1234, Kept: kept, Error: msg}}}
	}
	// A line split across chunks and interleaved with another clone's still prints whole.
	for _, ev := range []*anvilv1.RunEvent{
		out("ubuntu", "hel", false), out("alpine", "one\ntw", false), out("ubuntu", "lo\nwor", false),
		out("alpine", "warn\n", true), exit("alpine", 0, false, ""), out("ubuntu", "ld", false), exit("ubuntu", 2, true, ""),
	} {
		p.event(ev)
	}
	p.finish()
	want := "[alpine] one\n[ubuntu] hello\n[alpine] tw\n[ubuntu] world\n"
	if stdout.String() != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
	if !strings.HasPrefix(stderr.String(), "[alpine] warn\n") {
		t.Errorf("stderr = %q", stderr.String())
	}
	summary := stderr.String()
	for _, want := range []string{"DISTRO", "ubuntu  2     1.23s     kept as eph-X", "alpine  0"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	if code := p.exitCode(); code != 1 {
		t.Errorf("matrix exit code = %d, want 1", code)
	}

	single := newRunPrinter(io.Discard, io.Discard, []string{"ubuntu"}, false)
	single.event(exit("ubuntu", 7, false, ""))
	if code := single.exitCode(); code != 7 {
		t.Errorf("single exit code = %d, want the command's 7", code)
	}
	failed := newRunPrinter(io.Discard, io.Discard, []string{"ubuntu"}, false)
	failed.event(exit("ubuntu", -1, false, "restore failed"))
	if code := failed.exitCode(); code != 1 {
		t.Errorf("exit code of a clone that never ran = %d, want 1", code)
	}
}

func TestRunPrinterLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	p := newRunPrinter(io.Discard, io.Discard, []string{"ubuntu", "debian"}, false)
	if err := p.openLogs(dir); err != nil {
		t.Fatal(err)
	}
	p.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: "ubuntu", Data: []byte("a\nb")}}})
	p.closeLogs()
	if data, _ := os.ReadFile(filepath.Join(dir, "ubuntu.log")); string(data) != "a\nb" {
		t.Errorf("ubuntu.log = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "debian.log")); err != nil {
		t.Errorf("no log for a distro without output: %v", err)
	}
}
