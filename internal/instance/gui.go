package instance

import (
	"context"
	"fmt"

	"git.fferrando.cc/rdfilippo/anvil/internal/gui/appinfo"
)

// guiSpec looks up a GUI-enabled instance and starts it (waiting for first-boot setup) if stopped.
func (m *Manager) guiSpec(ctx context.Context, name string, progress func(status string)) (*Spec, Backend, error) {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return nil, nil, err
	}
	if !spec.GUI {
		return nil, nil, fmt.Errorf("instance: %s was created before GUI app support, recreate it to use `anvil gui`", name)
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return nil, nil, err
	}
	if spec.State != StateRunning {
		progress("starting " + name)
		if err := m.Start(ctx, []string{name}); err != nil {
			return nil, nil, err
		}
		if err := m.WaitReady(ctx, name, progress); err != nil {
			return nil, nil, err
		}
		if spec, err = m.registry.GetByName(name); err != nil {
			return nil, nil, err
		}
	}
	return spec, b, nil
}

// PrepareGUI readies name for `anvil gui`, starting it first when stopped.
func (m *Manager) PrepareGUI(ctx context.Context, name string, x11 bool, progress func(status string)) (*Spec, GUISession, error) {
	spec, err := m.registry.GetByName(name)
	if err != nil {
		return nil, GUISession{}, err
	}
	b, err := m.backendFor(spec.Kind)
	if err != nil {
		return nil, GUISession{}, err
	}
	gp, ok := b.(GUIPreparer)
	if !ok {
		return nil, GUISession{}, fmt.Errorf("instance: %s instances don't support GUI apps", spec.Kind)
	}
	spec, _, err = m.guiSpec(ctx, name, progress)
	if err != nil {
		return nil, GUISession{}, err
	}
	session, err := gp.PrepareGUI(ctx, spec, x11, progress)
	if err != nil {
		return nil, GUISession{}, err
	}
	return m.withGuests([]*Spec{spec})[0], session, nil
}

// GUIAppInfo finds the desktop entry name and icon of command inside name, starting it first when stopped.
func (m *Manager) GUIAppInfo(ctx context.Context, name, command string) (appinfo.App, bool, error) {
	spec, b, err := m.guiSpec(ctx, name, func(string) {})
	if err != nil {
		return appinfo.App{}, false, err
	}
	r, ok := b.(GuestFileReader)
	if !ok {
		return appinfo.App{}, false, fmt.Errorf("instance: %s instances can't be searched for apps", spec.Kind)
	}
	return appinfo.Find(ctx, guestFS{r, spec}, command)
}

// guestFS adapts a GuestFileReader to appinfo.FS for one instance.
type guestFS struct {
	r    GuestFileReader
	spec *Spec
}

func (g guestFS) List(ctx context.Context, dir string) ([]string, error) {
	return g.r.ListGuestDir(ctx, g.spec, dir)
}

func (g guestFS) Read(ctx context.Context, path string) ([]byte, bool, error) {
	return g.r.ReadGuestFile(ctx, g.spec, path)
}
