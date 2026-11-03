package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
)

func TestWaypipeArgs(t *testing.T) {
	ssh := []string{"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k"}
	got := waypipeArgs(ssh, "ubuntu@localhost", "/opt/anvil", true, []string{"ghidraRun", "--flag"})
	want := []string{"--remote-bin", "/opt/anvil/waypipe", "--xwls", "ssh",
		"-p", "2222", "-o", "StrictHostKeyChecking=accept-new", "-J", "me@far", "-i", "/k",
		"ubuntu@localhost", gui.WaitShellCommand([]string{"env", "_JAVA_AWT_WM_NONREPARENTING=1", "ghidraRun", "--flag"})}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := waypipeArgs(nil, "u@h", "/opt/anvil", false, []string{"app"}); slices.Contains(got, "--xwls") || got[len(got)-1] != gui.WaitShellCommand([]string{"app"}) {
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

func remoteContainerSession(relayDir, toolsDir string) *anvilv1.GUISession {
	return &anvilv1.GUISession{
		Instance: &anvilv1.Instance{Name: "web", Container: &anvilv1.ContainerSpec{ContainerId: "c0ffee"}},
		RelayDir: relayDir, X11Dir: relayDir + "/x11", HostToolsDir: toolsDir,
	}
}

func TestRemoteContainerGUIArgs(t *testing.T) {
	session := remoteContainerSession("/run/anvil/gui/01", "/usr/lib/anvil/gui")
	got, err := remoteContainerGUIArgs("me@far", session, "wayland-anvil-X", "bob", []string{"gedit", "it's.txt"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--remote-bin", "/usr/lib/anvil/gui/waypipe", "--display", "/run/anvil/gui/01/wayland-anvil-X",
		"ssh", "-o", "BatchMode=yes", "me@far"}
	if len(got) != len(want)+1 || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("got  %q\nwant %q + command line", got, want)
	}
	if slices.Contains(got, "--xwls") {
		t.Fatalf("a remote container never gets --xwls: %q", got)
	}

	// The command line must reach docker as these exact words once the remote shell parses it.
	dir := t.TempDir()
	socket := filepath.Join(dir, "wayland-anvil-X")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	argsOut := filepath.Join(dir, "args")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argsOut + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	line := strings.Replace(got[len(got)-1], "'/run/anvil/gui/01/wayland-anvil-X'", "'"+socket+"'", 1)
	cmd := exec.Command("sh", "-c", line)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running %s: %v: %s", line, err, out)
	}
	data, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatal(err)
	}
	wantDocker := append([]string{"exec", "-e", "XDG_RUNTIME_DIR=/run/anvil-gui", "-e", "WAYLAND_DISPLAY=wayland-anvil-X",
		"--user", "bob", "c0ffee"}, gui.WaitArgv([]string{"gedit", "it's.txt"})...)
	gotDocker := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	// The wait script has newlines of its own: compare it whole.
	if strings.Join(gotDocker, "\n") != strings.Join(wantDocker, "\n") {
		t.Fatalf("docker got  %q\nwant %q", gotDocker, wantDocker)
	}
	if st, err := os.Stat(socket); err != nil || st.Mode().Perm() != 0o777 {
		t.Fatalf("display socket mode %v, %v: want 0777 for any container user", st.Mode(), err)
	}
}

func TestRemoteContainerGUIArgs_Refusals(t *testing.T) {
	old := remoteContainerSession("/run/anvil/gui/01", "") // a daemon without host_tools_dir
	if _, err := remoteContainerGUIArgs("me@far", old, "wayland-anvil-X", "", []string{"app"}); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("old daemon: got %v", err)
	}
	noID := remoteContainerSession("/run/anvil/gui/01", "/usr/lib/anvil/gui")
	noID.Instance.Container.ContainerId = ""
	if _, err := remoteContainerGUIArgs("me@far", noID, "wayland-anvil-X", "", []string{"app"}); err == nil {
		t.Fatal("a container without an ID must be refused")
	}
}

func TestNewDisplayName(t *testing.T) {
	a, b := newDisplayName(), newDisplayName()
	if a == b || !strings.HasPrefix(a, "wayland-anvil-") {
		t.Fatalf("got %q and %q", a, b)
	}
}
