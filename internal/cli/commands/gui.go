package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/config"
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
			"A container connects straight to the host's compositor (GPU included); a VM goes " +
			"through waypipe over SSH. Put `--` before the app's command if it has its own flags.\n\n" +
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
			if opts.ephemeral {
				if opts.user != "" || opts.identity != "" {
					return errors.New("--user and --identity don't apply to --ephemeral: the app runs as the image's default user")
				}
				if opts.detach {
					return detachGUI(cmd.ErrOrStderr(), "ephemeral-"+args[0]+"-"+args[1])
				}
				return runEphemeralGUI(cmd.Context(), flags, opts.app, args[0], args[1], args[2:], opts.x11)
			}
			if cmd.Flags().Changed("cpus") || cmd.Flags().Changed("memory") || cmd.Flags().Changed("arch") || cmd.Flags().Changed("env") {
				return errors.New("--cpus, --memory, --arch and --env are for --ephemeral")
			}
			if opts.detach {
				return detachGUI(cmd.ErrOrStderr(), args[0])
			}
			return runGUI(cmd.Context(), flags, opts, args[0], args[1:], cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&opts.ephemeral, "ephemeral", false, "run <package>'s app in a throwaway clone of <distro>")
	opts.app.addFlags(cmd)
	cmd.Flags().BoolVar(&opts.x11, "x11", false, "also make X11 apps work, through XWayland")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "return right away and log to $XDG_STATE_HOME/anvil/gui/<name>.log")
	cmd.Flags().StringVar(&opts.user, "user", "", "user to run the app as (VM: SSH user, container: docker exec --user)")
	cmd.Flags().StringVarP(&opts.identity, "identity", "i", "", "private key for SSH, instead of anvil's own managed key (VM only)")
	cmd.AddCommand(newGUIExportCommand(flags), newGUIUnexportCommand(), newGUIExportsCommand())
	return cmd
}

// runGUI prepares name through the daemon, then connects the app to the local display.
func runGUI(ctx context.Context, flags *globalFlags, opts guiOptions, name string, command []string, progress io.Writer) error {
	if err := checkLocalDisplay(opts.x11); err != nil {
		return err
	}
	c, err := dial(flags)
	if err != nil {
		return err
	}
	session, err := prepareGUI(ctx, c, name, opts.x11, progress)
	c.Close()
	if err != nil {
		return err
	}
	inst := session.GetInstance()
	if inst.GetContainer() != nil {
		if c.Remote != "" {
			return errors.New("anvil gui with --remote only works for VMs for now")
		}
		return runContainerGUI(ctx, inst, session, opts, command)
	}
	return runVMGUI(c, inst, session, opts, command)
}

// prepareGUI calls PrepareGUI, printing its progress, and returns the final session.
func prepareGUI(ctx context.Context, c *client.Client, name string, x11 bool, progress io.Writer) (*anvilv1.GUISession, error) {
	stream, err := c.PrepareGUI(ctx, &anvilv1.PrepareGUIRequest{Name: name, X11: x11})
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
func runVMGUI(c *client.Client, inst *anvilv1.Instance, session *anvilv1.GUISession, opts guiOptions, command []string) error {
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
	cmd := exec.Command(bin, waypipeArgs(sshArgs, target.User+"@"+target.Host, session.GetGuestToolsDir(), opts.x11, command)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// x11JavaEnv makes Java/Swing apps draw under xwayland-satellite, which doesn't reparent
// windows. Without it their windows stay blank.
const x11JavaEnv = "_JAVA_AWT_WM_NONREPARENTING=1"

// waypipeArgs builds `waypipe [--xwls] ssh <ssh args> <dest> [env <x11JavaEnv>] <command>`,
// using the guest's shipped waypipe so both ends run the same version.
func waypipeArgs(sshArgs []string, dest, guestToolsDir string, x11 bool, command []string) []string {
	args := []string{"--remote-bin", path.Join(guestToolsDir, "waypipe")}
	if x11 {
		args = append(args, "--xwls")
	}
	args = append(args, "ssh")
	args = append(args, sshArgs...)
	args = append(args, dest)
	if x11 {
		args = append(args, "env", x11JavaEnv)
	}
	return append(args, command...)
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
	return "", fmt.Errorf("waypipe: not found in %s or on PATH (needed for `anvil gui` with a VM)", config.GUIToolsDir())
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
		if a == "--detach" || a == "--detach=true" {
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
