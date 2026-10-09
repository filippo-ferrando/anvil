// Package tui implements `anvil tui`, a Bubble Tea terminal UI built on
// the same pkg/client wrapper the CLI uses.
package tui

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anvil-project/anvil/pkg/client"
)

// statusVisible and errorVisible are how long a status line stays up before being cleared.
const (
	statusVisible = 4 * time.Second
	errorVisible  = 10 * time.Second
)

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
	who    string // local OS username@hostname, shown in the header

	daemonState string // "" before the first reply, then "ok" or "down", from the last instance list

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

	watchReloadPending bool // a reload triggered by a Watch event is already scheduled
}

// Run dials socketPath (on the ssh host remote, if set) and blocks running
// the TUI until the user quits or an error occurs.
func Run(socketPath, remote string) error {
	dial := func() (*client.Client, error) { return client.Dial(socketPath) }
	if remote != "" {
		dial = func() (*client.Client, error) { return client.DialSSH(remote, socketPath) }
		socketPath = remote + ":" + socketPath
	}
	c, err := dial()
	if err != nil {
		return fmt.Errorf("tui: dialing anvild at %s: %w", socketPath, err)
	}
	defer c.Close()

	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		who += "@" + h
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

// Init loads the Instances screen, starts the status-clearing tick and subscribes to instance changes.
func (m model) Init() tea.Cmd {
	return tea.Batch(loadInstances(m.client), tickCmd(), startWatch(m.client))
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		visible := statusVisible
		if m.statusBad {
			visible = errorVisible
		}
		if m.status != "" && time.Since(m.statusSetAt) > visible {
			m.status = ""
		}
		cmds := []tea.Cmd{tickCmd()}
		if m.screen == screenInstances {
			if cmd := m.instances.maybeRefreshStats(m.client); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		return m, tea.Batch(cmds...)
	case watchEventMsg:
		if msg.err != nil {
			// Daemon restarted or too old for Watch: try again later, the TUI still works without it.
			return m, tea.Tick(watchRetryInterval, func(time.Time) tea.Msg { return watchRetryMsg{} })
		}
		cmds := []tea.Cmd{recvWatch(msg.stream)}
		if !m.watchReloadPending {
			// Bursts of events (a launch changes state several times) collapse into one reload.
			m.watchReloadPending = true
			cmds = append(cmds, tea.Tick(watchReloadDelay, func(time.Time) tea.Msg { return watchReloadMsg{} }))
		}
		return m, tea.Batch(cmds...)
	case watchRetryMsg:
		return m, startWatch(m.client)
	case watchReloadMsg:
		m.watchReloadPending = false
		switch m.screen {
		case screenInstances, screenSnapshots, screenIntents:
			return m, loadCmdForScreen(m.screen, m.client)
		}
		return m, nil
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
	case instancesLoadedMsg:
		m.daemonState = "ok"
		if msg.err != nil {
			m.daemonState = "down"
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// alt+1..9 jumps to a screen from anywhere except the full-screen takeovers.
		if k := msg.String(); m.screen != screenLaunch && m.screen != screenLogs && strings.HasPrefix(k, "alt+") {
			if s, ok := screenForDigit(strings.TrimPrefix(k, "alt+")); ok {
				return m.jumpTo(s)
			}
		}
		if m.screen != screenLaunch && m.screen != screenLogs && m.sidebarFocused {
			return m.updateSidebar(msg)
		}
	}

	// Replies to long-running requests go to the screen that made them, even after the user moved on.
	switch msg.(type) {
	case waitStreamMsg:
		return m.updateInstances(msg)
	case imageChecksumMsg:
		return m.updateImages(msg)
	case applyStreamMsg:
		return m.updateIntents(msg)
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
	idx, n := screenIndex(m.screen), len(screenOrder)
	move := func(i int) (tea.Model, tea.Cmd) {
		m.screen = screenOrder[(i+n)%n]
		return m, loadCmdForScreen(m.screen, m.client)
	}
	switch k := keyMsg.String(); k {
	case "up", "k", "shift+tab":
		return move(idx - 1)
	case "down", "j", "tab":
		return move(idx + 1)
	case "home", "g":
		return move(0)
	case "end", "G":
		return move(n - 1)
	case "enter", "right", "l":
		m.sidebarFocused = false
	case "q":
		return m, tea.Quit
	default:
		if s, ok := screenForDigit(k); ok {
			return m.jumpTo(s)
		}
	}
	return m, nil
}

// screenForDigit maps "1".."9" to the sidebar entry at that position.
func screenForDigit(k string) (screen, bool) {
	if len(k) != 1 || k[0] < '1' || int(k[0]-'1') >= len(screenOrder) {
		return 0, false
	}
	return screenOrder[k[0]-'1'], true
}

// jumpTo opens s with focus on its content, reloading it.
func (m model) jumpTo(s screen) (tea.Model, tea.Cmd) {
	m.screen, m.sidebarFocused = s, false
	return m, loadCmdForScreen(s, m.client)
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
	header := m.headerView()

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

	return header + "\n\n" + body + m.statusView()
}

// headerView is the top bar: app name, user, socket, then daemon state, instance counts and clock on the right.
func (m model) headerView() string {
	left := styleTitle.Render("⚒ anvil") + styleSubtitle.Render("  "+m.who+"  "+m.socket)

	var right []string
	switch m.daemonState {
	case "ok":
		right = append(right, styleGood.Render("● daemon connected"))
	case "down":
		right = append(right, styleError.Render("● daemon unreachable"))
	default:
		right = append(right, styleWarn.Render("● connecting…"))
	}
	if running, total := m.instances.counts(); total > 0 {
		right = append(right, fmt.Sprintf("%s %s",
			styleGood.Render(fmt.Sprint(running)), styleSubtitle.Render(fmt.Sprintf("of %d running", total))))
	}
	right = append(right, styleSubtitle.Render(time.Now().Format("15:04:05")))
	r := strings.Join(right, styleSubtitle.Render("  │  ")) + " "

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(r)
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + r
}

// statusView renders the transient status line: a colored badge, the message on one line, and a hint for known errors.
func (m model) statusView() string {
	if m.status == "" {
		return ""
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(colorDark).Padding(0, 1)
	msg, hint, style := m.status, "", styleGood
	if m.statusBad {
		msg, hint = friendlyError(m.status)
		badge, style = badge.Background(colorBad).SetString("✕ ERROR"), styleError
	} else {
		badge = badge.Background(colorGood).SetString("✓")
	}
	b := badge.String()
	msg = strings.Join(strings.Fields(msg), " ")
	if w := m.width - lipgloss.Width(b) - 2; w > 0 {
		style = style.MaxWidth(w)
	}
	s := "\n" + b + " " + style.Render(msg)
	if hint != "" {
		s += "\n" + strings.Repeat(" ", lipgloss.Width(b)+1) + styleSubtitle.Render("hint: "+hint)
	}
	return s
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
	b.WriteString(styleSubtitle.Render(" MENU") + "\n\n")
	for i, s := range screenOrder {
		label := fmt.Sprintf("%d %s", i+1, screenLabels[s])
		switch {
		case s == m.screen && m.sidebarFocused:
			b.WriteString(styleMenuItemSelected.Render("▸ " + label))
		case s == m.screen:
			b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 1).Render("▸ " + label))
		default:
			b.WriteString(styleMenuItem.Render("  " + label))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	var keys []string
	if m.sidebarFocused {
		keys = helpItems("↑↓", "move", "1-7", "jump", "enter", "open", "q", "quit")
	} else {
		keys = helpItems("esc", "menu", "alt+1-7", "jump")
	}
	for _, k := range keys {
		b.WriteString(" " + k + "\n")
	}
	return lipgloss.NewStyle().
		Width(sidebarWidth - 1).Height(contentHeight(m.height)).
		BorderStyle(lipgloss.NormalBorder()).BorderRight(true).BorderForeground(colorAccentDim).
		Render(b.String())
}

// contentHeight leaves room for the header, spacing, and a two-line status area.
func contentHeight(termHeight int) int {
	h := termHeight - 7
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
