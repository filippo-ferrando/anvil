package tui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"git.fferrando.cc/rdfilippo/anvil/internal/gui/desktop"
)

// guiAppsLoadedMsg carries the desktop entries written by `anvil gui export`.
type guiAppsLoadedMsg struct {
	exports []desktop.Export
	err     error
}

// guiAppRemovedMsg reports an unexport; path is the removed entry.
type guiAppRemovedMsg struct {
	path string
	err  error
}

// loadGUIApps lists this machine's exported apps, the same as `anvil gui exports`.
func loadGUIApps() tea.Cmd {
	return func() tea.Msg {
		dataDir, err := desktop.DataHome()
		if err != nil {
			return guiAppsLoadedMsg{err: err}
		}
		exports, err := desktop.List(dataDir)
		return guiAppsLoadedMsg{exports: exports, err: err}
	}
}

// removeGUIApp removes one exported app, the same as `anvil gui unexport`.
func removeGUIApp(e desktop.Export) tea.Cmd {
	return func() tea.Msg {
		dataDir, err := desktop.DataHome()
		if err != nil {
			return guiAppRemovedMsg{err: err}
		}
		path, err := desktop.Remove(dataDir, e.Instance, e.App)
		if errors.Is(err, os.ErrNotExist) {
			err = errors.New("no exported app " + e.App + " for " + e.Instance)
		}
		return guiAppRemovedMsg{path: path, err: err}
	}
}

// relaunchGUIApp runs the entry's own command line, as the app launcher does.
func relaunchGUIApp(e desktop.Export) tea.Cmd {
	return func() tea.Msg {
		if len(e.Exec) == 0 {
			return guiLaunchedMsg{err: errors.New(e.File + " has no Exec line")}
		}
		out, err := exec.Command(e.Exec[0], e.Exec[1:]...).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil && text != "" {
			err = errors.New(strings.TrimPrefix(text, "anvil: "))
		}
		return guiLaunchedMsg{output: text, err: err}
	}
}

type guiAppItem struct{ e desktop.Export }

func (i guiAppItem) FilterValue() string { return i.e.Instance + " " + i.e.App }
func (i guiAppItem) Title() string       { return i.e.Name + "  (" + i.e.Instance + ")" }
func (i guiAppItem) Description() string {
	return "app " + i.e.App + "  •  " + i.e.Command + "  •  " + i.e.File
}

type guiAppsModel struct {
	list          list.Model
	confirmRemove *desktop.Export
}

func newGUIAppsModel() guiAppsModel {
	l := newList()
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Exported GUI apps"
	l.SetShowHelp(false)
	return guiAppsModel{list: l}
}

func (m guiAppsModel) selected() *desktop.Export {
	if it, ok := m.list.SelectedItem().(guiAppItem); ok {
		return &it.e
	}
	return nil
}

func (m model) updateGUIApps(msg tea.Msg) (tea.Model, tea.Cmd) {
	ga := &m.guiApps
	switch msg := msg.(type) {
	case guiAppsLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing exported apps: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.exports))
		for i, e := range msg.exports {
			items[i] = guiAppItem{e: e}
		}
		ga.list.SetItems(items)
		return m, nil

	case guiAppRemovedMsg:
		if msg.err != nil {
			m.setStatus("unexport: "+msg.err.Error(), true)
			return m, nil
		}
		m.setStatus("removed "+msg.path, false)
		return m, loadGUIApps()

	case guiLaunchedMsg:
		m.showGUILaunched(msg)
		return m, nil

	case tea.KeyMsg:
		if ga.confirmRemove != nil {
			e := *ga.confirmRemove
			ga.confirmRemove = nil
			if k := msg.String(); k == "y" || k == "enter" {
				return m, removeGUIApp(e)
			}
			return m, nil
		}
		switch msg.String() {
		case "esc", "q":
			m.sidebarFocused = true
			return m, nil
		case "r":
			return m, loadGUIApps()
		case "x":
			ga.confirmRemove = ga.selected()
			return m, nil
		case "enter":
			if e := ga.selected(); e != nil {
				m.setStatus("starting "+e.Command+" in "+e.Instance+"…", false)
				return m, relaunchGUIApp(*e)
			}
			return m, nil
		}
		var cmd tea.Cmd
		ga.list, cmd = ga.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m guiAppsModel) View() string {
	if e := m.confirmRemove; e != nil {
		return styleWarn.Render("Remove "+e.Name+" ("+e.Instance+") from the app launcher?") + "\n\n" +
			styleSubtitle.Render(e.File) + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if len(m.list.Items()) == 0 {
		return styleSubtitle.Render("No exported apps. Add one with g on Instances (Add to desktop).") + "\n\n" +
			helpBar("r", "refresh", "esc", "back")
	}
	return m.list.View() + "\n" + helpBar("enter", "launch", "x", "unexport", "r", "refresh", "esc", "back")
}
