//go:build linux

package relay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDockerContainerReachesHostCompositor runs real Wayland and X11 clients in a
// container through the relay. Skips without docker or a Wayland session.
func TestDockerContainerReachesHostCompositor(t *testing.T) {
	if testing.Short() {
		t.Skip("pulls an image and packages")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable")
	}
	wayland, err := WaylandSocket()
	if err != nil {
		t.Skip(err)
	}

	dir, err := os.MkdirTemp("", "anvil-gui-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	x11Dir := filepath.Join(dir, "x11")
	if err := os.Mkdir(x11Dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, filepath.Join(dir, "wayland-0"), wayland) }()

	script := "apk add -q --no-cache wayland-utils >/dev/null && wayland-info | grep -m1 wl_compositor"
	args := []string{"run", "--rm", "-v", dir + ":/run/anvil-gui", "-v", x11Dir + ":/tmp/.X11-unix",
		"-e", "XDG_RUNTIME_DIR=/run/anvil-gui", "-e", "WAYLAND_DISPLAY=wayland-0"}

	hasX11 := false
	if n, sock, err := X11Display(); err == nil {
		hasX11 = true
		go func() { _ = Serve(ctx, filepath.Join(x11Dir, "X0"), sock) }()
		if ok, _ := WriteGuestXAuthority(XAuthorityPath(), n, filepath.Join(dir, "Xauthority"), 0); ok {
			args = append(args, "-e", "XAUTHORITY=/run/anvil-gui/Xauthority")
		}
		args = append(args, "-e", "DISPLAY=:0")
		script = "apk add -q --no-cache wayland-utils xdpyinfo >/dev/null && wayland-info | grep -m1 wl_compositor && xdpyinfo | grep -m1 'version number'"
	}
	time.Sleep(100 * time.Millisecond) // let the relays start listening
	args = append(args, "docker.io/library/alpine:3.22", "sh", "-c", script)

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("container run failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "wl_compositor") {
		t.Fatalf("wayland-info didn't see the compositor:\n%s", out)
	}
	if hasX11 && !strings.Contains(string(out), "version number") {
		t.Fatalf("xdpyinfo didn't reach the X server:\n%s", out)
	}
	t.Logf("%s", out)
}
