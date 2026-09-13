package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type menuModel struct {
	items  []string
	cursor int
}

func newMenuModel() menuModel {
	return menuModel{items: []string{"Instances", "Cloud-Init", "Mirrors", "Migration", "Quit"}}
}

func (m model) updateMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "up", "k":
		if m.menu.cursor > 0 {
			m.menu.cursor--
		}
	case "down", "j":
		if m.menu.cursor < len(m.menu.items)-1 {
			m.menu.cursor++
		}
	case "q":
		return m, tea.Quit
	case "enter":
		switch m.menu.items[m.menu.cursor] {
		case "Instances":
			m.screen = screenInstances
			return m, loadInstances(m.client)
		case "Cloud-Init":
			m.screen = screenCloudInit
			return m, loadCloudInitList(m.client)
		case "Mirrors":
			m.screen = screenMirrors
			return m, loadMirrors(m.client)
		case "Migration":
			m.screen = screenMigration
			return m, loadHosts(m.client)
		case "Quit":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m menuModel) View() string {
	var b strings.Builder
	b.WriteString(styleSubtitle.Render("cloud-init VMs and containers, from your terminal") + "\n\n")
	for i, item := range m.items {
		if i == m.cursor {
			b.WriteString(styleMenuItemSelected.Render("▸ "+item) + "\n")
		} else {
			b.WriteString(styleMenuItem.Render("  "+item) + "\n")
		}
	}
	b.WriteString("\n" + helpBar("↑/↓", "move", "enter", "select", "q", "quit"))
	return b.String()
}
