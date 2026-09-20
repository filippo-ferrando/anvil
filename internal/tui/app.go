// Package tui implements `anvil tui`, a Bubble Tea terminal UI built on
// the same pkg/client wrapper the CLI uses.
package tui

import (
	"fmt"
	"os/user"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anvil-project/anvil/pkg/client"
)

// statusVisible is how long a status line stays up before being cleared.
const statusVisible = 4 * time.Second

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type screen int

const (
	screenInstances screen = iota
	screenSnapshots
	screenImages
	screenIntents
	screenCloudInit
	screenMirrors
	screenMigration
	screenLaunch // full-screen takeover reached via Instances' "n"
	screenLogs   // full-screen takeover reached via Instances' "l"
)

// screenOrder is the sidebar's own list, top to bottom.
var screenOrder = []screen{screenInstances, screenSnapshots, screenImages, screenIntents, screenCloudInit, screenMirrors, screenMigration}

var screenLabels = map[screen]string{
	screenInstances: "Instances",
	screenSnapshots: "Snapshots",
	screenImages:    "Images",
	screenIntents:   "Intents",
	screenCloudInit: "Cloud-Init",
	screenMirrors:   "Mirrors",
	screenMigration: "Migration",
}

const (
	sidebarWidth  = 20
	sidebarGutter = 2
)

// model is the whole application's Bubble Tea state, with one field per screen.
type model struct {
	client *client.Client
	socket string
	who    string // local OS username, shown in the header

	screen         screen
	sidebarFocused bool // true: up/down/enter on the sidebar; false: keys go to the active screen
	width          int
	height         int

	status      string // one-line, transient: last action's result or error
	statusBad   bool
	statusSetAt time.Time

	instances instancesModel
	snapshots snapshotsModel
	images    imagesModel
	intents   intentsModel
	launch    launchModel
	logs      logsModel
	cloudInit cloudInitModel
	mirrors   mirrorsModel
	migration migrationModel
}

