//go:build linux

package vm

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/qemu"
)

var (
	_ instance.GUIPreparer     = (*Backend)(nil)
	_ instance.GuestFileReader = (*Backend)(nil)
)

// xwaylandInstallTimeout bounds the first-use Xwayland install in the guest.
const xwaylandInstallTimeout = 15 * time.Minute

// guestPath is a full PATH, since the guest agent runs commands with a minimal one.
const guestPath = "export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// toolsAvailable reports whether this host has the GUI tools to share into VMs.
func toolsAvailable() bool {
	_, err := os.Stat(filepath.Join(config.GUIToolsDir(), "waypipe"))
	return err == nil
}

// startToolsShare starts a read-only virtiofsd for the GUI tools. A missing tools dir or a
// failed virtiofsd only costs GUI support, so the VM still boots.
func (b *Backend) startToolsShare(spec *instance.Spec) *qemu.Mount {
	if !toolsAvailable() {
		log.Printf("vm: %s: no GUI tools in %s, `anvil gui` won't work for it until a restart", spec.Name, config.GUIToolsDir())
		return nil
	}
	sock := virtiofsSocket(spec.ID, gui.ToolsTag)
	d, err := qemu.StartVirtiofsd(config.GUIToolsDir(), sock, true, virtiofsLog(spec.ID, gui.ToolsTag))
	if err != nil {
		log.Printf("vm: %s: sharing GUI tools: %v", spec.Name, err)
		return nil
	}
	b.trackVirtiofsd(spec.ID, gui.ToolsTag, d)
	return &qemu.Mount{Tag: gui.ToolsTag, SocketPath: sock}
}

// virtiofsToolsMount mounts a QEMU VM's tools share at the guest tools dir.
var virtiofsToolsMount = "mount -t virtiofs -o ro " + gui.ToolsTag + " " + gui.GuestToolsDir

// guiToolsScript mounts the tools (if they aren't yet) with mountCmd and puts them on the
// default PATH, where waypipe looks for xwayland-satellite.
func guiToolsScript(mountCmd string) string {
	d := gui.GuestToolsDir
	return strings.Join([]string{
		"set -e",
		guestPath,
		"if ! grep -qs ' " + d + " ' /proc/mounts; then",
		"  mkdir -p " + d,
		"  " + mountCmd,
		"fi",
		"mkdir -p /usr/local/bin",
		"for f in waypipe xwayland-satellite; do ln -sf " + d + "/$f /usr/local/bin/$f; done",
	}, "\n")
}

// xwaylandInstallScript installs Xwayland with whichever package manager the guest has.
// Alpine's xwayland package leaves out EGL and the keymap tools it needs.
func xwaylandInstallScript() string {
	return strings.Join([]string{
		"set -e",
		guestPath,
		"command -v Xwayland >/dev/null 2>&1 && exit 0",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  export DEBIAN_FRONTEND=noninteractive",
		"  apt-get update -q && apt-get install -y -q xwayland",
		"elif command -v dnf >/dev/null 2>&1; then dnf install -y -q xorg-x11-server-Xwayland",
		"elif command -v zypper >/dev/null 2>&1; then zypper --non-interactive -q install xwayland",
		"elif command -v pacman >/dev/null 2>&1; then pacman -Syu --noconfirm --needed xorg-xwayland",
		"elif command -v apk >/dev/null 2>&1; then apk add -q xwayland mesa-egl xkbcomp xkeyboard-config",
		"else echo 'no supported package manager, install Xwayland by hand' >&2; exit 1",
		"fi",
		"command -v Xwayland >/dev/null 2>&1",
	}, "\n")
}

// PrepareGUI mounts the GUI tools in the running guest and, for x11, installs Xwayland once.
func (b *Backend) PrepareGUI(ctx context.Context, spec *instance.Spec, x11 bool, progress func(status string)) (instance.GUISession, error) {
	if spec.VM == nil {
		return instance.GUISession{}, fmt.Errorf("vm: PrepareGUI called with a nil VMSpec")
	}
	_, running := b.lookup(spec.ID)
	if !running {
		return instance.GUISession{}, fmt.Errorf("vm: %s isn't running", spec.Name)
	}
	return b.prepareGUI(ctx, spec, virtiofsToolsMount, x11, progress)
}

// prepareGUI is PrepareGUI for a running VM, with mountCmd mounting its tools in the guest.
func (b *guestTracker) prepareGUI(ctx context.Context, spec *instance.Spec, mountCmd string, x11 bool, progress func(status string)) (instance.GUISession, error) {
	if spec.VM.NoGuestAgent {
		return instance.GUISession{}, fmt.Errorf("vm: %s was launched without the guest agent, which `anvil gui` needs", spec.Name)
	}
	if !toolsAvailable() {
		return instance.GUISession{}, fmt.Errorf("vm: GUI tools (waypipe, xwayland-satellite) aren't installed in %s on this host", config.GUIToolsDir())
	}

	if _, err := b.guestExec(ctx, spec.ID, guiToolsScript(mountCmd)); err != nil {
		return instance.GUISession{}, fmt.Errorf("vm: mounting the GUI tools in %s (restart it if it was started before the tools were installed): %w", spec.Name, err)
	}
	if x11 {
		if _, err := b.guestExec(ctx, spec.ID, guestPath+"\ncommand -v Xwayland"); err != nil {
			progress("installing Xwayland in " + spec.Name + " (first X11 use, this can take a few minutes)")
			ictx, cancel := context.WithTimeout(ctx, xwaylandInstallTimeout)
			defer cancel()
			if _, err := b.guestExec(ictx, spec.ID, xwaylandInstallScript()); err != nil {
				return instance.GUISession{}, fmt.Errorf("vm: installing Xwayland in %s: %w", spec.Name, err)
			}
		}
	}
	return instance.GUISession{GuestToolsDir: gui.GuestToolsDir}, nil
}

// ListGuestDir lists dir in the guest through the agent.
func (b *guestTracker) ListGuestDir(ctx context.Context, spec *instance.Spec, dir string) ([]string, error) {
	res, err := b.guestExec(ctx, spec.ID, "ls -1A -- "+shQuote(dir)+" 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("vm: listing %s in %s: %w", dir, spec.Name, err)
	}
	return strings.Fields(res.Stdout), nil
}

// ReadGuestFile reads path in the guest through the agent. A file the guest can't open counts as missing.
func (b *guestTracker) ReadGuestFile(ctx context.Context, spec *instance.Spec, path string) ([]byte, bool, error) {
	var data []byte
	var ok bool
	err := b.agentFor(spec.ID).Do(ctx, func(c *qemu.QGAConn) error {
		var err error
		data, ok, err = c.ReadFile(path)
		return err
	})
	if qemu.IsGuestError(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("vm: reading %s in %s: %w", path, spec.Name, err)
	}
	return data, ok, nil
}
