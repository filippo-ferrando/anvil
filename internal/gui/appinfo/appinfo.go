// Package appinfo finds an app's desktop entry and icon inside an instance's filesystem.
package appinfo

import (
	"bufio"
	"bytes"
	"context"
	"path"
	"strings"
)

// FS is read access to an instance's files.
type FS interface {
	// List returns the names in dir, nil when it doesn't exist.
	List(ctx context.Context, dir string) ([]string, error)
	// Read returns a file's content, ok false when it doesn't exist.
	Read(ctx context.Context, path string) (data []byte, ok bool, err error)
}

// App is what was found for a command.
type App struct {
	Name     string
	Icon     []byte
	IconExt  string // "png" or "svg"
	IconSize int    // png pixel size from the theme directory, 0 for svg or unknown
}

// MaxIconBytes skips icon files bigger than this.
const MaxIconBytes = 2 << 20

var applicationDirs = []string{"/usr/share/applications", "/usr/local/share/applications"}

// iconCandidates is the hicolor theme lookup order: scalable first, then big to small.
var iconCandidates = []struct {
	dir  string
	ext  string
	size int
}{
	{"/usr/share/icons/hicolor/scalable/apps", "svg", 0},
	{"/usr/share/icons/hicolor/512x512/apps", "png", 512},
	{"/usr/share/icons/hicolor/256x256/apps", "png", 256},
	{"/usr/share/icons/hicolor/128x128/apps", "png", 128},
	{"/usr/share/icons/hicolor/96x96/apps", "png", 96},
	{"/usr/share/icons/hicolor/64x64/apps", "png", 64},
	{"/usr/share/icons/hicolor/48x48/apps", "png", 48},
	{"/usr/share/pixmaps", "svg", 0},
	{"/usr/share/pixmaps", "png", 0},
}

// Entry is the part of a desktop entry this package uses.
type Entry struct {
	Name, Exec, TryExec, Icon string
}

// Find looks for the desktop entry that runs command and loads its icon.
func Find(ctx context.Context, fs FS, command string) (App, bool, error) {
	want := path.Base(command)
	var fallback *Entry
	for _, dir := range applicationDirs {
		names, err := fs.List(ctx, dir)
		if err != nil {
			return App{}, false, err
		}
		for _, n := range names {
			if !strings.HasSuffix(n, ".desktop") {
				continue
			}
			data, ok, err := fs.Read(ctx, path.Join(dir, n))
			if err != nil {
				return App{}, false, err
			}
			if !ok {
				continue
			}
			e, ok := Parse(data)
			if !ok {
				continue
			}
			if Program(e.Exec) == want || (e.TryExec != "" && path.Base(e.TryExec) == want) {
				return Load(ctx, fs, e)
			}
			if fallback == nil && strings.TrimSuffix(n, ".desktop") == want {
				fallback = &e
			}
		}
	}
	if fallback != nil {
		return Load(ctx, fs, *fallback)
	}
	return App{}, false, nil
}

// Load returns e's name and icon.
func Load(ctx context.Context, fs FS, e Entry) (App, bool, error) {
	app := App{Name: e.Name}
	icon, ext, size, err := findIcon(ctx, fs, e.Icon)
	if err != nil {
		return App{}, false, err
	}
	app.Icon, app.IconExt, app.IconSize = icon, ext, size
	return app, true, nil
}

func findIcon(ctx context.Context, fs FS, icon string) ([]byte, string, int, error) {
	if icon == "" {
		return nil, "", 0, nil
	}
	if path.IsAbs(icon) {
		ext := strings.TrimPrefix(path.Ext(icon), ".")
		if ext != "png" && ext != "svg" {
			return nil, "", 0, nil
		}
		data, ok, err := fs.Read(ctx, icon)
		if err != nil || !ok || len(data) > MaxIconBytes {
			return nil, "", 0, err
		}
		return data, ext, 0, nil
	}
	for _, c := range iconCandidates {
		data, ok, err := fs.Read(ctx, path.Join(c.dir, icon+"."+c.ext))
		if err != nil {
			return nil, "", 0, err
		}
		if ok && len(data) > 0 && len(data) <= MaxIconBytes {
			return data, c.ext, c.size, nil
		}
	}
	return nil, "", 0, nil
}

// Parse reads the [Desktop Entry] group of a desktop file; ok is false for anything
// other than a visible application.
func Parse(data []byte) (Entry, bool) {
	var e Entry
	inGroup, isApp, hidden := false, false, false
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inGroup = line == "[Desktop Entry]"
			continue
		}
		if !inGroup {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Type":
			isApp = value == "Application"
		case "Name":
			e.Name = value
		case "Exec":
			e.Exec = value
		case "TryExec":
			e.TryExec = value
		case "Icon":
			e.Icon = value
		case "Hidden":
			hidden = value == "true"
		}
	}
	return e, isApp && !hidden && e.Exec != ""
}

// Program returns the base name of the program an Exec value runs, skipping an
// `env VAR=value ...` prefix. Field codes like %U are ignored.
func Program(exec string) string {
	args := SplitExec(exec)
	i := 0
	if i < len(args) && path.Base(args[i]) == "env" {
		i++
		for i < len(args) && (strings.Contains(args[i], "=") || strings.HasPrefix(args[i], "-")) {
			if args[i] == "-u" || args[i] == "-C" {
				i++ // these take a value
			}
			i++
		}
	}
	if i >= len(args) {
		return ""
	}
	return path.Base(args[i])
}

// SplitExec splits an Exec value into arguments, following the desktop entry quoting rules.
func SplitExec(exec string) []string {
	var args []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(exec); i++ {
		c := exec[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(exec):
			i++
			cur.WriteByte(exec[i])
		case c == '"':
			inQuote = !inQuote
			have = true
		case !inQuote && (c == ' ' || c == '\t'):
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}
	out := args[:0]
	for _, a := range args {
		if len(a) == 2 && a[0] == '%' {
			continue // field code
		}
		out = append(out, a)
	}
	return out
}
