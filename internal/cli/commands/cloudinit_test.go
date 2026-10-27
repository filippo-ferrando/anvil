package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCloudInitGenOptionsRequest(t *testing.T) {
	pub := filepath.Join(t.TempDir(), "id.pub")
	if err := os.WriteFile(pub, []byte("ssh-ed25519 FILE me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := cloudInitGenOptions{
		user:     "ubuntu",
		sshKeys:  []string{"ssh-rsa LIT", pub},
		packages: []string{"nginx", "curl"},
		runCmds:  []string{"echo a, b"},
	}
	req, err := o.request("web")
	if err != nil {
		t.Fatal(err)
	}
	if req.GetSaveAs() != "web" || req.GetUser() != "ubuntu" {
		t.Fatalf("save_as/user = %q/%q", req.GetSaveAs(), req.GetUser())
	}
	if !slices.Equal(req.GetSshKeys(), []string{"ssh-rsa LIT", "ssh-ed25519 FILE me"}) {
		t.Fatalf("ssh keys = %q", req.GetSshKeys())
	}
	if !slices.Equal(req.GetPackages(), o.packages) || !slices.Equal(req.GetRunCommands(), o.runCmds) {
		t.Fatalf("packages/run = %q/%q", req.GetPackages(), req.GetRunCommands())
	}

	if _, err := (cloudInitGenOptions{sshKeys: []string{filepath.Join(t.TempDir(), "missing.pub")}}).request("x"); err == nil {
		t.Fatal("expected a missing key file to fail")
	}
}

func TestCloudInitNewFlags(t *testing.T) {
	t.Run("--package splits on commas, --run does not", func(t *testing.T) {
		cmd := newCloudInitNewCommand(&globalFlags{})
		if err := cmd.ParseFlags([]string{"--package", "nginx,curl", "--package", "git", "--run", "echo a, b"}); err != nil {
			t.Fatal(err)
		}
		pkgs, _ := cmd.Flags().GetStringSlice("package")
		runs, _ := cmd.Flags().GetStringArray("run")
		if !slices.Equal(pkgs, []string{"nginx", "curl", "git"}) || !slices.Equal(runs, []string{"echo a, b"}) {
			t.Fatalf("packages %q, run %q", pkgs, runs)
		}
	})

	t.Run("--from excludes the generator flags", func(t *testing.T) {
		cmd := newCloudInitNewCommand(&globalFlags{})
		cmd.SetArgs([]string{"web", "--from", "x.yaml", "--user", "ubuntu"})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "from") {
			t.Fatalf("expected a mutual exclusion error, got %v", err)
		}
	})
}
