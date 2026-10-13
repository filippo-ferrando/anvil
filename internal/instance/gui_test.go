package instance

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// guiBackend is a guestBackend that also prepares GUI sessions.
type guiBackend struct {
	guestBackend
	started  []string
	prepared []string
	x11      bool
	err      error
}

func (b *guiBackend) Start(_ context.Context, spec *Spec) error {
	b.started = append(b.started, spec.Name)
	return nil
}

func (b *guiBackend) PrepareGUI(_ context.Context, spec *Spec, x11 bool, progress func(string)) (GUISession, error) {
	b.prepared = append(b.prepared, spec.Name)
	b.x11 = x11
	progress("preparing")
	return GUISession{RelayDir: "/run/anvil/gui/" + spec.ID}, b.err
}

func TestPrepareGUI_RunningInstance(t *testing.T) {
	b := &guiBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "dev", Kind: KindContainer, State: StateRunning, GUI: true})
	m := NewManager(reg, map[Kind]Backend{KindContainer: b})

	var statuses []string
	spec, session, err := m.PrepareGUI(t.Context(), "dev", true, func(s string) { statuses = append(statuses, s) })
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "dev" || session.RelayDir != "/run/anvil/gui/01" || !b.x11 {
		t.Fatalf("got %+v %+v x11=%v", spec, session, b.x11)
	}
	if len(b.started) != 0 {
		t.Fatalf("a running instance must not be started again: %v", b.started)
	}
	if len(statuses) != 1 || statuses[0] != "preparing" {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestPrepareGUI_StartsStoppedInstanceFirst(t *testing.T) {
	b := &guiBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "dev", Kind: KindVM, State: StateStopped, GUI: true})
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	if _, _, err := m.PrepareGUI(t.Context(), "dev", false, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if len(b.started) != 1 || len(b.waited) != 1 || len(b.prepared) != 1 {
		t.Fatalf("started %v waited %v prepared %v", b.started, b.waited, b.prepared)
	}
	if got, _ := reg.GetByName("dev"); got.State != StateRunning {
		t.Fatalf("state %s, want running", got.State)
	}
}

func TestPrepareGUI_RejectsOlderInstance(t *testing.T) {
	b := &guiBackend{}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "old", Kind: KindVM, State: StateRunning})
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	_, _, err := m.PrepareGUI(t.Context(), "old", false, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "created before GUI app support") {
		t.Fatalf("got %v", err)
	}
	if len(b.prepared) != 0 {
		t.Fatal("backend must not be called")
	}
}

func TestPrepareGUI_BackendWithoutSupport(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "dev", Kind: KindVM, State: StateRunning, GUI: true})
	m := NewManager(reg, map[Kind]Backend{KindVM: &guestBackend{}})
	if _, _, err := m.PrepareGUI(t.Context(), "dev", false, func(string) {}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestPrepareGUI_PropagatesBackendError(t *testing.T) {
	want := errors.New("boom")
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "dev", Kind: KindVM, State: StateRunning, GUI: true})
	m := NewManager(reg, map[Kind]Backend{KindVM: &guiBackend{err: want}})
	if _, _, err := m.PrepareGUI(t.Context(), "dev", false, func(string) {}); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestLaunchAndForkEnableGUI(t *testing.T) {
	b := &forkingBackend{}
	reg := &memRegistry{}
	m := NewManager(reg, map[Kind]Backend{KindVM: b})
	if err := m.Launch(t.Context(), LaunchParams{Name: "src", Kind: KindVM, VM: &VMSpec{}, NoStart: true}, func(LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	if err := m.Fork(t.Context(), ForkParams{Source: "src", NewName: "copy"}, func(LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"src", "copy"} {
		if s, _ := reg.GetByName(name); !s.GUI {
			t.Errorf("%s: GUI not enabled", name)
		}
	}
}

// forkingBackend is a guestBackend that also forks.
type forkingBackend struct{ guestBackend }

func (b *forkingBackend) Fork(context.Context, *Spec, *Spec, func(string)) error { return nil }

// fileBackend is a guiBackend with an in-memory guest filesystem.
type fileBackend struct {
	guiBackend
	files map[string]string
}

func (b *fileBackend) ListGuestDir(_ context.Context, _ *Spec, dir string) ([]string, error) {
	var names []string
	for p := range b.files {
		if strings.HasPrefix(p, dir+"/") {
			names = append(names, strings.TrimPrefix(p, dir+"/"))
		}
	}
	return names, nil
}

func (b *fileBackend) ReadGuestFile(_ context.Context, _ *Spec, path string) ([]byte, bool, error) {
	data, ok := b.files[path]
	return []byte(data), ok, nil
}

func TestGUIAppInfo_StartsAndSearches(t *testing.T) {
	b := &fileBackend{files: map[string]string{
		"/usr/share/applications/gimp.desktop":         "[Desktop Entry]\nType=Application\nName=GIMP\nExec=gimp %U\nIcon=gimp\n",
		"/usr/share/icons/hicolor/48x48/apps/gimp.png": "icon",
	}}
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "dev", Kind: KindVM, State: StateStopped, GUI: true})
	m := NewManager(reg, map[Kind]Backend{KindVM: b})

	app, ok, err := m.GUIAppInfo(t.Context(), "dev", "gimp")
	if err != nil || !ok || app.Name != "GIMP" || string(app.Icon) != "icon" {
		t.Fatalf("got %+v %v %v", app, ok, err)
	}
	if len(b.started) != 1 {
		t.Fatal("a stopped instance should be started first")
	}
}
