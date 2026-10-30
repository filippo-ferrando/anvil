package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// appFlags are the `anvil gui --ephemeral` flags that size the clone.
type appFlags struct {
	cpus      int32
	memoryMiB int64
	arch      string
	env       []string
}

func (f appFlags) request(distro, pkg string, command []string, x11, infoOnly bool) *anvilv1.AppRequest {
	return &anvilv1.AppRequest{
		Distro: distro, Package: pkg, Command: command, X11: x11, Cpus: f.cpus, MemoryMib: f.memoryMiB,
		Arch: f.arch, Env: f.env, InfoOnly: infoOnly,
	}
}

// args turns f back into `anvil gui` flags, for a desktop entry.
func (f appFlags) args() []string {
	var args []string
	if f.cpus > 0 {
		args = append(args, "--cpus", strconv.Itoa(int(f.cpus)))
	}
	if f.memoryMiB > 0 {
		args = append(args, "--memory", strconv.FormatInt(f.memoryMiB, 10))
	}
	if f.arch != "" {
		args = append(args, "--arch", f.arch)
	}
	for _, e := range f.env {
		args = append(args, "--env", e)
	}
	return args
}

// waypipeSocketWait bounds how long the local waypipe takes to create its socket.
const waypipeSocketWait = 5 * time.Second

// runEphemeralGUI runs pkg's app in a throwaway clone of distro, with its windows on this
// desktop through a local `waypipe client`, and returns once the app exits.
func runEphemeralGUI(ctx context.Context, flags *globalFlags, f appFlags, distro, pkg string, command []string, x11 bool) error {
	// The clone's waypipe runs xwayland-satellite itself, so the host only needs Wayland.
	if err := checkLocalDisplay(false); err != nil {
		return err
	}
	bin, err := hostWaypipe()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	c, err := dial(flags)
	if err != nil {
		return err
	}
	defer c.Close()
	if c.Remote != "" {
		return errors.New("anvil gui --ephemeral doesn't work with --remote yet")
	}
	stream, err := c.Ephemeral.RunApp(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&anvilv1.AppInput{Input: &anvilv1.AppInput_Request{Request: f.request(distro, pkg, command, x11, false)}}); err != nil {
		return err
	}

	var waypipe *exec.Cmd
	defer func() {
		if waypipe != nil {
			_ = waypipe.Process.Kill()
			_ = waypipe.Wait()
		}
	}()
	var exit *anvilv1.RunExit
	err = streamStatus(os.Stderr, stream.Recv, func(l *liveLine, ev *anvilv1.AppEvent) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.AppEvent_Progress:
			l.set(e.Progress.GetMessage())
		case *anvilv1.AppEvent_Session:
			if waypipe, err = startWaypipeClient(bin, e.Session.GetDisplayDir()); err != nil {
				return true, err
			}
			return false, stream.Send(&anvilv1.AppInput{Input: &anvilv1.AppInput_DisplayReady{DisplayReady: true}})
		case *anvilv1.AppEvent_App:
			l.printf("anvil: %s runs in %s, deleted when it exits\n", appLabel(e.App), e.App.GetCloneId())
		case *anvilv1.AppEvent_Output:
			l.finish()
			w := os.Stdout
			if e.Output.GetStream() == anvilv1.OutputStream_OUTPUT_STREAM_STDERR {
				w = os.Stderr
			}
			_, _ = w.Write(e.Output.GetData())
		case *anvilv1.AppEvent_Exit:
			exit = e.Exit
		}
		return false, nil
	})
	if err != nil {
		if status.Code(err) == codes.Canceled || ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "anvil: cancelled, the clone is deleted")
			return ExitError{Code: 130}
		}
		return err
	}
	if exit == nil {
		return errors.New("anvild ended the app run without a result")
	}
	if exit.GetError() != "" {
		return errors.New(exit.GetError())
	}
	return ExitError{Code: int(exit.GetExitCode())}.orNil()
}

// startWaypipeClient starts `waypipe client` on dir's socket, reachable by anvild once it
// exists: anvild joins the clone's waypipe to it.
func startWaypipeClient(bin, dir string) (*exec.Cmd, error) {
	sock := filepath.Join(dir, gui.AppDisplaySocket)
	cmd := exec.Command(bin, "-s", sock, "client")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting waypipe: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.Now().Add(waypipeSocketWait)
	for {
		if _, err := os.Stat(sock); err == nil {
			// anvild runs as its own user and needs write access to connect.
			if err := os.Chmod(sock, 0o666); err != nil {
				_ = cmd.Process.Kill()
				return nil, err
			}
			return cmd, nil
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("waypipe exited before it listened: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("waypipe didn't create %s", sock)
		}
	}
}

// appLabel names an app for a message: its desktop entry's name and its command.
func appLabel(a *anvilv1.AppStarted) string {
	cmd := strings.Join(a.GetCommand(), " ")
	if name := a.GetInfo().GetAppName(); name != "" && a.GetInfo().GetFound() {
		return fmt.Sprintf("%s (%s)", name, cmd)
	}
	return cmd
}

// ephemeralAppInfo builds pkg's warm snapshot when needed, then reports its app without running it.
func ephemeralAppInfo(ctx context.Context, c *client.Client, f appFlags, distro, pkg string, command []string, x11 bool, progress io.Writer) (*anvilv1.AppStarted, error) {
	stream, err := c.Ephemeral.RunApp(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&anvilv1.AppInput{Input: &anvilv1.AppInput_Request{Request: f.request(distro, pkg, command, x11, true)}}); err != nil {
		return nil, err
	}
	var app *anvilv1.AppStarted
	err = streamStatus(progress, stream.Recv, func(l *liveLine, ev *anvilv1.AppEvent) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.AppEvent_Progress:
			l.set(e.Progress.GetMessage())
		case *anvilv1.AppEvent_App:
			app = e.App
		case *anvilv1.AppEvent_Exit:
			if e.Exit.GetError() != "" {
				return true, errors.New(e.Exit.GetError())
			}
		}
		return false, nil
	})
	if err == nil && app == nil {
		err = fmt.Errorf("anvild found no app for %s on %s", pkg, distro)
	}
	return app, err
}
