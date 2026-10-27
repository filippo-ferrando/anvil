package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestExecValueRoundTrip(t *testing.T) {
	args := []string{"/usr/bin/anvil", "gui", "dev", "--", "app", "a b", `q"x`, "50%", "$HOME", `back\slash`}
	got := execValue(args)
	want := `/usr/bin/anvil gui dev -- app "a b" "q\\"x" 50%% "\\$HOME" "back\\\\slash"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if back := splitExec(got); !slices.Equal(back, args) {
		t.Fatalf("round trip %q", back)
	}
}

func TestDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	if d, _ := DataHome(); d != "/data" {
		t.Fatalf("got %s", d)
	}
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/me")
	if d, _ := DataHome(); d != "/home/me/.local/share" {
		t.Fatalf("got %s", d)
	}
}

func TestWriteListRemove(t *testing.T) {
	data := t.TempDir()
	info := &anvilv1.GUIAppInfoReply{Found: true, AppName: "Ghidra", Icon: []byte("png"), IconExt: "png"}
	path, err := Write(data, Spec{Bin: "/usr/bin/anvil", Remote: "me@far", Instance: "re-lab", Command: []string{"ghidraRun"}, X11: true}, info)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(data, "applications", "anvil-re-lab-ghidrarun.desktop") {
		t.Fatalf("path %s", path)
	}
	content, _ := os.ReadFile(path)
	iconPath := filepath.Join(data, "anvil", "icons", "anvil-re-lab-ghidrarun.png")
	for _, want := range []string{
		"Name=Ghidra (re-lab)\n",
		"Exec=/usr/bin/anvil --remote me@far gui --detach --x11 re-lab -- ghidraRun\n",
		"Icon=" + iconPath + "\n",
		"X-Anvil-Instance=re-lab\n",
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
	if icon, _ := os.ReadFile(iconPath); string(icon) != "png" {
		t.Fatalf("icon %q", icon)
	}

	// No icon from the instance: the generic one.
	path2, _ := Write(data, Spec{Bin: "/a", Instance: "dev", Command: []string{"/usr/bin/xterm", "-e", "top"}, Title: "Top"}, &anvilv1.GUIAppInfoReply{})
	content2, _ := os.ReadFile(path2)
	if !strings.Contains(string(content2), "Icon=application-x-executable\n") || !strings.Contains(string(content2), "Name=Top (dev)\n") {
		t.Fatalf("content:\n%s", content2)
	}

	exports, err := List(data)
	if err != nil || len(exports) != 2 {
		t.Fatalf("%+v %v", exports, err)
	}
	if e := exports[0]; e.Instance != "dev" || e.App != "xterm" || e.Command != "/usr/bin/xterm -e top" || e.Name != "Top" {
		t.Fatalf("first export %+v", e)
	}
	if want := []string{"/a", "gui", "--detach", "dev", "--", "/usr/bin/xterm", "-e", "top"}; !slices.Equal(exports[0].Exec, want) {
		t.Fatalf("exec %q", exports[0].Exec)
	}

	removed, err := Remove(data, "re-lab", "ghidrarun")
	if err != nil || removed != path {
		t.Fatalf("remove: %s %v", removed, err)
	}
	if _, err := os.Stat(iconPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("icon left behind: %v", err)
	}
	if _, err := Remove(data, "re-lab", "ghidrarun"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second remove: %v", err)
	}
	if exports, _ := List(data); len(exports) != 1 {
		t.Fatalf("after remove %+v", exports)
	}
}

func TestListEmpty(t *testing.T) {
	exports, err := List(t.TempDir())
	if err != nil || len(exports) != 0 {
		t.Fatalf("%+v %v", exports, err)
	}
}
