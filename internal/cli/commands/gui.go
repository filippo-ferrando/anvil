package commands

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/events"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// guiOptions are the `anvil gui` flags shared by the run and export paths.
type guiOptions struct {
	x11       bool
	detach    bool
	user      string
	identity  string
	ephemeral bool
	app       appFlags // with ephemeral
	json      bool
}

// guiOutput writes `anvil gui` progress: human lines on stderr, or JSON events on stdout with --json.
type guiOutput struct {
	json          bool
	out, progress io.Writer
}

func newGUIOutput(cmd *cobra.Command, json bool) guiOutput {
	return guiOutput{json: json, out: cmd.OutOrStdout(), progress: cmd.ErrOrStderr()}
}

// steps is where progress statuses go; with --json each line becomes a step event.
func (o guiOutput) steps() io.Writer {
	if o.json {
		return &events.StepWriter{W: o.out}
	}
	return o.progress
}

// appStdout keeps the app's own output off stdout with --json, which only carries events.
func (o guiOutput) appStdout() io.Writer {
	if o.json {
		return os.Stderr
	}
	return os.Stdout
}

func (o guiOutput) event(e events.Event) { _ = events.Encode(o.out, e) }

// running says app started in where; human output puts text on stderr.
func (o guiOutput) running(app, where, text string) {
	if o.json {
		o.event(events.Event{Event: events.Running, App: app, Where: where, Text: text})
		return
	}
	fmt.Fprintf(o.progress, "anvil: %s\n", text)
}

// finish ends a --json run with an error or a done event, and returns err.
func (o guiOutput) finish(err error) error {
	if o.json {
		if err != nil {
			o.event(events.Event{Event: events.Error, Message: err.Error()})
		} else {
			o.event(events.Event{Event: events.Done})
		}
	}
	return err
}

func newGUICommand(flags *globalFlags) *cobra.Command {
	var opts guiOptions
	cmd := &cobra.Command{
		Use:   "gui <name> [--] <command> [args...] | gui --ephemeral <distro> <package> [-- <command> [args...]]",
		Short: "Run a graphical app from an instance in a window on this desktop",
		Long: "Run a graphical app inside a VM or container and show its windows on this host's " +
			"Wayland desktop. A stopped instance is started first. X11 apps (Java/Swing ones like " +
			"Ghidra, for example) need --x11; in a VM it also sets _JAVA_AWT_WM_NONREPARENTING=1 so " +
			"Java windows don't stay blank. Only instances created by an anvil version with GUI " +
			"support can do this.\n\n" +
			"A container connects straight to the host's compositor (GPU included); a VM, or a " +
			"container behind --remote, goes through waypipe over SSH (no --x11 for a remote " +
			"container). Put `--` before the app's command if it has its own flags.\n\n" +
			"With --ephemeral the app runs in a throwaway Firecracker clone of <distro> instead, deleted " +
			"when the app exits. <package> is installed with the distro's own package manager when the " +
			"image lacks it, once, into a warm snapshot that later runs start from. The command defaults " +
			"to the one in the package's desktop entry. See docs/gui.md.",
		Example: "  anvil gui dev -- gnome-text-editor\n" +
			"  anvil gui --x11 re-lab -- ghidraRun\n" +
			"  anvil gui --detach dev -- firefox\n" +
			"  anvil gui --ephemeral debian-13 firefox-esr\n" +
			"  anvil gui --ephemeral fedora-42 libreoffice -- libreoffice --writer",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			o := newGUIOutput(cmd, opts.json)
			return o.finish(runGUICommand(cmd, flags, opts, o, args))
		},
	}
	cmd.Flags().BoolVar(&opts.ephemeral, "ephemeral", false, "run <package>'s app in a throwaway clone of <distro>")
	opts.app.addFlags(cmd)
	cmd.Flags().BoolVar(&opts.x11, "x11", false, "also make X11 apps work, through XWayland")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "return right away and log to $XDG_STATE_HOME/anvil/gui/<name>.log")
	cmd.Flags().StringVar(&opts.user, "user", "", "user to run the app as (VM: SSH user, container: docker exec --user)")
	cmd.Flags().StringVarP(&opts.identity, "identity", "i", "", "private key for SSH, instead of anvil's own managed key (VM only)")
	cmd.Flags().BoolVar(&opts.json, "json", false, "write progress as JSON events on stdout, one per line, for scripts and the TUI")
	cmd.AddCommand(newGUIExportCommand(flags), newGUIUnexportCommand(), newGUIExportsCommand())
	return cmd
}

