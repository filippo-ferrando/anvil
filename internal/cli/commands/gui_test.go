package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
)

func TestWaypipeArgs(t *testing.T) {
	ssh := []string{"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k"}
	got := waypipeArgs(ssh, "ubuntu@localhost", "/opt/anvil", true, []string{"ghidraRun", "--flag"})
	want := []string{"--remote-bin", "/opt/anvil/waypipe", "--xwls", "ssh",
		"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k",
		"ubuntu@localhost", "env", "_JAVA_AWT_WM_NONREPARENTING=1", "ghidraRun", "--flag"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := waypipeArgs(nil, "u@h", "/opt/anvil", false, []string{"app"}); slices.Contains(got, "--xwls") || slices.Contains(got, "env") {
		t.Fatalf("x11 settings without x11: %q", got)
	}
}

func TestWithoutDetach(t *testing.T) {
	got := withoutDetach([]string{"--remote", "h", "gui", "--detach", "--x11", "dev", "--", "app", "--detach"})
	want := []string{"--remote", "h", "gui", "--x11", "dev", "--", "app", "--detach"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestGUILogPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/state")
	if p, _ := guiLogPath("dev"); p != "/state/anvil/gui/dev.log" {
		t.Fatalf("got %s", p)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/me")
	if p, _ := guiLogPath("dev"); p != filepath.Join("/home/me", ".local/state/anvil/gui/dev.log") {
		t.Fatalf("got %s", p)
	}
}

func TestDesktopExec(t *testing.T) {
	got := desktopExec([]string{"/usr/bin/anvil", "gui", "dev", "--", "app", "a b", `q"x`, "50%", "$HOME"})
	want := `/usr/bin/anvil gui dev -- app "a b" "q\\"x" 50%% "\\$HOME"`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// Reading it back gives the original arguments, field codes aside.
	back := appinfo.SplitExec(strings.ReplaceAll(strings.ReplaceAll(got, `\\`, `\`), "%%", "%"))
	if !slices.Equal(back, []string{"/usr/bin/anvil", "gui", "dev", "--", "app", "a b", `q"x`, "50%", "$HOME"}) {
		t.Fatalf("round trip %q", back)
	}
}

func TestWriteAndListGUIExports(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	info := &anvilv1.GUIAppInfoReply{Found: true, AppName: "Ghidra", Icon: []byte("png"), IconExt: "png"}
	path, err := writeGUIExport("/usr/bin/anvil", "me@far", "re-lab", []string{"ghidraRun"}, guiOptions{x11: true}, "", "", info)
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
	path2, _ := writeGUIExport("/a", "", "dev", []string{"/usr/bin/xterm", "-e", "top"}, guiOptions{}, "Top", "", &anvilv1.GUIAppInfoReply{})
	content2, _ := os.ReadFile(path2)
	if !strings.Contains(string(content2), "Icon=application-x-executable\n") || !strings.Contains(string(content2), "Name=Top (dev)\n") {
		t.Fatalf("content:\n%s", content2)
	}

	exports, err := listGUIExports(filepath.Join(data, "applications"))
	if err != nil || len(exports) != 2 {
		t.Fatalf("%+v %v", exports, err)
	}
	if exports[0].Instance != "dev" || exports[0].App != "xterm" || exports[0].Command != "/usr/bin/xterm -e top" {
		t.Fatalf("first export %+v", exports[0])
	}
}
