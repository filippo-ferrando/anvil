package ephemeral

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
)

// DefaultAppMemoryMiB is a GUI app clone's memory: browsers and office suites need more than a shell.
const DefaultAppMemoryMiB = 2048

// AppPort is the host vsock port a clone's waypipe connects to.
const AppPort = 7100

// appSetupVersion is part of an app snapshot's key: a change to the setup below builds new ones.
const appSetupVersion = "app-v1"

// appInstallTimeout bounds the package install in a warm build.
const appInstallTimeout = 30 * time.Minute

// appRuntimeDir is the app's XDG_RUNTIME_DIR, where waypipe puts its Wayland socket.
const appRuntimeDir = "/run/anvil-app"

// guiTools are the files of the GUI tools dir an app snapshot gets in /usr/local/bin.
var guiTools = []string{"waypipe", "xwayland-satellite"}

var guiToolsDir = config.GUIToolsDir

const guestPath = "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_:@-]*$`)

// ValidPackage checks that name looks like a package name of any supported package manager.
func ValidPackage(name string) error {
	if !packageName.MatchString(name) {
		return fmt.Errorf("%q is not a package name", name)
	}
	return nil
}

// AppRequest is one `anvil gui --ephemeral` run: a GUI app in a clone of a warm snapshot
// that has its package installed.
type AppRequest struct {
	Distro, Arch string
	Package      string
	Command      []string // empty for the package's own desktop entry
	Env          []string
	X11          bool
	CPUs         int
	MemoryMiB    int64 // 0 for DefaultAppMemoryMiB
	// InfoOnly builds the warm snapshot and reports the app, without running it.
	InfoOnly bool
	// Display is the host socket of the client's `waypipe client`, usable once DisplayReady closes.
	Display      string
	DisplayReady <-chan struct{}
}

// App is what an app run starts, or what an InfoOnly run found.
type App struct {
	CloneID, Distro string
	Name            string
	FromEntry       bool // Name and the icon come from the package's desktop entry
	Command         []string
	Icon            []byte
	IconExt         string
	IconSize        int
}

// withApp turns the engine's spec for p into the spec of p's app snapshot, with its own key.
func withApp(s WarmSpec, p WarmParams) (WarmSpec, error) {
	if err := ValidPackage(p.App); err != nil {
		return WarmSpec{}, err
	}
	s.App, s.X11 = p.App, p.X11
	h := sha256.New()
	parts := []string{appSetupVersion, s.Key, s.App, strconv.FormatBool(s.X11)}
	for _, name := range guiTools {
		sum, err := fileSum(filepath.Join(guiToolsDir(), name))
		if err != nil {
			return WarmSpec{}, fmt.Errorf("GUI tools (waypipe, xwayland-satellite) aren't installed in %s: %w", guiToolsDir(), err)
		}
		parts = append(parts, sum)
	}
	for _, part := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	s.Key = hex.EncodeToString(h.Sum(nil))[:16]
	return s, nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// appSetup installs the GUI tools and s's package (plus Xwayland for X11) in a warm build VM.
func appSetup(s WarmSpec) Setup {
	return func(ctx context.Context, dial Dialer, progress func(string)) error {
		progress("adding the GUI tools")
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeToolsTar(pw, guiToolsDir())) }()
		install := "install -m 0755 " + strings.Join(guiTools, " ") + " /usr/local/bin/ && rm -rf \"$PWD\""
		if _, err := guestOutput(ctx, dial, guestexec.Request{Header: guestexec.Header{Argv: []string{"sh", "-c", install}}, Upload: pr}); err != nil {
			return fmt.Errorf("adding the GUI tools: %w", err)
		}
		progress("installing " + s.App + " with the distro's package manager, once for the warm snapshot")
		ictx, cancel := context.WithTimeout(ctx, appInstallTimeout)
		defer cancel()
		req := shRequest(appInstallScript(s.App, s.X11))
		lines := &lineProgress{prefix: "installing " + s.App + ": ", progress: progress}
		req.Stdout, req.Stderr = lines, lines
		if _, err := guestOutput(ictx, dial, req); err != nil {
			return fmt.Errorf("installing %s: %w", s.App, err)
		}
		return nil
	}
}

// writeToolsTar writes the GUI tools as a tar stream for an upload.
func writeToolsTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	for _, name := range guiTools {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	return tw.Close()
}

