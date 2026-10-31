package commands

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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

// countingWriter counts its writes.
type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

func TestWriteLinesOneWrite(t *testing.T) {
	var out countingWriter
	p := newRunPrinter(&out, io.Discard, []string{"alpine", "ubuntu-24.04"}, false)
	p.writeLines("alpine", anvilv1.OutputStream_OUTPUT_STREAM_STDOUT, &out, []byte("one\ntwo\nthree\npart"))
	if out.writes != 1 {
		t.Errorf("%d writes for three lines, want one", out.writes)
	}
	want := "[alpine      ] one\n[alpine      ] two\n[alpine      ] three\n"
	if out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
	p.writeLines("alpine", anvilv1.OutputStream_OUTPUT_STREAM_STDOUT, &out, []byte("ial\n"))
	if !strings.HasSuffix(out.String(), "[alpine      ] partial\n") || out.writes != 2 {
		t.Errorf("the rest of a line came out as %q in %d writes", out.String(), out.writes)
	}
}

func TestRunPrinterCollect(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	p := newRunPrinter(&stdout, &stderr, []string{"ubuntu"}, false)
	p.collector = client.NewCollector([]string{"dist", "*.xml"}, func(i int, _ string) string { return filepath.Join(dir, "out") })
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "dist/app", Mode: 0o755, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	collect := func(index int32, data []byte, done bool, warning string) *anvilv1.RunEvent {
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Collect{Collect: &anvilv1.RunCollect{
			CloneId: "eph-X", Distro: "ubuntu", Index: index, Data: data, Done: done, Warning: warning}}}
	}
	p.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: "ubuntu", Stream: anvilv1.OutputStream_OUTPUT_STREAM_STDOUT, Data: []byte("no newline")}}})
	p.event(collect(0, tb.Bytes(), false, ""))
	p.event(collect(0, nil, true, ""))
	p.event(collect(1, nil, true, "*.xml: no match"))
	if got, err := os.ReadFile(filepath.Join(dir, "out", "dist", "app")); err != nil || string(got) != "hi" {
		t.Errorf("collected file = %q, %v", got, err)
	}
	for _, want := range []string{"[ubuntu] collected dist into " + filepath.Join(dir, "out"), "[ubuntu] collect: *.xml: no match"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if stdout.String() != "[ubuntu] no newline\n" {
		t.Errorf("the command's last line wasn't flushed before the note: %q", stdout.String())
	}
}
