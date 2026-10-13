package relay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// X11SocketDir is where X servers (and X clients) put display sockets.
const X11SocketDir = "/tmp/.X11-unix"

// WaylandSocket returns the path of the host's Wayland socket, from
// $WAYLAND_DISPLAY (absolute, or relative to $XDG_RUNTIME_DIR).
func WaylandSocket() (string, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		return "", errors.New("no Wayland session: WAYLAND_DISPLAY is not set")
	}
	path := name
	if !filepath.IsAbs(name) {
		runtime := os.Getenv("XDG_RUNTIME_DIR")
		if runtime == "" {
			return "", errors.New("no Wayland session: XDG_RUNTIME_DIR is not set")
		}
		path = filepath.Join(runtime, name)
	}
	if st, err := os.Stat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("no Wayland socket at %s", path)
	}
	return path, nil
}

// X11Display returns the local X display number and socket from $DISPLAY.
func X11Display() (int, string, error) {
	n, err := ParseDisplay(os.Getenv("DISPLAY"))
	if err != nil {
		return 0, "", err
	}
	path := filepath.Join(X11SocketDir, "X"+strconv.Itoa(n))
	if st, err := os.Stat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
		return 0, "", fmt.Errorf("no X11 socket at %s (is XWayland running?)", path)
	}
	return n, path, nil
}

// ParseDisplay reads the display number out of a local $DISPLAY value
// (":0", ":1.0", "unix:2"). Remote displays (TCP) aren't supported.
func ParseDisplay(display string) (int, error) {
	if display == "" {
		return 0, errors.New("no X11 display: DISPLAY is not set")
	}
	host, rest, ok := strings.Cut(display, ":")
	if !ok || (host != "" && host != "unix") {
		return 0, fmt.Errorf("only a local X11 display is supported, got DISPLAY=%q", display)
	}
	num, _, _ := strings.Cut(rest, ".")
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid DISPLAY=%q", display)
	}
	return n, nil
}
