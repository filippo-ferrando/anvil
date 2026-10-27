package tui

import (
	"slices"
	"testing"
)

func TestNewWizardRequest(t *testing.T) {
	req := newWizardRequest("web", "ubuntu", "ssh-ed25519 AAAA", "nginx, curl", "apt-get install -y a, b; ufw allow 80")
	if req.GetSaveAs() != "web" || req.GetUser() != "ubuntu" {
		t.Fatalf("save_as/user = %q/%q", req.GetSaveAs(), req.GetUser())
	}
	if !slices.Equal(req.GetSshKeys(), []string{"ssh-ed25519 AAAA"}) {
		t.Fatalf("ssh keys = %v", req.GetSshKeys())
	}
	if !slices.Equal(req.GetPackages(), []string{"nginx", " curl"}) {
		t.Fatalf("packages = %q", req.GetPackages())
	}
	// Commas inside a run command pass through; only ";" splits.
	if !slices.Equal(req.GetRunCommands(), []string{"apt-get install -y a, b", " ufw allow 80"}) {
		t.Fatalf("run commands = %q", req.GetRunCommands())
	}

	if empty := newWizardRequest("x", "", "", "", ""); len(empty.GetSshKeys()) != 0 {
		t.Fatalf("an empty key field sent keys %v", empty.GetSshKeys())
	}
}
