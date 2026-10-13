package commands

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// genericAppIcon is the icon theme name used when the instance has no icon for the app.
const genericAppIcon = "application-x-executable"

// guiExport is one desktop entry written by `anvil gui export`.
type guiExport struct {
	Instance string `json:"instance"`
	App      string `json:"app"`
	Name     string `json:"name"`
	Command  string `json:"command"`
	File     string `json:"file"`
}

func newGUIExportCommand(flags *globalFlags) *cobra.Command {
	var opts guiOptions
	var title, icon string
	cmd := &cobra.Command{
		Use:   "export <name> [--] <command> [args...]",
		Short: "Add an instance's GUI app to this desktop's app launcher",
		Long: "Write a desktop entry (~/.local/share/applications) that runs the app with " +
			"`anvil gui --detach`, so it shows up in the app launcher like a native app. The app's " +
			"name and icon are taken from its own desktop entry inside the instance, when it has one.",
		Example: "  anvil gui export --x11 re-lab -- ghidraRun\n" +
			"  anvil gui export --title Inkscape --icon ~/icons/inkscape.svg dev -- inkscape",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, command := args[0], args[1:]
			c, err := dial(flags)
			if err != nil {
				return err
			}
			info, err := c.GetGUIAppInfo(cmd.Context(), &anvilv1.GUIAppInfoRequest{Name: name, Command: command[0]})
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
			path, err := writeGUIExport(exe, flags.remote, name, command, opts, title, icon, info)
			if err != nil {
				return err
			}
			refreshDesktopDatabase(filepath.Dir(path))
			fmt.Fprintf(cmd.OutOrStdout(), "Exported %s\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.x11, "x11", false, "the app needs X11 (XWayland)")
	cmd.Flags().StringVar(&opts.user, "user", "", "user to run the app as")
	cmd.Flags().StringVar(&title, "title", "", "name in the launcher (default: the app's own name)")
	cmd.Flags().StringVar(&icon, "icon", "", "icon file on this host (default: the app's icon from the instance)")
	return cmd
}

func newGUIUnexportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unexport <name> <app>",
		Short: "Remove a desktop entry added with `anvil gui export`",
		Long:  "Remove a desktop entry and its icon. <app> is the APP column of `anvil gui exports`.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, err := xdgDataHome()
			if err != nil {
				return err
			}
			base := exportBaseName(args[0], args[1])
			desktop := filepath.Join(dataDir, "applications", base+".desktop")
			if err := os.Remove(desktop); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("no exported app %q for %q (see `anvil gui exports`)", args[1], args[0])
				}
				return err
			}
			icons, _ := filepath.Glob(filepath.Join(dataDir, "anvil", "icons", base+".*"))
			for _, p := range icons {
				_ = os.Remove(p)
			}
			refreshDesktopDatabase(filepath.Dir(desktop))
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", desktop)
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
			dataDir, err := xdgDataHome()
			if err != nil {
				return err
			}
			exports, err := listGUIExports(filepath.Join(dataDir, "applications"))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if output == "json" {
				// Local files, not a daemon reply, so this is the one list with its own JSON shape.
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if exports == nil {
					exports = []guiExport{}
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

// writeGUIExport writes the desktop entry (and icon) for command in instance name, returning its path.
func writeGUIExport(anvilBin, remote, name string, command []string, opts guiOptions, title, icon string, info *anvilv1.GUIAppInfoReply) (string, error) {
	dataDir, err := xdgDataHome()
	if err != nil {
		return "", err
	}
	app := exportSlug(filepath.Base(command[0]))
	base := exportBaseName(name, app)

	if title == "" {
		title = info.GetAppName()
	}
	if title == "" {
		title = filepath.Base(command[0])
	}

	iconValue := genericAppIcon
	switch {
	case icon != "":
		abs, err := filepath.Abs(icon)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("--icon: %w", err)
		}
		iconValue = abs
	case len(info.GetIcon()) > 0:
		iconDir := filepath.Join(dataDir, "anvil", "icons")
		if err := os.MkdirAll(iconDir, 0o755); err != nil {
			return "", err
		}
		iconPath := filepath.Join(iconDir, base+"."+info.GetIconExt())
		if err := os.WriteFile(iconPath, info.GetIcon(), 0o644); err != nil {
			return "", err
		}
		iconValue = iconPath
	}

	exec := []string{anvilBin}
	if remote != "" {
		exec = append(exec, "--remote", remote)
	}
	exec = append(exec, "gui", "--detach")
	if opts.x11 {
		exec = append(exec, "--x11")
	}
	if opts.user != "" {
		exec = append(exec, "--user", opts.user)
	}
	exec = append(exec, name, "--")
	exec = append(exec, command...)

	var b strings.Builder
	b.WriteString("[Desktop Entry]\nType=Application\n")
	fmt.Fprintf(&b, "Name=%s (%s)\n", desktopValue(title), desktopValue(name))
	fmt.Fprintf(&b, "Comment=Runs in the anvil instance %s\n", desktopValue(name))
	fmt.Fprintf(&b, "Exec=%s\n", desktopExec(exec))
	fmt.Fprintf(&b, "Icon=%s\n", desktopValue(iconValue))
	b.WriteString("Terminal=false\nCategories=Utility;\n")
	fmt.Fprintf(&b, "X-Anvil-Instance=%s\n", desktopValue(name))
	fmt.Fprintf(&b, "X-Anvil-App=%s\n", app)
	fmt.Fprintf(&b, "X-Anvil-Title=%s\n", desktopValue(title))
	fmt.Fprintf(&b, "X-Anvil-Command=%s\n", desktopValue(strings.Join(command, " ")))

	appsDir := filepath.Join(dataDir, "applications")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(appsDir, base+".desktop")
	return path, os.WriteFile(path, []byte(b.String()), 0o644)
}

// listGUIExports reads every anvil desktop entry in appsDir.
func listGUIExports(appsDir string) ([]guiExport, error) {
	paths, err := filepath.Glob(filepath.Join(appsDir, "anvil-*.desktop"))
	if err != nil {
		return nil, err
	}
	var out []guiExport
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		e := guiExport{File: p}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			key, value, _ := strings.Cut(sc.Text(), "=")
			switch key {
			case "X-Anvil-Instance":
				e.Instance = value
			case "X-Anvil-App":
				e.App = value
			case "X-Anvil-Title":
				e.Name = value
			case "X-Anvil-Command":
				e.Command = value
			}
		}
		f.Close()
		if e.Instance != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

func xdgDataHome() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

func exportBaseName(instance, app string) string {
	return "anvil-" + exportSlug(instance) + "-" + exportSlug(app)
}

// exportSlug keeps a name file-name and desktop-id safe.
func exportSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// desktopValue escapes a string value for a desktop entry.
func desktopValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
}

// desktopExec quotes args for an Exec key: reserved characters need double quotes, a few
// characters need a backslash inside them, and % is doubled so it isn't a field code.
func desktopExec(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, "%", "%%")
		if a != "" && !strings.ContainsAny(a, " \t\n\"'\\><~|&;$*?#()`=") {
			quoted[i] = a
			continue
		}
		a = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(a)
		quoted[i] = `"` + a + `"`
	}
	return desktopValue(strings.Join(quoted, " "))
}

// refreshDesktopDatabase asks the desktop to notice changed entries, when the tool is installed.
func refreshDesktopDatabase(dir string) {
	if bin, err := exec.LookPath("update-desktop-database"); err == nil {
		cmd := exec.Command(bin, dir)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		_ = cmd.Run()
	}
}
