package appinfo

import (
	"context"
	"path"
	"slices"
	"sort"
	"testing"
)

// mapFS is an in-memory FS.
type mapFS map[string]string

func (m mapFS) List(_ context.Context, dir string) ([]string, error) {
	var names []string
	for p := range m {
		if path.Dir(p) == dir {
			names = append(names, path.Base(p))
		}
	}
	sort.Strings(names)
	return names, nil
}

func (m mapFS) Read(_ context.Context, p string) ([]byte, bool, error) {
	data, ok := m[p]
	return []byte(data), ok, nil
}

func TestProgram(t *testing.T) {
	cases := map[string]string{
		"gimp-2.10 %U":                          "gimp-2.10",
		"/usr/bin/ghidraRun":                    "ghidraRun",
		`env GDK_BACKEND=x11 -u FOO firefox %u`: "firefox",
		`"/opt/My App/run" --flag %F`:           "run",
		"":                                      "",
	}
	for in, want := range cases {
		if got := Program(in); got != want {
			t.Errorf("Program(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitExec(t *testing.T) {
	got := SplitExec(`app "a b" "q\"x" %f plain`)
	want := []string{"app", "a b", `q"x`, "plain"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestParse(t *testing.T) {
	e, ok := Parse([]byte("# c\n[Desktop Entry]\nType=Application\nName=GIMP\nExec=gimp %U\nIcon=gimp\n[Desktop Action new]\nName=Other\nExec=other\n"))
	if !ok || e.Name != "GIMP" || e.Exec != "gimp %U" || e.Icon != "gimp" {
		t.Fatalf("got %+v %v", e, ok)
	}
	if _, ok := Parse([]byte("[Desktop Entry]\nType=Link\nExec=x\n")); ok {
		t.Error("links aren't apps")
	}
	if _, ok := Parse([]byte("[Desktop Entry]\nType=Application\nExec=x\nHidden=true\n")); ok {
		t.Error("hidden entries are skipped")
	}
}

func TestFind_ByExecWithThemeIcon(t *testing.T) {
	fs := mapFS{
		"/usr/share/applications/org.gimp.GIMP.desktop":     "[Desktop Entry]\nType=Application\nName=GNU Image Manipulation Program\nExec=gimp-2.10 %U\nIcon=gimp\n",
		"/usr/share/applications/other.desktop":             "[Desktop Entry]\nType=Application\nName=Other\nExec=other\n",
		"/usr/share/icons/hicolor/256x256/apps/gimp.png":    "png256",
		"/usr/share/icons/hicolor/48x48/apps/gimp.png":      "png48",
		"/usr/share/icons/hicolor/scalable/apps/other.svg":  "svg",
		"/usr/local/share/applications/unrelated.directory": "",
	}
	app, ok, err := Find(context.Background(), fs, "/usr/bin/gimp-2.10")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if app.Name != "GNU Image Manipulation Program" || string(app.Icon) != "png256" || app.IconExt != "png" || app.IconSize != 256 {
		t.Fatalf("got %+v", app)
	}
}

func TestFind_ScalableWinsAndAbsoluteIcons(t *testing.T) {
	fs := mapFS{
		"/usr/share/applications/a.desktop":            "[Desktop Entry]\nType=Application\nName=A\nExec=a\nIcon=a\n",
		"/usr/share/icons/hicolor/scalable/apps/a.svg": "svg",
		"/usr/share/icons/hicolor/512x512/apps/a.png":  "png",
		"/usr/local/share/applications/b.desktop":      "[Desktop Entry]\nType=Application\nName=B\nExec=b\nIcon=/opt/b/b.png\n",
		"/opt/b/b.png": "bpng",
	}
	app, _, _ := Find(context.Background(), fs, "a")
	if app.IconExt != "svg" || string(app.Icon) != "svg" {
		t.Fatalf("a: %+v", app)
	}
	app, ok, _ := Find(context.Background(), fs, "b")
	if !ok || string(app.Icon) != "bpng" || app.IconExt != "png" {
		t.Fatalf("b: %+v", app)
	}
}

func TestFind_FallsBackToFileNameAndMissing(t *testing.T) {
	fs := mapFS{
		"/usr/share/applications/ghidra.desktop": "[Desktop Entry]\nType=Application\nName=Ghidra\nExec=/opt/ghidra/ghidraRun\nIcon=ghidra\n",
		"/usr/share/pixmaps/ghidra.png":          "pix",
	}
	app, ok, _ := Find(context.Background(), fs, "ghidra")
	if !ok || app.Name != "Ghidra" || string(app.Icon) != "pix" {
		t.Fatalf("got %+v %v", app, ok)
	}
	if _, ok, _ := Find(context.Background(), fs, "nothing"); ok {
		t.Fatal("expected no match")
	}
}
