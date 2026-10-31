package ephemeral

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseCollect(t *testing.T) {
	for _, tc := range []struct {
		in, cwd    string
		base       string
		glob, name []string
	}{
		{in: "dist", cwd: "/work", base: "/work", name: []string{"dist"}},
		{in: "/var/log/syslog", cwd: "/work", base: "/var/log", name: []string{"syslog"}},
		{in: "out/../dist/", cwd: "/work", base: "/work", name: []string{"dist"}},
		{in: "/work/**/*.xml", cwd: "/", base: "/work", glob: []string{"**", "*.xml"}},
		{in: "reports/*.xml", cwd: "/work", base: "/work/reports", glob: []string{"*.xml"}},
		{in: "/*.log", cwd: "/work", base: "/", glob: []string{"*.log"}},
	} {
		got, err := parseCollect(tc.in, tc.cwd)
		if err != nil || got.base != tc.base || !slices.Equal(got.glob, tc.glob) || !slices.Equal(got.names, tc.name) {
			t.Errorf("parseCollect(%q, %q) = %+v, %v", tc.in, tc.cwd, got, err)
		}
	}
	for _, bad := range []string{"", " ", "/", "/work/[a"} {
		if _, err := parseCollect(bad, "/work"); err == nil {
			t.Errorf("parseCollect(%q) was accepted", bad)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	split := func(s string) []string { return strings.Split(s, "/") }
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.xml", "a.xml", true},
		{"*.xml", "d/a.xml", false},
		{"**/*.xml", "a.xml", true},
		{"**/*.xml", "d/e/a.xml", true},
		{"**/*.xml", "d/e/a.txt", false},
		{"d/**", "d/e/f", true},
		{"d/**/f", "d/f", true},
		{"d/**/f", "x/f", false},
		{"**", "anything/at/all", true},
	} {
		if got := matchGlob(split(tc.pattern), split(tc.name)); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestMatchListing(t *testing.T) {
	listing := strings.Join([]string{"/work/", "/work/a.xml", "/work/d", "/work/d/b.xml", "/work/d/c.txt", "/work/x.xml", "/work/x.xml/inner.xml", "/work/new\nline.xml"}, "\x00") + "\x00"
	got := matchListing("/work", []string{"**", "*.xml"}, []byte(listing))
	// x.xml is a directory that matched, so its own matches ride along in its tar.
	if want := []string{"a.xml", "d/b.xml", "new\nline.xml", "x.xml"}; !slices.Equal(got, want) {
		t.Errorf("matchListing = %q, want %q", got, want)
	}
	if got := matchListing("/", []string{"*.log"}, []byte("/\x00/a.log\x00/d/b.log\x00")); !slices.Equal(got, []string{"a.log"}) {
		t.Errorf("matchListing under / = %q", got)
	}
}

func TestChunker(t *testing.T) {
	var got [][]byte
	c := &chunker{send: func(b []byte) { got = append(got, b) }}
	data := bytes.Repeat([]byte("ab"), collectChunk) // two full chunks
	_, _ = c.Write(data[:100])
	_, _ = c.Write(data[100:])
	_, _ = c.Write([]byte("tail"))
	c.flush()
	if len(got) != 3 || len(got[0]) != collectChunk || len(got[1]) != collectChunk || string(got[2]) != "tail" || c.sent != len(data)+4 {
		t.Fatalf("chunks: %d, sent %d", len(got), c.sent)
	}
	got[0][0] = 'X' // each chunk is its own memory
	if got[1][0] != 'a' {
		t.Error("chunks share memory")
	}
}

// TestRunCollect runs a real anvil-guest server on this host, so it needs find and tar.
func TestRunCollect(t *testing.T) {
	for _, tool := range []string{"find", "tar", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	m, _ := newTestManager(t)
	dir := t.TempDir()
	script := `mkdir -p out/a out/b && echo one > out/a/r.xml && echo two > out/b/s.xml && echo txt > out/b/n.txt && echo log > top.log; exit 2`

	tars := map[int]*bytes.Buffer{}
	warnings := map[int]string{}
	var order []string
	err := m.Run(context.Background(), RunRequest{
		Distros: []string{"ubuntu"}, Argv: []string{"sh", "-c", script}, Cwd: dir,
		Collect: []string{"out/**/*.xml", "top.log", "missing.txt", dir + "/nothing/*.xml", "out/b"},
	}, func(ev Event) {
		switch {
		case ev.Collect != nil:
			c := ev.Collect
			if c.Distro != "ubuntu" || c.CloneID == "" {
				t.Errorf("collect event without its clone: %+v", c)
			}
			if tars[c.Index] == nil {
				tars[c.Index] = &bytes.Buffer{}
			}
			tars[c.Index].Write(c.Data)
			if c.Done {
				warnings[c.Index] = c.Warning
				order = append(order, "done")
			}
		case ev.Exit != nil:
			order = append(order, "exit")
			if ev.Exit.Code != 2 || ev.Exit.Err != "" {
				t.Errorf("exit = %+v, want the command's code 2", ev.Exit)
			}
		}
	})
	m.WaitDeletes()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"done", "done", "done", "done", "done", "exit"}; !slices.Equal(order, want) {
		t.Errorf("event order = %v, want %v", order, want)
	}
	for i, want := range map[int][]string{
		0: {"a/r.xml=one\n", "b/s.xml=two\n"},
		1: {"top.log=log\n"},
		4: {"b/", "b/n.txt=txt\n", "b/s.xml=two\n"},
	} {
		if got := tarFiles(t, tars[i]); !slices.Equal(got, want) || warnings[i] != "" {
			t.Errorf("collect %d = %q (warning %q), want %q", i, got, warnings[i], want)
		}
	}
	for i, want := range map[int]string{2: "missing.txt: no such file", 3: "no match"} {
		if !strings.Contains(warnings[i], want) || (tars[i] != nil && tars[i].Len() > 0) {
			t.Errorf("collect %d: warning %q, %d bytes; want %q", i, warnings[i], tars[i].Len(), want)
		}
	}
}

// tarFiles lists a tar's entries as name=content for files, name/ for directories.
func tarFiles(t *testing.T, b *bytes.Buffer) []string {
	t.Helper()
	if b == nil {
		return nil
	}
	var out []string
	tr := tar.NewReader(b)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Clean(h.Name)
		if h.Typeflag == tar.TypeDir {
			out = append(out, name+"/")
			continue
		}
		data, _ := io.ReadAll(tr)
		out = append(out, name+"="+string(data))
	}
	slices.Sort(out)
	return out
}

func TestRunCwd(t *testing.T) {
	for _, tc := range []struct {
		r    RunRequest
		want string
	}{
		{RunRequest{}, "/"},
		{RunRequest{Upload: "x.tar"}, workDir},
		{RunRequest{Upload: "x.tar", Cwd: "/src"}, "/src"},
	} {
		if got := runCwd(tc.r); got != tc.want {
			t.Errorf("runCwd(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}
