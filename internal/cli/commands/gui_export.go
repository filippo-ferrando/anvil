package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/desktop"
)

func newGUIExportCommand(flags *globalFlags) *cobra.Command {
	var opts guiOptions
	var title, icon string
	var ephemeral bool
	cmd := &cobra.Command{
		Use:   "export <name> [--] <command> [args...] | export --ephemeral <distro> <package> [-- <command> [args...]]",
		Short: "Add an instance's GUI app to this desktop's app launcher",
		Long: "Write a desktop entry (~/.local/share/applications) that runs the app with " +
			"`anvil gui --detach`, so it shows up in the app launcher like a native app. The app's " +
			"name and icon are taken from its own desktop entry inside the instance, when it has one.\n\n" +
			"With --ephemeral the entry runs the app in a throwaway clone (`anvil gui --ephemeral`). " +
			"The export builds the warm snapshot with the package now, so the first launch is fast.",
		Example: "  anvil gui export --x11 re-lab -- ghidraRun\n" +
			"  anvil gui export --title Inkscape --icon ~/icons/inkscape.svg dev -- inkscape\n" +
			"  anvil gui export --ephemeral debian-13 firefox-esr",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, command := args[0], args[1:]
			pkg := ""
			c, err := dial(flags)
			if err != nil {
				return err
			}
			var info *anvilv1.GUIAppInfoReply
			if ephemeral {
				if opts.user != "" {
					c.Close()
					return errors.New("--user doesn't apply to --ephemeral: the app runs as the image's default user")
				}
				if c.Remote != "" {
					c.Close()
					return errors.New("anvil gui --ephemeral doesn't work with --remote yet")
				}
				pkg, command = args[1], args[2:]
				var app *anvilv1.AppStarted
				app, err = ephemeralAppInfo(cmd.Context(), c, opts.app, name, pkg, command, opts.x11, cmd.ErrOrStderr())
				if err == nil {
					info, command = app.GetInfo(), app.GetCommand()
				}
			} else {
				info, err = c.GetGUIAppInfo(cmd.Context(), &anvilv1.GUIAppInfoRequest{Name: name, Command: command[0]})
			}
			c.Close()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			dataDir, err := desktop.DataHome()
			if err != nil {
				return err
			}
			path, err := desktop.Write(dataDir, desktop.Spec{
				Bin: exe, Remote: flags.remote, Instance: name, Command: command,
				X11: opts.x11, User: opts.user, Title: title, Icon: icon, Package: pkg, Flags: opts.app.args(),
			}, info)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Exported %s\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.x11, "x11", false, "the app needs X11 (XWayland)")
	cmd.Flags().StringVar(&opts.user, "user", "", "user to run the app as")
	cmd.Flags().StringVar(&title, "title", "", "name in the launcher (default: the app's own name)")
	cmd.Flags().StringVar(&icon, "icon", "", "icon file on this host (default: the app's icon from the instance)")
	cmd.Flags().BoolVar(&ephemeral, "ephemeral", false, "the entry runs <package>'s app in a throwaway clone of <distro>")
	opts.app.addFlags(cmd)
	return cmd
}

func newGUIUnexportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unexport <name> <app>",
		Short: "Remove a desktop entry added with `anvil gui export`",
		Long: "Remove a desktop entry and its icon. <name> and <app> are the INSTANCE and APP columns of " +
			"`anvil gui exports`, e.g. `ephemeral:debian-13 firefox-esr` for an `export --ephemeral` entry.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, err := desktop.DataHome()
			if err != nil {
				return err
			}
			path, err := desktop.Remove(dataDir, args[0], args[1])
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("no exported app %q for %q (see `anvil gui exports`)", args[1], args[0])
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", path)
			return nil
		},
	}
}

func newGUIExportsCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "exports",
		Short: "List the desktop entries added with `anvil gui export`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkOutput(output); err != nil {
				return err
			}
			dataDir, err := desktop.DataHome()
			if err != nil {
				return err
			}
			exports, err := desktop.List(dataDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if output == "json" {
				// Local files, not a daemon reply, so this is the one list with its own JSON shape.
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if exports == nil {
					exports = []desktop.Export{}
				}
				return enc.Encode(exports)
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "INSTANCE\tAPP\tNAME\tCOMMAND")
			for _, e := range exports {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Instance, e.App, e.Name, e.Command)
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &output)
	return cmd
}