// appInstallScript installs pkg (and Xwayland with x11) with the guest's package manager,
// unless it is already there. Alpine's xwayland package leaves out EGL and the keymap tools.
func appInstallScript(pkg string, x11 bool) string {
	return strings.Join([]string{
		"set -e",
		guestPath,
		"pkg=" + shQuote(pkg),
		"x11=" + strconv.FormatBool(x11),
		"if command -v apt-get >/dev/null 2>&1; then",
		"  export DEBIAN_FRONTEND=noninteractive",
		`  has() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q ' installed$'; }`,
		`  add() { apt-get update -q && apt-get install -y -q "$@"; }`,
		"  xw=xwayland",
		"elif command -v dnf >/dev/null 2>&1; then",
		`  has() { rpm -q --whatprovides "$1" >/dev/null 2>&1; }`,
		`  add() { dnf install -y -q "$@"; }`,
		"  xw=xorg-x11-server-Xwayland",
		"elif command -v zypper >/dev/null 2>&1; then",
		`  has() { rpm -q --whatprovides "$1" >/dev/null 2>&1; }`,
		`  add() { zypper --non-interactive -q install "$@"; }`,
		"  xw=xwayland",
		"elif command -v pacman >/dev/null 2>&1; then",
		`  has() { pacman -Q "$1" >/dev/null 2>&1; }`,
		`  add() { pacman -Syu --noconfirm --needed "$@"; }`,
		"  xw=xorg-xwayland",
		"elif command -v apk >/dev/null 2>&1; then",
		`  has() { apk info -e "$1" >/dev/null 2>&1; }`,
		`  add() { apk add -q "$@"; }`,
		`  xw="xwayland mesa-egl xkbcomp xkeyboard-config"`,
		"else",
		"  echo 'no supported package manager (apt, dnf, zypper, pacman, apk) in this image' >&2; exit 1",
		"fi",
		"want=",
		`has "$pkg" || want="$pkg"`,
		`if [ "$x11" = true ] && ! command -v Xwayland >/dev/null 2>&1; then want="$want $xw"; fi`,
		`if [ -n "$want" ]; then add $want; fi`,
		`has "$pkg" || { echo "$pkg is not installed after the install" >&2; exit 1; }`,
	}, "\n")
}

// desktopFilesScript prints the desktop entries pkg installed, one path per line.
func desktopFilesScript(pkg string) string {
	return strings.Join([]string{
		guestPath,
		"pkg=" + shQuote(pkg),
		"{",
		`if command -v dpkg-query >/dev/null 2>&1; then dpkg-query -L "$pkg"`,
		`elif command -v rpm >/dev/null 2>&1; then rpm -ql "$pkg"`,
		`elif command -v pacman >/dev/null 2>&1; then pacman -Qlq "$pkg"`,
		`elif command -v apk >/dev/null 2>&1; then apk info -qL "$pkg" | sed 's|^|/|'`,
		"fi",
		`} 2>/dev/null | grep -E '^/usr/(local/)?share/applications/[^/]+\.desktop$' || true`,
	}, "\n")
}

// resolveApp finds what to run for pkg: command when given, else the Exec of the package's
// own desktop entry, else a command named like the package.
func (m *Manager) resolveApp(ctx context.Context, clone Clone, pkg string, command []string) (App, error) {
	fs := cloneFS{m: m, clone: clone}
	out, err := m.output(ctx, clone, desktopFilesScript(pkg))
	if err != nil {
		return App{}, fmt.Errorf("listing the desktop entries of %s: %w", pkg, err)
	}
	var entries []appinfo.Entry
	var names []string
	for _, p := range strings.Fields(out) {
		data, ok, err := fs.Read(ctx, p)
		if err != nil {
			return App{}, err
		}
		if e, ok2 := appinfo.Parse(data); ok && ok2 {
			entries, names = append(entries, e), append(names, strings.TrimSuffix(path.Base(p), ".desktop"))
		}
	}
	entry, found := pickEntry(pkg, command, entries, names)
	app := App{Name: pkg, Command: command}
	if found {
		info, _, err := appinfo.Load(ctx, fs, entry)
		if err != nil {
			return App{}, err
		}
		app.Name, app.Icon, app.IconExt, app.IconSize, app.FromEntry = info.Name, info.Icon, info.IconExt, info.IconSize, true
		if len(command) == 0 {
			app.Command = appinfo.SplitExec(entry.Exec)
		}
	}
	if len(app.Command) == 0 {
		if _, err := m.output(ctx, clone, guestPath+"\ncommand -v "+shQuote(pkg)); err != nil {
			return App{}, fmt.Errorf("%s has no desktop entry and no command of that name, name the command after --", pkg)
		}
		app.Command = []string{pkg}
	}
	return app, nil
}

// pickEntry picks the desktop entry for an app: the one running command when given, else
// the one named like the package, else the first.
func pickEntry(pkg string, command []string, entries []appinfo.Entry, names []string) (appinfo.Entry, bool) {
	if len(entries) == 0 {
		return appinfo.Entry{}, false
	}
	if len(command) > 0 {
		for _, e := range entries {
			if appinfo.Program(e.Exec) == path.Base(command[0]) {
				return e, true
			}
		}
		return appinfo.Entry{}, false
	}
	for i, n := range names {
		if n == pkg || strings.HasSuffix(n, "."+pkg) {
			return entries[i], true
		}
	}
	return entries[0], true
}

