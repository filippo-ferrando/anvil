//go:build linux

package ephemeral

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
)

// fakeGUITools points guiToolsDir at a dir with stand-in tools.
func fakeGUITools(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range guiTools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := guiToolsDir
	guiToolsDir = func() string { return dir }
	t.Cleanup(func() { guiToolsDir = old })
	return dir
}

func TestValidPackage(t *testing.T) {
	for _, ok := range []string{"firefox-esr", "libreoffice", "g++", "python3.12", "lib32:i386"} {
		if err := ValidPackage(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-rf", "a b", "x;reboot", "$(id)", "../x"} {
		if ValidPackage(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestWithAppKey(t *testing.T) {
	tools := fakeGUITools(t)
	base := WarmSpec{Key: "base", Distro: "debian-13"}
	a, err := withApp(base, WarmParams{App: "gimp"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := withApp(base, WarmParams{App: "gimp", X11: true})
	c, _ := withApp(base, WarmParams{App: "inkscape"})
	if a.Key == base.Key || a.Key == b.Key || a.Key == c.Key || a.App != "gimp" || !b.X11 {
		t.Fatalf("keys %s %s %s", a.Key, b.Key, c.Key)
	}
	// New GUI tools make a new snapshot.
	if err := os.WriteFile(filepath.Join(tools, "waypipe"), []byte("v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if a2, _ := withApp(base, WarmParams{App: "gimp"}); a2.Key == a.Key {
		t.Fatal("key ignores the GUI tools")
	}
	if _, err := withApp(base, WarmParams{App: "x;y"}); err == nil {
		t.Fatal("bad package accepted")
	}
	os.Remove(filepath.Join(tools, "xwayland-satellite"))
	if _, err := withApp(base, WarmParams{App: "gimp"}); err == nil {
		t.Fatal("missing GUI tools accepted")
	}
}

func TestScriptsParse(t *testing.T) {
	for _, script := range []string{appInstallScript("firefox-esr", true), desktopFilesScript("gimp"), runtimeDirScript("debian"), runtimeDirScript("")} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%v: %s\n%s", err, out, script)
		}
	}
	if !strings.Contains(appInstallScript("o'x", false), `pkg='o'\''x'`) {
		t.Fatal("package not quoted")
	}
}

func TestPickEntry(t *testing.T) {
	entries := []appinfo.Entry{{Name: "Writer", Exec: "libreoffice --writer %U"}, {Name: "LibreOffice", Exec: "libreoffice %U"}, {Name: "Draw", Exec: "/usr/bin/lodraw"}}
	names := []string{"libreoffice-writer", "libreoffice-startcenter", "org.libreoffice.draw"}
	if e, _ := pickEntry("libreoffice", nil, entries, names); e.Name != "Writer" {
		t.Errorf("no match by name: %+v", e)
	}
	if e, _ := pickEntry("draw", nil, entries, names); e.Name != "Draw" {
		t.Errorf("reverse-DNS name: %+v", e)
	}
	if e, ok := pickEntry("x", []string{"lodraw"}, entries, names); !ok || e.Name != "Draw" {
		t.Errorf("by command: %+v", e)
	}
	if _, ok := pickEntry("x", []string{"other"}, entries, names); ok {
		t.Error("unrelated command matched")
	}
	if _, ok := pickEntry("x", nil, nil, nil); ok {
		t.Error("no entries matched")
	}
}

func TestAppArgv(t *testing.T) {
	argv := appArgv([]string{"gimp", "-n"}, true)
	if !slices.Equal(argv[:6], []string{"waypipe", "--vsock", "-s", "2:7100", "--xwls", "server"}) || !slices.Equal(argv[len(argv)-2:], []string{"gimp", "-n"}) {
		t.Fatalf("argv %q", argv)
	}
	if env := appEnv(true, []string{"A=1"}); !slices.Contains(env, "_JAVA_AWT_WM_NONREPARENTING=1") || env[len(env)-1] != "A=1" {
		t.Fatalf("env %q", env)
	}
}

const firefoxEntry = "[Desktop Entry]\nType=Application\nName=Firefox ESR\nExec=/usr/lib/firefox-esr/firefox-esr %u\nIcon=/usr/share/pixmaps/ff.png\n"

// fakeAppGuest answers RunApp's commands the way a debian clone with firefox-esr would,
// and writes hello through the clone's vsock listener when the app runs.
type fakeAppGuest struct {
	mu        sync.Mutex
	app       guestexec.Header
	delivered chan string // what the display got, waited on while the app "runs"
	received  string
}

func (g *fakeAppGuest) run(_ context.Context, clone Clone, r guestexec.Request) (int, error) {
	argv := r.Argv
	script := ""
	if len(argv) == 3 && argv[0] == "sh" {
		script = argv[2]
	}
	switch {
	case strings.Contains(script, "dpkg-query -L"):
		io.WriteString(r.Stdout, "/usr/share/applications/firefox-esr.desktop\n")
	case argv[0] == "head" && argv[len(argv)-1] == "/usr/share/applications/firefox-esr.desktop":
		io.WriteString(r.Stdout, firefoxEntry)
	case argv[0] == "head" && argv[len(argv)-1] == "/usr/share/pixmaps/ff.png":
		io.WriteString(r.Stdout, "PNG")
	case argv[0] == "head":
		return 1, nil
	case strings.HasPrefix(script, "install -d"):
	case argv[0] == "waypipe":
		g.mu.Lock()
		g.app = r.Header
		g.mu.Unlock()
		conn, err := net.Dial("unix", clone.(*fakeClone).vsock)
		if err != nil {
			return -1, err
		}
		io.WriteString(conn, "hello")
		conn.Close()
		g.received = <-g.delivered
		io.WriteString(r.Stderr, "app says bye\n")
		return 3, nil
	default:
		return 1, nil
	}
	return 0, nil
}

func TestRunApp(t *testing.T) {
	fakeGUITools(t)
	m, e := newTestManager(t)
	guest := &fakeAppGuest{delivered: make(chan string, 1)}
	m.guestRun = guest.run

	dir, err := os.MkdirTemp("", "display")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	display := filepath.Join(dir, "waypipe.sock")
	l, err := net.Listen("unix", display)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := guest.delivered
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(conn)
		got <- string(data)
	}()
	ready := make(chan struct{})
	close(ready)

	var events []Event
	err = m.RunApp(context.Background(), AppRequest{Distro: "debian-13", Package: "firefox-esr", Env: []string{"A=1"}, Display: display, DisplayReady: ready},
		func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if e.setups.Load() != 1 {
		t.Fatal("the warm build had no app setup")
	}
	var app *App
	var exit *Exit
	var output string
	for _, ev := range events {
		switch {
		case ev.App != nil:
			app = ev.App
		case ev.Exit != nil:
			exit = ev.Exit
		case ev.Output != nil:
			output += string(ev.Output.Data)
		}
	}
	if app == nil || app.Name != "Firefox ESR" || !app.FromEntry || string(app.Icon) != "PNG" || !slices.Equal(app.Command, []string{"/usr/lib/firefox-esr/firefox-esr"}) {
		t.Fatalf("app %+v", app)
	}
	if exit == nil || exit.Code != 3 || exit.Err != "" || output != "app says bye\n" {
		t.Fatalf("exit %+v output %q", exit, output)
	}
	if guest.received != "hello" {
		t.Fatalf("display got %q", guest.received)
	}
	if !slices.Contains(guest.app.Env, "A=1") || guest.app.Argv[len(guest.app.Argv)-1] != "/usr/lib/firefox-esr/firefox-esr" {
		t.Fatalf("app header %+v", guest.app)
	}
	if len(m.Clones()) != 0 || len(e.deleted) != 1 {
		t.Fatalf("clone left: %+v, deleted %v", m.Clones(), e.deleted)
	}
	warm, _ := m.ListWarm()
	if len(warm) != 1 || warm[0].App != "firefox-esr" {
		t.Fatalf("warm %+v", warm)
	}

	// info_only finds the app without running it, from the same snapshot.
	guest.app = guestexec.Header{}
	var info *App
	err = m.RunApp(context.Background(), AppRequest{Distro: "debian-13", Package: "firefox-esr", InfoOnly: true}, func(ev Event) {
		if ev.App != nil {
			info = ev.App
		}
	})
	if err != nil || info == nil || info.Name != "Firefox ESR" || guest.app.Argv != nil || e.builds.Load() != 1 {
		t.Fatalf("info %+v err %v app %+v builds %d", info, err, guest.app, e.builds.Load())
	}
}

func TestRunAppFallsBackToCommand(t *testing.T) {
	fakeGUITools(t)
	m, _ := newTestManager(t)
	m.guestRun = func(_ context.Context, _ Clone, r guestexec.Request) (int, error) {
		if r.Argv[0] == "sh" && strings.Contains(r.Argv[2], "command -v 'xterm'") {
			return 0, nil
		}
		return 0, nil // no desktop entries listed
	}
	var app *App
	err := m.RunApp(context.Background(), AppRequest{Distro: "alpine", Package: "xterm", InfoOnly: true}, func(ev Event) {
		if ev.App != nil {
			app = ev.App
		}
	})
	if err != nil || app == nil || app.FromEntry || !slices.Equal(app.Command, []string{"xterm"}) {
		t.Fatalf("app %+v err %v", app, err)
	}

	m.guestRun = func(_ context.Context, _ Clone, r guestexec.Request) (int, error) {
		if strings.Contains(r.Argv[2], "command -v 'nothing'") {
			return 1, nil
		}
		return 0, nil
	}
	var exit *Exit
	_ = m.RunApp(context.Background(), AppRequest{Distro: "alpine", Package: "nothing", InfoOnly: true}, func(ev Event) {
		if ev.Exit != nil {
			exit = ev.Exit
		}
	})
	if exit == nil || !strings.Contains(exit.Err, "name the command after --") {
		t.Fatalf("exit %+v", exit)
	}
}

func TestLineProgress(t *testing.T) {
	var got []string
	l := &lineProgress{prefix: "installing x: ", progress: func(s string) { got = append(got, s) }}
	io.WriteString(l, "Reading package lists...\n\x1b[1mUnpack")
	io.WriteString(l, "ing\x1b[0m libgtk\r\n\n")
	if len(got) != 1 || got[0] != "installing x: Reading package lists..." {
		t.Fatalf("got %q", got)
	}
	l.last = time.Time{} // past the throttle
	io.WriteString(l, "Setting up x\n")
	if got[len(got)-1] != "installing x: Setting up x" {
		t.Fatalf("got %q", got)
	}
}
