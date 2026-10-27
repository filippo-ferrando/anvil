package client

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseVars(t *testing.T) {
	got, err := ParseVars([]string{"A=1", "B=x=y", "C="})
	if err != nil || got["A"] != "1" || got["B"] != "x=y" || got["C"] != "" {
		t.Errorf("got %v, %v", got, err)
	}
	for _, bad := range []string{"A", "=1"} {
		if _, err := ParseVars([]string{bad}); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestCeilSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int32{0: 0, -time.Second: 0, 300 * time.Millisecond: 1, 15 * time.Minute: 900} {
		if got := CeilSeconds(d); got != want {
			t.Errorf("CeilSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestNewApplyRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "anvil.yaml")
	data := []byte("name: ${APP}\n")
	req, err := NewApplyRequest(path, data, ApplyOptions{Prune: true, Vars: []string{"APP=web"}, WaitTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !req.GetPrune() || req.GetDryRun() || req.GetWaitTimeoutSeconds() != 60 || req.GetVars()["APP"] != "web" {
		t.Errorf("unexpected request %+v", req)
	}
	if len(req.GetSshPublicKeys()) != 1 {
		t.Errorf("expected anvil's own key, got %q", req.GetSshPublicKeys())
	}
	if _, err := NewApplyRequest(path, data, ApplyOptions{EnvFile: filepath.Join(os.TempDir(), "missing-anvil.env")}); err == nil {
		t.Error("expected a missing explicit env file to fail")
	}
}
