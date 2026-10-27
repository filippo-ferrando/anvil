package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewApplyRequestFromForm(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	manifest := filepath.Join(dir, "anvil.yaml")
	if err := os.WriteFile(manifest, []byte("name: ${APP}\nmembers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "prod.env")
	if err := os.WriteFile(envFile, []byte("APP=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	form := newApplyForm()
	setField(&form, "Manifest path", manifest)
	setField(&form, "Env file (optional)", envFile)
	setField(&form, "Wait timeout", "2m")
	toggle(&form, "Prune")
	req, err := newApplyRequest(form)
	if err != nil {
		t.Fatal(err)
	}
	if !req.GetDryRun() || !req.GetPrune() || req.GetRecreate() || req.GetWaitTimeoutSeconds() != 120 {
		t.Errorf("unexpected request %+v", req)
	}
	if got := req.GetVars()["APP"]; got != "from-file" {
		t.Errorf("APP = %q, want the env file's value", got)
	}

	setField(&form, "Vars (optional)", "APP=web, EXTRA=1")
	if req, err = newApplyRequest(form); err != nil || req.GetVars()["APP"] != "web" || req.GetVars()["EXTRA"] != "1" {
		t.Errorf("expected Vars to win over the env file, got %v, %v", req.GetVars(), err)
	}

	setField(&form, "Vars (optional)", "nokey")
	if _, err := newApplyRequest(form); err == nil {
		t.Error("expected a var without = to fail")
	}
}
