package cloudinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestXorrisoBuilderBuild(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not installed, skipping")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "seed.iso")

	b := NewBuilder()
	err := b.Build(Seed{
		UserData: "#cloud-config\nhostname: anvil-test\n",
		MetaData: "instance-id: test-instance\nlocal-hostname: anvil-test\n",
	}, out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("expected output ISO to exist: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected non-empty ISO file")
	}
}

func TestXorrisoBuilderRejectsEmptyUserData(t *testing.T) {
	b := NewBuilder()
	err := b.Build(Seed{MetaData: "x"}, filepath.Join(t.TempDir(), "seed.iso"))
	if err == nil {
		t.Error("expected an error for empty UserData")
	}
}

func TestXorrisoBuilderRejectsEmptyMetaData(t *testing.T) {
	b := NewBuilder()
	err := b.Build(Seed{UserData: "x"}, filepath.Join(t.TempDir(), "seed.iso"))
	if err == nil {
		t.Error("expected an error for empty MetaData")
	}
}
