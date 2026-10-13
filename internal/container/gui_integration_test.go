//go:build linux

package container

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/relay"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// TestRealDockerGUIContainer creates a GUI-enabled container through DockerBackend and runs real
// Wayland and X11 clients in it through the relays. Skips without dockerd, a Wayland session or
// alpine:3.22 already pulled.
func TestRealDockerGUIContainer(t *testing.T) {
	client := docker.NewClient(docker.DefaultSocket)
	if ok, err := client.ImageExists(t.Context(), "alpine:3.22"); err != nil || !ok {
		t.Skip("dockerd not reachable or alpine:3.22 not pulled, skipping")
	}
	wayland, err := relay.WaylandSocket()
	if err != nil {
		t.Skip(err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not on PATH")
	}

	base, err := os.MkdirTemp("", "anvil-gui")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	old := guiRelayDir
	guiRelayDir = func(id string) string { return filepath.Join(base, id) }
	defer func() { guiRelayDir = old }()

	spec := &instance.Spec{
		ID: "guitest" + strconv.FormatInt(time.Now().UnixNano(), 36), Name: "gui-it", Kind: instance.KindContainer, GUI: true,
		Container: &instance.ContainerSpec{
			ImageRef: "alpine:3.22",
			Cmd: []string{"sh", "-c", "apk add -q --no-cache wayland-utils xdpyinfo && " +
				"mkdir -p /usr/share/applications /usr/share/icons/hicolor/scalable/apps && " +
				"printf '[Desktop Entry]\\nType=Application\\nName=Demo App\\nExec=/usr/bin/demo %%U\\nIcon=demo\\n' > /usr/share/applications/demo.desktop && " +
				"echo '<svg/>' > /usr/share/icons/hicolor/scalable/apps/real.svg && " +
				"ln -s real.svg /usr/share/icons/hicolor/scalable/apps/demo.svg && " +
				"touch /ready && sleep 600"},
		},
	}
	spec.Name = spec.ID
	b := &DockerBackend{Client: client}
	ctx := t.Context()
	if err := b.Create(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	defer b.Delete(context.Background(), spec)
	if err := b.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	session, err := b.PrepareGUI(ctx, spec, true, func(string) {})
	if err != nil {
		t.Fatal(err)
	}

	waitFor := func(cmd ...string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for exec.Command("docker", append([]string{"exec", spec.Container.ContainerID}, cmd...)...).Run() != nil {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %v", cmd)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	waitFor("test", "-e", "/ready")

	wr, err := relay.Listen(filepath.Join(session.RelayDir, "wayland-0"), wayland)
	if err != nil {
		t.Fatal(err)
	}
	go wr.Serve()
	defer wr.Close()
	script := "wayland-info | grep -m1 wl_compositor"
	if _, xsock, err := relay.X11Display(); err == nil {
		xr, err := relay.Listen(filepath.Join(session.X11Dir, "X0"), xsock)
		if err != nil {
			t.Fatal(err)
		}
		go xr.Serve()
		defer xr.Close()
		script += " && DISPLAY=:0 xdpyinfo | grep -m1 'version number'"
	}

	out, err := exec.Command("docker", "exec", "-e", "XDG_RUNTIME_DIR="+gui.ContainerRuntimeDir, "-e", "WAYLAND_DISPLAY=wayland-0",
		"--user", "1234", spec.Container.ContainerID, "sh", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "wl_compositor") {
		t.Fatalf("app in container (as an unrelated uid) couldn't reach the display: %v\n%s", err, out)
	}
	t.Logf("%s", out)

	app, found, err := appinfo.Find(ctx, containerFS{b, spec}, "demo")
	if err != nil || !found || app.Name != "Demo App" || app.IconExt != "svg" || strings.TrimSpace(string(app.Icon)) != "<svg/>" {
		t.Fatalf("app lookup: %+v %v %v", app, found, err)
	}

	// The relay directory goes away with the container.
	if err := b.Delete(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session.RelayDir); !os.IsNotExist(err) {
		t.Fatalf("relay dir still there: %v", err)
	}
}

type containerFS struct {
	b    *DockerBackend
	spec *instance.Spec
}

func (f containerFS) List(ctx context.Context, dir string) ([]string, error) {
	return f.b.ListGuestDir(ctx, f.spec, dir)
}

func (f containerFS) Read(ctx context.Context, p string) ([]byte, bool, error) {
	return f.b.ReadGuestFile(ctx, f.spec, p)
}
