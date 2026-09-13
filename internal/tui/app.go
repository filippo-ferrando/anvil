// Package tui implements `anvil tui` (M8): a Bubble Tea terminal UI,
// built on the exact same pkg/client wrapper the CLI uses — no business
// logic here either, per the plan's thin-client mandate, just a
// different way to drive the same RPCs `anvil launch`/`list`/`migrate`/
// etc. already do.
//
// Originally written against rivo/tview; rewritten on
// charmbracelet/bubbletea + charmbracelet/bubbles + charmbracelet/
// lipgloss after a real run showed tview's manual widget wiring producing
// broken-looking text boxes and confusing navigation. Bubble Tea's
// model (one state machine: Init/Update/View, external work reported back
// as typed messages) is a smaller surface to get right than tview's
// imperative widget tree, and the Charm ecosystem is what most terminal
// UIs people actually call "modern" today are built on.
//
// Same honest caveat as before: no real TTY in this sandbox to run a
// terminal UI against, and no network access to fetch these three
// dependencies (see the Makefile's `tui-deps` target) — reviewed
// carefully, gofmt-clean, built against long-stable core APIs
// (Model/Update/View, bubbles' list/textinput/textarea/viewport,
// tea.ExecProcess for the shell handoff), but genuinely unexercised
// until a real `anvil tui` walkthrough on your own machine.
package tui

import (
	"fmt"
	"os/user"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anvil-project/anvil/pkg/client"
)

type screen int

const (
	screenMenu screen = iota
	screenInstances
	screenLaunch
	screenCloudInit
	screenMirrors
	screenMigration
)

// model is the whole application's state — one Bubble Tea model, per the
// framework's own architecture, with a field per screen holding that
// screen's own state. Only the active screen's Update/View actually run;
// the others just sit idle, cheap to keep around.
type model struct {
	client *client.Client
	socket string
	who    string // local OS username, shown in the header

	screen screen
	width  int
	height int

	status    string // one-line, transient: last action's result or error
	statusBad bool

	menu      menuModel
	instances instancesModel
	launch    launchModel
	cloudInit cloudInitModel
	mirrors   mirrorsModel
	migration migrationModel
}

// Run dials socketPath and blocks running the TUI until the user quits
// (q from the main menu, or Ctrl+C anywhere) or an unrecoverable error
// occurs.
func Run(socketPath string) error {
	c, err := client.Dial(socketPath)
	if err != nil {
		return fmt.Errorf("tui: dialing anvild at %s: %w", socketPath, err)
	}
	defer c.Close()

	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}

	m := model{
		client:    c,
		socket:    socketPath,
		who:       who,
		menu:      newMenuModel(),
		instances: newInstancesModel(),
		cloudInit: newCloudInitModel(),
		mirrors:   newMirrorsModel(),
		migration: newMigrationModel(),
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.instances.list.SetSize(msg.Width-4, contentHeight(msg.Height))
		m.cloudInit.setSize(msg.Width, contentHeight(msg.Height))
		m.mirrors.list.SetSize(msg.Width-4, contentHeight(msg.Height))
		m.migration.setSize(msg.Width, contentHeight(msg.Height))
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	switch m.screen {
	case screenMenu:
		return m.updateMenu(msg)
	case screenInstances:
		return m.updateInstances(msg)
	case screenLaunch:
		return m.updateLaunch(msg)
	case screenCloudInit:
		return m.updateCloudInit(msg)
	case screenMirrors:
		return m.updateMirrors(msg)
	case screenMigration:
		return m.updateMigration(msg)
	}
	return m, nil
}

func (m model) View() string {
	header := styleTitle.Render(" anvil ") + styleSubtitle.Render(fmt.Sprintf("  %s  socket=%s", m.who, m.socket))
	var body string
	switch m.screen {
	case screenMenu:
		body = m.menu.View()
	case screenInstances:
		body = m.instances.View()
	case screenLaunch:
		body = m.launch.View()
	case screenCloudInit:
		body = m.cloudInit.View()
	case screenMirrors:
		body = m.mirrors.View()
	case screenMigration:
		body = m.migration.View()
	}

	status := ""
	if m.status != "" {
		style := styleGood
		if m.statusBad {
			style = styleError
		}
		status = "\n" + style.Render(m.status)
	}

	return header + "\n\n" + body + status
}

// contentHeight leaves room for the header, spacing, and a status line —
// the same budget every screen's list/viewport sizing subtracts, so
// nothing ever gets clipped at the bottom of the terminal.
func contentHeight(termHeight int) int {
	h := termHeight - 6
	if h < 3 {
		h = 3
	}
	return h
}

// setStatus is how a screen reports "this action just happened" back up
// — a single consistent place in the view instead of each screen
// inventing its own inline message, and cleared the next time something
// else happens.
func (m *model) setStatus(text string, bad bool) {
	m.status, m.statusBad = text, bad
}