func runGUICommand(cmd *cobra.Command, flags *globalFlags, opts guiOptions, o guiOutput, args []string) error {
	if opts.ephemeral {
		if opts.user != "" || opts.identity != "" {
			return errors.New("--user and --identity don't apply to --ephemeral: the app runs as the image's default user")
		}
		if opts.detach {
			return detachGUI(o, "ephemeral-"+args[0]+"-"+args[1])
		}
		return runEphemeralGUI(cmd.Context(), flags, opts.app, o, args[0], args[1], args[2:], opts.x11)
	}
	if cmd.Flags().Changed("cpus") || cmd.Flags().Changed("memory") || cmd.Flags().Changed("arch") || cmd.Flags().Changed("env") {
		return errors.New("--cpus, --memory, --arch and --env are for --ephemeral")
	}
	if opts.detach {
		return detachGUI(o, args[0])
	}
	return runGUI(cmd.Context(), flags, opts, o, args[0], args[1:])
}

// errRemoteContainerX11 is anvild's own refusal, for checks made before calling it.
var errRemoteContainerX11 = errors.New("X11 apps (--x11) don't work over --remote for containers yet; use a VM for X11 apps")

// runGUI prepares name through the daemon, then connects the app to the local display.
func runGUI(ctx context.Context, flags *globalFlags, opts guiOptions, o guiOutput, name string, command []string) error {
	if err := checkLocalDisplay(opts.x11); err != nil {
		return err
	}
	c, err := dial(flags)
	if err != nil {
		return err
	}
	// A remote session needs the local waypipe for VMs and containers alike: find it before
	// PrepareGUI can start or restart anything there.
	var waypipe string
	if c.Remote != "" {
		if waypipe, err = hostWaypipe(); err != nil {
			c.Close()
			return err
		}
	}
	session, err := prepareGUI(ctx, c, name, opts.x11, o.steps())
	c.Close()
	if err != nil {
		return err
	}
	inst := session.GetInstance()
	app := strings.Join(command, " ")
	var remoteArgs []string
	if inst.GetContainer() != nil && c.Remote != "" {
		if remoteArgs, err = remoteContainerGUIArgs(c.Remote, session, newDisplayName(), opts.user, command); err != nil {
			return err
		}
	}
	o.running(app, name, app+" runs in "+name)
	switch {
	case remoteArgs != nil:
		return runWaypipe(waypipe, remoteArgs, o)
	case inst.GetContainer() != nil:
		return runContainerGUI(ctx, inst, session, opts, o, command)
	}
	return runVMGUI(c, inst, session, opts, o, command)
}

// prepareGUI calls PrepareGUI, printing its progress, and returns the final session.
func prepareGUI(ctx context.Context, c *client.Client, name string, x11 bool, progress io.Writer) (*anvilv1.GUISession, error) {
	stream, err := c.PrepareGUI(ctx, &anvilv1.PrepareGUIRequest{Name: name, X11: x11, Remote: c.Remote != ""})
	if err != nil {
		return nil, err
	}
	var session *anvilv1.GUISession
	err = streamStatus(progress, stream.Recv, func(l *liveLine, ev *anvilv1.PrepareGUIProgress) (bool, error) {
		switch e := ev.GetEvent().(type) {
		case *anvilv1.PrepareGUIProgress_Status:
			l.set(e.Status)
		case *anvilv1.PrepareGUIProgress_Error:
			return false, errors.New(e.Error)
		case *anvilv1.PrepareGUIProgress_Session:
			session = e.Session
			return true, nil
		}
		return false, nil
	})
	if err == nil && session == nil {
		err = fmt.Errorf("anvild ended the GUI setup of %q without a result", name)
	}
	return session, err
}

// runVMGUI runs command in the VM under waypipe, over the same SSH path as `anvil exec`.
func runVMGUI(c *client.Client, inst *anvilv1.Instance, session *anvilv1.GUISession, opts guiOptions, o guiOutput, command []string) error {
	key, err := client.ResolveIdentity(opts.identity)
	if err != nil {
		return err
	}
	target, err := c.SSHTarget(inst, opts.user)
	if err != nil {
		return err
	}
	sshArgs, err := client.SSHArgs(target, "-p", key)
	if err != nil {
		return err
	}
	bin, err := hostWaypipe()
	if err != nil {
		return err
	}
	return runWaypipe(bin, waypipeArgs(sshArgs, target.User+"@"+target.Host, session.GetGuestToolsDir(), opts.x11, command), o)
}

