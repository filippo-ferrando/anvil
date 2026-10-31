package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSetup(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = os.MkdirAll("ci", 0o755)
	_ = os.WriteFile("ci/setup.sh", []byte("apt-get install -y make\n"), 0o755)
	_ = os.WriteFile("go.sum", []byte("sums"), 0o644)

	if s, err := LoadSetup("", nil); s != nil || err != nil {
		t.Errorf("no script = %v, %v", s, err)
	}
	s, err := LoadSetup("ci/setup.sh", []string{"go.sum", "./ci/../go.sum"})
	if err != nil {
		t.Fatal(err)
	}
	if s.GetName() != "ci/setup.sh" || string(s.GetScript()) != "apt-get install -y make\n" || len(s.GetFiles()) != 2 ||
		s.GetFiles()[0].GetName() != "go.sum" || s.GetFiles()[1].GetName() != "go.sum" || string(s.GetFiles()[0].GetData()) != "sums" {
		t.Errorf("setup = %+v", s)
	}
	for _, bad := range []struct {
		script string
		files  []string
		want   string
	}{
		{"", []string{"go.sum"}, "needs --setup"},
		{"missing.sh", nil, "reading the setup script"},
		{"ci/setup.sh", []string{"../x"}, "inside the current directory"},
		{"ci/setup.sh", []string{filepath.Join(dir, "go.sum")}, "inside the current directory"},
		{"ci/setup.sh", []string{"nope"}, "reading key file"},
	} {
		if _, err := LoadSetup(bad.script, bad.files); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("LoadSetup(%q, %q) = %v, want %q", bad.script, bad.files, err, bad.want)
		}
	}
	_ = os.WriteFile("big", make([]byte, MaxSetupBytes), 0o644)
	if _, err := LoadSetup("ci/setup.sh", []string{"big"}); err == nil {
		t.Error("an oversized setup was accepted")
	}
}
