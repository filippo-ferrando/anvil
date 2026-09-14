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
// Layout is a persistent left sidebar plus the active page centered
// after it (Hyperpass's own shape, requested explicitly) rather than a
// full-screen menu you navigate away from and back to — the sidebar is
// always visible, `up`/`down` on it switches which page is showing, and
// `enter`/`right` moves keyboard focus into that page itself (`esc`
// hands focus back to the sidebar, not to a separate "menu" screen,
// since there isn't one anymore).
//
// Same honest caveat as before: no real TTY in this sandbox to run a
// terminal UI against, and no network access to fetch these three
// dependencies (see the Makefile's `tui-deps` target) — reviewed
// carefully, gofmt-clean, built against long-stable core APIs
// (Model/Update/View, bubbles' list/textinput/textarea, tea.ExecProcess
// for the shell handoff), but genuinely unexercised until a real
// `anvil tui` walkthrough on your own machine — a pty-based smoke test
// (see the session notes) did catch and fix several real rendering bugs
// this way, but it's not a substitute for that real walkthrough.
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

// statusVisible is how long a status line stays up before the recurring
// tick (see tickMsg) clears it — it used to just sit there forever until
// the next action overwrote it, which reads as stale rather than as
// feedback for whatever just happened.
const statusVisible = 4 * time.Second

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type screen int

const (
	screenInstances screen = iota
	screenImages
	screenIntents
	screenCloudInit
	screenMirrors
	screenMigration
	screenLaunch // not in the sidebar: a full-screen takeover reached via Instances' "n", not a nav destination
	screenLogs   // same: reached via Instances' "l"
)

// screenOrder is the sidebar's own list, top to bottom.
var screenOrder = []screen{screenInstances, screenImages, screenIntents, screenCloudInit, screenMirrors, screenMigration}

var screenLabels = map[screen]string{
	screenInstances: "Instances",
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

// model is the whole application's state — one Bubble Tea model, per the
// framework's own architecture, with a field per screen holding that
// screen's own state. Only the active screen's Update/View actually run;
// the others just sit idle, cheap to keep around.
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
	statusSetAt time.Time // for the recurring tick in Update to know when to clear it

	instances instancesModel
	images    imagesModel
	intents   intentsModel
	launch    launchModel
	logs      logsModel
	cloudInit cloudInitModel
	mirrors   mirrorsModel
	migration migrationModel
}

// Run dials socketPath and blocks running the TUI until the user quits
// (q on the sidebar, or Ctrl+C anywhere) or an unrecoverable error
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
		client:         c,
		socket:         socketPath,
		who:            who,
		screen:         screenInstances,
		sidebarFocused: true,
		instances:      newInstancesModel(),
		images:         newImagesModel(),
		intents:        newIntentsModel(),
		cloudInit:      newCloudInitModel(),
		mirrors:        newMirrorsModel(),
		migration:      newMigrationModel(),
	}

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func (m model) Init() tea.Cmd {
	// The sidebar starts on Instances; load it without waiting for a
	// keypress. tickCmd starts the recurring clock that clears m.status
	// a few seconds after it's set (see statusVisible) — one ongoing
	// command instead of threading a per-call tea.Cmd through every one
	// of setStatus's many call sites.
	return tea.Batch(loadInstances(m.client), tickCmd())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.status != "" && time.Since(m.statusSetAt) > statusVisible {
			m.status = ""
		}
		return m, tickCmd()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		contentWidth := msg.Width - sidebarWidth - sidebarGutter
		h := contentHeight(msg.Height)
		// Instances/Mirrors render their list unboxed (no border), so
		// this is just a small side margin, not a border correction —
		// see boxOverhead in cloudinit.go for the screens that do wrap
		// panels in styleBox.
		m.instances.list.SetSize(contentWidth-2, h)
		m.images.setSize(contentWidth, h)
		m.intents.list.SetSize(contentWidth-2, h)
		m.cloudInit.setSize(contentWidth, h)
		m.mirrors.list.SetSize(contentWidth-2, h)
		m.migration.setSize(contentWidth, h)
		m.migration.migrateForm.SetHeight(h - 2)
		// launch/logs are both full-width takeovers, not squeezed by the sidebar.
		m.launch.form.SetHeight(contentHeight(msg.Height) - 2)
		m.logs.viewport.Width, m.logs.viewport.Height = msg.Width, contentHeight(msg.Height)-2
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.screen != screenLaunch && m.screen != screenLogs && m.sidebarFocused {
			return m.updateSidebar(msg)
		}
	}

	// Non-key messages (background RPC results arriving from a tea.Cmd)
	// and content-focused key presses both dispatch to the active
	// screen's own Update — a data message has to reach its screen
	// regardless of where keyboard focus currently is, e.g. a list
	// that's still loading while the user has already moved the sidebar
	// on to something else.
	switch m.screen {
	case screenInstances:
		return m.updateInstances(msg)
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

// updateSidebar handles up/down/enter/q while the sidebar has focus —
// switching m.screen re-triggers that screen's own load command, so
// data's never more than one navigation away from fresh.
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

	var body string
	if m.screen == screenLaunch || m.screen == screenLogs {
		// A full-screen takeover: no sidebar, matching how each is reached
		// (Instances' "n"/"l") and left (Esc, back to Instances).
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
	m.status, m.statusBad, m.statusSetAt = text, bad, time.Now()
}