// runWaypipe runs the local waypipe with args, the app's output going where o wants it.
func runWaypipe(bin string, args []string, o guiOutput) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, o.appStdout(), os.Stderr
	return cmd.Run()
}

// newDisplayName is a Wayland display name no other session in a relay dir can guess or reuse.
func newDisplayName() string {
	return "wayland-anvil-" + rand.Text()[:12]
}

// chmodThenExec makes waypipe's display socket ($1) reachable by any container user, then runs the rest.
const chmodThenExec = `chmod 0777 "$1" && shift && exec "$@"`

// remoteContainerGUIArgs builds the waypipe call for a container behind --remote: waypipe server
// listens in its relay dir there, and docker exec runs command under the wait script.
func remoteContainerGUIArgs(remote string, session *anvilv1.GUISession, display, user string, command []string) ([]string, error) {
	inst := session.GetInstance()
	spec := inst.GetContainer()
	if spec.GetContainerId() == "" {
		return nil, fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}
	if session.GetHostToolsDir() == "" || session.GetRelayDir() == "" {
		return nil, fmt.Errorf("anvild on %s is too old for container GUI apps over --remote: update anvil there", remote)
	}
	socket := path.Join(session.GetRelayDir(), display)
	words := []string{"sh", "-c", chmodThenExec, "sh", socket,
		format.Engine(spec.GetEngine()), "exec",
		"-e", "XDG_RUNTIME_DIR=" + gui.ContainerRuntimeDir, "-e", "WAYLAND_DISPLAY=" + display}
	if user != "" {
		words = append(words, "--user", user)
	}
	words = append(words, spec.GetContainerId())
	words = append(words, gui.WaitArgv(command)...)
	for i, w := range words {
		words[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return []string{
		"--remote-bin", path.Join(session.GetHostToolsDir(), "waypipe"), "--display", socket,
		"ssh", "-o", "BatchMode=yes", remote, strings.Join(words, " "),
	}, nil
}

// x11JavaEnv makes Java/Swing apps draw under xwayland-satellite, which doesn't reparent
// windows. Without it their windows stay blank.
const x11JavaEnv = "_JAVA_AWT_WM_NONREPARENTING=1"

// waypipeArgs builds `waypipe [--xwls] ssh <ssh args> <dest> <command line>`, with the guest's
// shipped waypipe; the command line waits for processes the command leaves in the background.
func waypipeArgs(sshArgs []string, dest, guestToolsDir string, x11 bool, command []string) []string {
	args := []string{"--remote-bin", path.Join(guestToolsDir, "waypipe")}
	if x11 {
		args = append(args, "--xwls")
	}
	args = append(args, "ssh")
	args = append(args, sshArgs...)
	args = append(args, dest)
	if x11 {
		command = append([]string{"env", x11JavaEnv}, command...)
	}
	return append(args, gui.WaitShellCommand(command))
}

// hostWaypipe prefers the waypipe shipped with anvil, the same build the guests run.
func hostWaypipe() (string, error) {
	shipped := filepath.Join(config.GUIToolsDir(), "waypipe")
	if st, err := os.Stat(shipped); err == nil && st.Mode()&0o111 != 0 {
		return shipped, nil
	}
	if bin, err := exec.LookPath("waypipe"); err == nil {
		return bin, nil
	}
	return "", fmt.Errorf("waypipe: not found in %s or on PATH (needed for `anvil gui` with a VM or --remote)", config.GUIToolsDir())
}

// guiLogPath is where a detached `anvil gui` for instance name writes its output.
func guiLogPath(name string) (string, error) {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "anvil", "gui", name+".log"), nil
}

// withoutDetach drops --detach from anvil's own flags, leaving the app's command (after --) alone.
func withoutDetach(args []string) []string {
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...)
		}
		if a == "--detach" || strings.HasPrefix(a, "--detach=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (f *appFlags) addFlags(cmd *cobra.Command) {
	cmd.Flags().Int32Var(&f.cpus, "cpus", 0, "with --ephemeral: vCPUs of the clone (default 2)")
	cmd.Flags().Int64Var(&f.memoryMiB, "memory", 0, "with --ephemeral: memory of the clone in MiB (default 2048)")
	cmd.Flags().StringVar(&f.arch, "arch", "", "with --ephemeral: image architecture (default: this host's)")
	cmd.Flags().StringArrayVarP(&f.env, "env", "e", nil, "with --ephemeral: environment variable KEY=VALUE for the app (repeatable)")
}