// Run dials socketPath and blocks running the TUI until the user quits or an error occurs.
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
		client:         c,
		socket:         socketPath,
		who:            who,
		screen:         screenInstances,
		sidebarFocused: true,
		instances:      newInstancesModel(),
		snapshots:      newSnapshotsModel(),
		images:         newImagesModel(),
		intents:        newIntentsModel(),
		launch:         newLaunchModel(),
		cloudInit:      newCloudInitModel(),
		mirrors:        newMirrorsModel(),
		migration:      newMigrationModel(),
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// Init loads the Instances screen and starts the status-clearing tick.
func (m model) Init() tea.Cmd {
	return tea.Batch(loadInstances(m.client), tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.status != "" && time.Since(m.statusSetAt) > statusVisible {
			m.status = ""
		}
		cmds := []tea.Cmd{tickCmd()}
		if m.screen == screenInstances {
			if cmd := m.instances.maybeRefreshStats(m.client); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		contentWidth := msg.Width - sidebarWidth - sidebarGutter
		h := contentHeight(msg.Height)
		m.instances.setSize(contentWidth, h)
		m.snapshots.setSize(contentWidth, h)
		m.images.setSize(contentWidth, h)
		m.intents.list.SetSize(contentWidth-2, h)
		m.cloudInit.setSize(contentWidth, h)
		m.mirrors.list.SetSize(contentWidth-2, h)
		m.migration.setSize(contentWidth, h)
		m.migration.migrateForm.SetHeight(h - 2)
		// launch/logs are full-width takeovers, not squeezed by the sidebar.
		m.launch.setSize(msg.Width, contentHeight(msg.Height))
		m.logs.viewport.Width, m.logs.viewport.Height = msg.Width, logsViewportHeight(msg.Height)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.screen != screenLaunch && m.screen != screenLogs && m.sidebarFocused {
			return m.updateSidebar(msg)
		}
	}

	// Dispatch actionDoneMsg to the screen that originated the action, not necessarily m.screen.
	if adm, ok := msg.(actionDoneMsg); ok {
		switch adm.screen {
		case screenInstances:
			return m.updateInstances(msg)
		case screenSnapshots:
			return m.updateSnapshots(msg)
		case screenImages:
			return m.updateImages(msg)
		case screenIntents:
			return m.updateIntents(msg)
		case screenCloudInit:
			return m.updateCloudInit(msg)
		case screenMirrors:
			return m.updateMirrors(msg)
		case screenMigration:
			return m.updateMigration(msg)
		}
	}

	// Dispatch remaining messages and content-focused key presses to the active screen.
	switch m.screen {
	case screenInstances:
		return m.updateInstances(msg)
	case screenSnapshots:
		return m.updateSnapshots(msg)
	case screenImages:
		return m.updateImages(msg)
	case screenIntents:
		return m.updateIntents(msg)
	case screenLaunch:
		return m.updateLaunch(msg)
	case screenLogs:
		return m.updateLogs(msg)
	case screenCloudInit:
		return m.updateCloudInit(msg)
	case screenMirrors:
		return m.updateMirrors(msg)
	case screenMigration:
		return m.updateMigration(msg)
	}
	return m, nil
}

// updateSidebar handles up/down/enter/q while the sidebar has focus.
func (m model) updateSidebar(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	idx := screenIndex(m.screen)
	switch keyMsg.String() {
	case "up", "k":
		if idx > 0 {
			m.screen = screenOrder[idx-1]
			return m, loadCmdForScreen(m.screen, m.client)
		}
	case "down", "j":
		if idx < len(screenOrder)-1 {
			m.screen = screenOrder[idx+1]
			return m, loadCmdForScreen(m.screen, m.client)
		}
	case "enter", "right", "l":
		m.sidebarFocused = false
	case "q":
		return m, tea.Quit
	}
	return m, nil
}

func screenIndex(s screen) int {
	for i, o := range screenOrder {
		if o == s {
			return i
		}
	}
	return 0
}

func loadCmdForScreen(s screen, c *client.Client) tea.Cmd {
	switch s {
	case screenInstances:
		return loadInstances(c)
	case screenSnapshots:
		return loadInstances(c)
	case screenImages:
		return tea.Batch(loadCachedImages(c), loadCatalog(c))
	case screenIntents:
		return loadIntents(c)
	case screenCloudInit:
		return loadCloudInitList(c)
	case screenMirrors:
		return loadMirrors(c)
	case screenMigration:
		return loadHosts(c)
	}
	return nil
}

func (m model) View() string {
	header := styleTitle.Render(" anvil ") + styleSubtitle.Render(fmt.Sprintf("  %s  socket=%s", m.who, m.socket))
	if running, total := m.instances.counts(); total > 0 {
		header += styleSubtitle.Render(fmt.Sprintf("  •  %d/%d running", running, total))
	}

	var body string
	if m.screen == screenLaunch || m.screen == screenLogs {
		// Full-screen takeover: no sidebar.
		if m.screen == screenLaunch {
			body = m.launch.View()
		} else {
			body = m.logs.View()
		}
	} else {
		content := m.screenView()
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(), strings.Repeat(" ", sidebarGutter), content)
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

func (m model) screenView() string {
	switch m.screen {
	case screenInstances:
		return m.instances.View()
	case screenSnapshots:
		return m.snapshots.View()
	case screenImages:
		return m.images.View()
	case screenIntents:
		return m.intents.View()
	case screenCloudInit:
		return m.cloudInit.View()
	case screenMirrors:
		return m.mirrors.View()
	case screenMigration:
		return m.migration.View()
	}
	return ""
}

func (m model) sidebarView() string {
	var b strings.Builder
	for _, s := range screenOrder {
		label := screenLabels[s]
		switch {
		case s == m.screen && m.sidebarFocused:
			b.WriteString(styleMenuItemSelected.Render("▸ " + label))
		case s == m.screen:
			b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 2).Render(label))
		default:
			b.WriteString(styleMenuItem.Render("  " + label))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	if m.sidebarFocused {
		b.WriteString(styleHelp.Render("↑/↓ move\nenter open\nq quit"))
	} else {
		b.WriteString(styleHelp.Render("esc back here"))
	}
	return lipgloss.NewStyle().Width(sidebarWidth).Render(b.String())
}

// contentHeight leaves room for the header, spacing, and a status line.
func contentHeight(termHeight int) int {
	h := termHeight - 6
	if h < 3 {
		h = 3
	}
	return h
}

// logsViewportHeight is contentHeight minus the 3 chrome lines logsModel.View wraps its viewport in (title, blank line, help bar).
// Keeping this logic here keeps the on-open (instances.go) and on-resize heights in sync; disagreement here left stale lines when toggling Logs.
func logsViewportHeight(termHeight int) int {
	h := contentHeight(termHeight) - 3
	if h < 1 {
		h = 1
	}
	return h
}

// setStatus records a one-line status message for the view to render.
func (m *model) setStatus(text string, bad bool) {
	m.status, m.statusBad, m.statusSetAt = text, bad, time.Now()
}