// cloneFS reads a clone's files through anvil-guest, for appinfo.
type cloneFS struct {
	m     *Manager
	clone Clone
}

func (f cloneFS) List(ctx context.Context, dir string) ([]string, error) {
	out, err := f.m.output(ctx, f.clone, "ls -1A -- "+shQuote(dir)+" 2>/dev/null || true")
	return strings.Fields(out), err
}

func (f cloneFS) Read(ctx context.Context, p string) ([]byte, bool, error) {
	var out strings.Builder
	code, err := f.m.guestRun(ctx, f.clone, guestexec.Request{
		Header: guestexec.Header{Argv: []string{"head", "-c", strconv.Itoa(appinfo.MaxIconBytes + 1), "--", p}}, Stdout: &out,
	})
	if err != nil {
		return nil, false, err
	}
	return []byte(out.String()), code == 0, nil
}

// appArgv runs command as the GUI app under waypipe, which connects to the host over vsock,
// in a D-Bus session when the guest has one to offer.
func appArgv(command []string, x11 bool) []string {
	argv := []string{"waypipe", "--vsock", "-s", "2:" + strconv.Itoa(AppPort)}
	if x11 {
		argv = append(argv, "--xwls")
	}
	argv = append(argv, "server", "--", "sh", "-c",
		`if command -v dbus-run-session >/dev/null 2>&1; then exec dbus-run-session -- "$@"; fi; exec "$@"`, "sh")
	return append(argv, command...)
}

// appEnv is the app's environment; Java windows stay blank under xwayland-satellite without
// _JAVA_AWT_WM_NONREPARENTING.
func appEnv(x11 bool, extra []string) []string {
	env := []string{"XDG_RUNTIME_DIR=" + appRuntimeDir}
	if x11 {
		env = append(env, "_JAVA_AWT_WM_NONREPARENTING=1")
	}
	return append(env, extra...)
}

// runtimeDirScript makes the app's XDG_RUNTIME_DIR, owned by user (root when empty).
func runtimeDirScript(user string) string {
	if user == "" {
		return "install -d -m 0700 " + appRuntimeDir
	}
	u := shQuote(user)
	return `install -d -m 0700 -o ` + u + ` -g "$(id -g ` + u + `)" ` + appRuntimeDir
}

// RunApp runs a GUI app in a clone restored from a warm snapshot with its package installed,
// building that snapshot first when there is none. Its windows show through the client's
// waypipe at r.Display. The clone is deleted at the end; a cancelled ctx deletes it too.
func (m *Manager) RunApp(ctx context.Context, r AppRequest, emit func(Event)) error {
	if err := ValidPackage(r.Package); err != nil {
		return err
	}
	if !r.InfoOnly && (r.Display == "" || r.DisplayReady == nil) {
		return errors.New("no display to show the app on")
	}
	e, err := m.engine()
	if err != nil {
		return err
	}
	if r.MemoryMiB <= 0 {
		r.MemoryMiB = DefaultAppMemoryMiB
	}
	id := "eph-" + ulid.Make().String()
	distro := r.Distro
	progress := func(msg string) { emit(Event{Progress: &Progress{CloneID: id, Distro: distro, Message: msg}}) }
	fail := func(err error) error {
		emit(Event{Exit: &Exit{CloneID: id, Distro: distro, Code: -1, Err: err.Error()}})
		return nil
	}
	_, spec, err := m.Warm(ctx, WarmParams{Distro: r.Distro, Arch: r.Arch, CPUs: r.CPUs, MemoryMiB: r.MemoryMiB, App: r.Package, X11: r.X11}, false, progress)
	if err != nil {
		return fail(err)
	}
	progress("restoring a clone")
	start := time.Now()
	clone, err := e.RestoreClone(ctx, id, spec, m.Dir(spec.Key))
	if err != nil {
		return fail(err)
	}
	st := &cloneState{id: id, distro: distro, warmKey: spec.Key, startedAt: start, clone: clone}
	m.mu.Lock()
	m.clones[id] = st
	m.mu.Unlock()
	defer m.deleteClone(context.WithoutCancel(ctx), id)

	app, err := m.resolveApp(ctx, clone, r.Package, r.Command)
	if err != nil {
		return fail(err)
	}
	app.CloneID, app.Distro = id, distro
	emit(Event{App: &app})
	if r.InfoOnly {
		emit(Event{Exit: &Exit{CloneID: id, Distro: distro, Duration: time.Since(start)}})
		return nil
	}

	if _, err := m.output(ctx, clone, runtimeDirScript(spec.User)); err != nil {
		return fail(fmt.Errorf("making the app's runtime dir: %w", err))
	}
	l, err := clone.Listen(AppPort)
	if err != nil {
		return fail(fmt.Errorf("listening for the clone's waypipe: %w", err))
	}
	defer l.Close()
	select {
	case <-r.DisplayReady:
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	go forwardDisplay(l, r.Display)

	progress("running: " + app.Name)
	code, runErr := m.guestRun(ctx, clone, guestexec.Request{
		Header: guestexec.Header{Argv: appArgv(app.Command, r.X11), Env: appEnv(r.X11, r.Env), User: spec.User},
		Stdout: writerFunc(func(p []byte) { emit(Event{Output: &Output{CloneID: id, Distro: distro, Data: p}}) }),
		Stderr: writerFunc(func(p []byte) { emit(Event{Output: &Output{CloneID: id, Distro: distro, Stderr: true, Data: p}}) }),
	})
	st.mu.Lock()
	st.exited, st.exitCode = true, code
	st.mu.Unlock()
	exit := &Exit{CloneID: id, Distro: distro, Code: code, Duration: time.Since(start)}
	if runErr != nil {
		exit.Code, exit.Err = -1, runErr.Error()
	}
	emit(Event{Exit: exit})
	return nil
}

// forwardDisplay joins every connection the clone's waypipe opens to the client's waypipe socket.
func forwardDisplay(l net.Listener, display string) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			peer, err := net.Dial("unix", display)
			if err != nil {
				log.Printf("ephemeral: reaching the client's waypipe: %v", err)
				return
			}
			defer peer.Close()
			splice(conn, peer)
		}()
	}
}

