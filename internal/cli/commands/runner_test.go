package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestParseLabels(t *testing.T) {
	m, err := parseLabels([]string{"debian=debian-13", "big=@3f9a"})
	if err != nil || m["debian"] != "debian-13" || m["big"] != "@3f9a" {
		t.Errorf("parseLabels = %v, %v", m, err)
	}
	for _, bad := range []string{"debian", "=x", "x="} {
		if _, err := parseLabels([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := runnerLabels(map[string]string{"b": "2", "a": "1"}); got != "a=1,b=2" {
		t.Errorf("runnerLabels = %q", got)
	}
}

func TestReadToken(t *testing.T) {
	t.Setenv(runnerTokenEnv, "")
	if _, err := readToken("", nil); err == nil {
		t.Error("no token accepted")
	}
	t.Setenv(runnerTokenEnv, " env-tok \n")
	if tok, err := readToken("", nil); err != nil || tok != "env-tok" {
		t.Errorf("env = %q, %v", tok, err)
	}
	if tok, err := readToken("-", strings.NewReader("stdin-tok\n")); err != nil || tok != "stdin-tok" {
		t.Errorf("stdin = %q, %v", tok, err)
	}
	f := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(f, []byte("file-tok\n"), 0o600)
	if tok, err := readToken(f, nil); err != nil || tok != "file-tok" {
		t.Errorf("file = %q, %v", tok, err)
	}
	_ = os.WriteFile(f, []byte("  \n"), 0o600)
	if _, err := readToken(f, nil); err == nil {
		t.Error("an empty token file was accepted")
	}
}

func TestRunnerColumns(t *testing.T) {
	for _, tc := range []struct {
		r    *anvilv1.Runner
		want string
	}{
		{&anvilv1.Runner{Paused: true, LastError: "x"}, "paused"},
		{&anvilv1.Runner{LastError: "403"}, "error: 403"},
		{&anvilv1.Runner{}, "starting"},
		{&anvilv1.Runner{LastPollUnix: time.Now().Add(-3 * time.Second).Unix()}, "polled "},
	} {
		if got := runnerState(tc.r); !strings.HasPrefix(got, tc.want) {
			t.Errorf("runnerState = %q, want %q", got, tc.want)
		}
	}
	if got := oneLine("first\nsecond line is long", 10); got != "second li…" {
		t.Errorf("oneLine = %q", got)
	}
	if got := jobDuration(100, 165); got != "1m5s" {
		t.Errorf("jobDuration = %q", got)
	}
}
