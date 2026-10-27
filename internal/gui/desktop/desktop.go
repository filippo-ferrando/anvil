// Package desktop writes, lists and removes the desktop entries behind `anvil gui export`.
package desktop

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
)

// genericAppIcon is the icon theme name used when the instance has no icon for the app.
const genericAppIcon = "application-x-executable"

// Export is one desktop entry written by `anvil gui export`.
type Export struct {
	Instance string `json:"instance"`
	App      string `json:"app"`
	Name     string `json:"name"`
	Command  string `json:"command"`
	File     string `json:"file"`
	// Exec is the entry's command line, the same one the app launcher runs.
	Exec []string `json:"-"`
}

// Spec describes the entry to write: the anvil binary and the `anvil gui` flags it runs with.
type Spec struct {
	Bin, Remote, Instance string
	Command               []string
	X11                   bool
	User                  string
	Title, Icon           string // launcher name and icon file, both optional
}

// DataHome returns $XDG_DATA_HOME, or ~/.local/share when it is not set.
func DataHome() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share"), nil
}

// Write writes the desktop entry (and icon) for s under dataDir, returning its path.
func Write(dataDir string, s Spec, info *anvilv1.GUIAppInfoReply) (string, error) {
	app := slug(filepath.Base(s.Command[0]))
	base := baseName(s.Instance, app)

	title := s.Title
	if title == "" {
		title = info.GetAppName()
	}
	if title == "" {
		title = filepath.Base(s.Command[0])
	}

	iconValue := genericAppIcon
	switch {
	case s.Icon != "":
		abs, err := filepath.Abs(s.Icon)
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

	args := []string{s.Bin}
	if s.Remote != "" {
		args = append(args, "--remote", s.Remote)
	}
	args = append(args, "gui", "--detach")
	if s.X11 {
		args = append(args, "--x11")
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	args = append(args, s.Instance, "--")
	args = append(args, s.Command...)

	var b strings.Builder
	b.WriteString("[Desktop Entry]\nType=Application\n")
	fmt.Fprintf(&b, "Name=%s (%s)\n", escapeValue(title), escapeValue(s.Instance))
	fmt.Fprintf(&b, "Comment=Runs in the anvil instance %s\n", escapeValue(s.Instance))
	fmt.Fprintf(&b, "Exec=%s\n", execValue(args))
	fmt.Fprintf(&b, "Icon=%s\n", escapeValue(iconValue))
	b.WriteString("Terminal=false\nCategories=Utility;\n")
	fmt.Fprintf(&b, "X-Anvil-Instance=%s\n", escapeValue(s.Instance))
	fmt.Fprintf(&b, "X-Anvil-App=%s\n", app)
	fmt.Fprintf(&b, "X-Anvil-Title=%s\n", escapeValue(title))
	fmt.Fprintf(&b, "X-Anvil-Command=%s\n", escapeValue(strings.Join(s.Command, " ")))

	appsDir := filepath.Join(dataDir, "applications")
	if err := os.MkdirAll(appsDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(appsDir, base+".desktop")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	refresh(appsDir)
	return path, nil
}

// List reads every anvil desktop entry under dataDir, sorted by file name.
func List(dataDir string) ([]Export, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "applications", "anvil-*.desktop"))
	if err != nil {
		return nil, err
	}
	var out []Export
	for _, p := range paths {
		if e, ok := readEntry(p); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func readEntry(path string) (Export, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Export{}, false
	}
	defer f.Close()
	e := Export{File: path}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, _ := strings.Cut(sc.Text(), "=")
		switch key {
		case "Exec":
			e.Exec = splitExec(value)
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
	return e, e.Instance != ""
}

// Remove deletes the entry for app in instance and its icon, returning the entry's path.
// The error wraps os.ErrNotExist when there is no such entry.
func Remove(dataDir, instance, app string) (string, error) {
	base := baseName(instance, app)
	path := filepath.Join(dataDir, "applications", base+".desktop")
	if err := os.Remove(path); err != nil {
		return path, err
	}
	icons, _ := filepath.Glob(filepath.Join(dataDir, "anvil", "icons", base+".*"))
	for _, p := range icons {
		_ = os.Remove(p)
	}
	refresh(filepath.Dir(path))
	return path, nil
}

func baseName(instance, app string) string {
	return "anvil-" + slug(instance) + "-" + slug(app)
}

// slug keeps a name file-name and desktop-id safe.
func slug(s string) string {
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

// escapeValue escapes a string value for a desktop entry.
func escapeValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
}

// unescapeValue reverses escapeValue, plus the \s escape of the spec.
func unescapeValue(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\n`, "\n", `\t`, "\t", `\r`, "\r", `\s`, " ").Replace(s)
}

// execValue quotes args for an Exec key: reserved characters need double quotes, a few
// characters need a backslash inside them, and % is doubled so it isn't a field code.
func execValue(args []string) string {
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
	return escapeValue(strings.Join(quoted, " "))
}

// splitExec reads an Exec value back into arguments, dropping field codes.
func splitExec(value string) []string {
	args := appinfo.SplitExec(unescapeValue(value))
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "%%", "%")
	}
	return args
}

// refresh asks the desktop to notice changed entries, when the tool is installed.
func refresh(dir string) {
	if bin, err := exec.LookPath("update-desktop-database"); err == nil {
		cmd := exec.Command(bin, dir)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		_ = cmd.Run()
	}
}