// splice copies both ways until both sides are done, passing on each half-close.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go half(a, b)
	go half(b, a)
	<-done
	<-done
}

// output runs script with sh in clone and returns its output, failing on a non-zero exit.
func (m *Manager) output(ctx context.Context, clone Clone, script string) (string, error) {
	return collectOutput(shRequest(script), func(r guestexec.Request) (int, error) { return m.guestRun(ctx, clone, r) })
}

func shRequest(script string) guestexec.Request {
	return guestexec.Request{Header: guestexec.Header{Argv: []string{"sh", "-c", script}}}
}

// guestOutput is output for r on the VM behind dial.
func guestOutput(ctx context.Context, dial Dialer, r guestexec.Request) (string, error) {
	return collectOutput(r, func(r guestexec.Request) (int, error) {
		conn, err := dial(ctx)
		if err != nil {
			return -1, fmt.Errorf("reaching anvil-guest: %w", err)
		}
		return guestexec.Run(ctx, conn, r)
	})
}

// collectOutput runs r with run and returns its output, with the tail of its error output in
// the error on a non-zero exit.
func collectOutput(r guestexec.Request, run func(guestexec.Request) (int, error)) (string, error) {
	var stdout, stderr strings.Builder
	r.Stdout, r.Stderr = tee(&stdout, r.Stdout), tee(&stderr, r.Stderr)
	code, err := run(r)
	if err == nil && code != 0 {
		msg := tailLines(stderr.String(), 15)
		if msg == "" {
			msg = tailLines(stdout.String(), 15)
		}
		err = fmt.Errorf("exit status %d: %s", code, msg)
	}
	return stdout.String(), err
}

// tee writes to b, and to w as well when there is one.
func tee(b *strings.Builder, w io.Writer) io.Writer {
	if w == nil {
		return b
	}
	return io.MultiWriter(b, w)
}

// lineProgress reports the last line written to it as a progress status, at most every
// lineProgressEvery, so a long package install shows what it is doing.
type lineProgress struct {
	prefix   string
	progress func(string)

	mu   sync.Mutex
	buf  []byte
	last time.Time
}

const lineProgressEvery = 250 * time.Millisecond

// maxProgressLine cuts long lines, which a terminal status line can't show anyway.
const maxProgressLine = 160

func (l *lineProgress) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexAny(l.buf, "\r\n")
		if i < 0 {
			break
		}
		line := ansiEscape.ReplaceAllString(string(l.buf[:i]), "")
		line = strings.TrimSpace(strings.Map(printable, line))
		l.buf = l.buf[i+1:]
		if line == "" || time.Since(l.last) < lineProgressEvery {
			continue
		}
		if r := []rune(line); len(r) > maxProgressLine {
			line = string(r[:maxProgressLine]) + "…"
		}
		l.last = time.Now()
		l.progress(l.prefix + line)
	}
	return len(p), nil
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// printable drops control characters, e.g. the colors some package managers print.
func printable(r rune) rune {
	if r < ' ' && r != '\t' || r == 0x7f {
		return -1
	}
	return r
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// shQuote quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
