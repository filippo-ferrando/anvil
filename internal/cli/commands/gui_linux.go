package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/relay"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

const (
	// guiFirstConnectWait is how long to wait for an app whose launcher already exited
	// (ghidraRun starts Java in the background) to open its first window.
	guiFirstConnectWait = 15 * time.Second
	// guiIdleGrace keeps the relay up briefly after the last window closes, for apps that reconnect.
	guiIdleGrace = 2 * time.Second
)

// checkLocalDisplay fails early, before anything starts, when this session has no display to use.
func checkLocalDisplay(x11 bool) error {
	if _, err := relay.WaylandSocket(); err != nil {
		return fmt.Errorf("anvil gui needs a Wayland desktop session: %w", err)
	}
	if x11 {
		if _, _, err := relay.X11Display(); err != nil {
			return fmt.Errorf("--x11 needs XWayland on this desktop: %w", err)
		}
	}
	return nil
}

// runContainerGUI relays this session's display sockets into the container's GUI directory,
// then runs command there with docker exec.
func runContainerGUI(ctx context.Context, inst *anvilv1.Instance, session *anvilv1.GUISession, opts guiOptions, command []string) error {
	spec := inst.GetContainer()
	if spec.GetContainerId() == "" {
		return fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}
	bin, err := client.ContainerBin(spec.GetEngine())
	if err != nil {
		return err
	}

	env, relays, cleanup, err := startGUIRelays(session.GetRelayDir(), session.GetX11Dir(), opts.x11)
	defer cleanup()
	if err != nil {
		return err
	}

	args := []string{"exec"}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	if opts.user != "" {
		args = append(args, "--user", opts.user)
	}
	args = append(args, spec.GetContainerId())
	args = append(args, command...)

	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	runErr := cmd.Run()
	waitForGUIApp(ctx, relays)
	return runErr
}

// startGUIRelays starts this session's relays in relayDir (and x11Dir), returning the
// environment for the app. cleanup is always safe to call.
func startGUIRelays(relayDir, x11Dir string, x11 bool) (env []string, relays []*relay.Relay, cleanup func(), err error) {
	var undo []func()
	cleanup = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}

	wayland, err := relay.WaylandSocket()
	if err != nil {
		return nil, nil, cleanup, err
	}
	n, release, err := relay.Claim(relayDir, "wayland-")
	if err != nil {
		return nil, nil, cleanup, err
	}
	undo = append(undo, release)
	name := "wayland-" + strconv.Itoa(n)
	wr, err := relay.Listen(filepath.Join(relayDir, name), wayland)
	if err != nil {
		return nil, nil, cleanup, err
	}
	go func() { _ = wr.Serve() }()
	undo = append(undo, wr.Close)
	relays = append(relays, wr)
	env = []string{"XDG_RUNTIME_DIR=" + gui.ContainerRuntimeDir, "WAYLAND_DISPLAY=" + name}
	if !x11 {
		return env, relays, cleanup, nil
	}

	hostDisplay, hostSocket, err := relay.X11Display()
	if err != nil {
		return nil, nil, cleanup, err
	}
	xn, xrelease, err := relay.Claim(x11Dir, "X")
	if err != nil {
		return nil, nil, cleanup, err
	}
	undo = append(undo, xrelease)
	xr, err := relay.Listen(filepath.Join(x11Dir, "X"+strconv.Itoa(xn)), hostSocket)
	if err != nil {
		return nil, nil, cleanup, err
	}
	go func() { _ = xr.Serve() }()
	undo = append(undo, xr.Close)
	relays = append(relays, xr)
	env = append(env, "DISPLAY=:"+strconv.Itoa(xn))

	authName := gui.XAuthorityName + "-" + strconv.Itoa(xn)
	authPath := filepath.Join(relayDir, authName)
	ok, err := relay.WriteGuestXAuthority(relay.XAuthorityPath(), hostDisplay, authPath, xn)
	if err != nil {
		return nil, nil, cleanup, err
	}
	if ok {
		undo = append(undo, func() { os.Remove(authPath) })
		env = append(env, "XAUTHORITY="+filepath.Join(gui.ContainerRuntimeDir, authName))
	}
	return env, relays, cleanup, nil
}

// waitForGUIApp keeps the relays up after the launched command exited, for as long as the app
// still has windows open, or might still open its first one.
func waitForGUIApp(ctx context.Context, relays []*relay.Relay) {
	used := func() bool {
		for _, r := range relays {
			if r.Used() {
				return true
			}
		}
		return false
	}
	if !used() {
		deadline := time.After(guiFirstConnectWait)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for !used() {
			select {
			case <-ctx.Done():
				return
			case <-deadline:
				return
			case <-tick.C:
			}
		}
	}
	relay.WaitIdle(ctx, guiIdleGrace, relays...)
}

// detachGUI re-runs this same command without --detach in a new session, logging to a file.
func detachGUI(out io.Writer, name string) error {
	logPath, err := guiLogPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	fmt.Fprintf(logFile, "--- %s anvil %v\n", time.Now().Format(time.RFC3339), withoutDetach(os.Args[1:]))

	cmd := exec.Command(exe, withoutDetach(os.Args[1:])...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Fprintf(out, "started in the background, log: %s\n", logPath)
	return cmd.Process.Release()
}
