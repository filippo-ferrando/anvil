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

	"git.fferrando.cc/rdfilippo/anvil/internal/termbg"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
	screenManifests
	screenCloudInit
	screenMirrors
	screenMigration
	screenEvents
	screenDevices
	screenGUIApps
	screenEphemeral
	screenCI
	screenTasks
	screenLaunch // full-screen takeover reached via Instances' "n"
	screenLogs   // full-screen takeover reached via Instances' "l"
)

// screenOrder is the sidebar's own list, top to bottom.
var screenOrder = []screen{
	screenInstances, screenEphemeral, screenSnapshots,
	screenIntents, screenManifests, screenCI, screenGUIApps,
	screenImages, screenCloudInit, screenMirrors,
	screenMigration, screenDevices, screenEvents, screenTasks,
}

// screenGroups heads the sidebar's sections, keyed by the screen each one starts at.
var screenGroups = map[screen]string{
	screenInstances: "WORKLOADS",
	screenIntents:   "AUTOMATION",
	screenImages:    "LIBRARY",
	screenMigration: "HOST",
}

// screenKeys are the jump keys of the sidebar entries, in screenOrder's order.
const screenKeys = "1234567890abcd"

var screenLabels = map[screen]string{
	screenInstances: "Instances",
	screenSnapshots: "Snapshots",
	screenImages:    "Images",
	screenIntents:   "Intents",
	screenManifests: "Manifests",
	screenCloudInit: "Cloud-Init",
	screenMirrors:   "Mirrors",
	screenMigration: "Migration",
	screenEvents:    "Events",
	screenDevices:   "Devices",
	screenGUIApps:   "GUI Apps",
	screenEphemeral: "Ephemeral",
	screenCI:        "CI",
	screenTasks:     "Tasks",
}

const (
	sidebarWidth  = 20
	sidebarGutter = 2
)

// model is the whole application's Bubble Tea state, with one field per screen.
type model struct {
	client     *client.Client
	socket     string
	socketPath string // the daemon socket as given, for anvil commands run from the TUI
	who        string // local OS username@hostname, shown in the header

	daemonState   string // "" before the first reply, then "ok" or "down", from the last instance list
	daemonVersion string // from the Version RPC, empty until it answers
	clientVersion string // this binary's own version

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
	manifests manifestsModel
	mirrors   mirrorsModel
	migration migrationModel
	events    eventsModel
	devices   devicesModel
	guiApps   guiAppsModel
	ephemeral ephemeralModel
	ci        ciModel
	tasks     tasksModel

	quitAsked time.Time // when q was pressed with tasks running; a second press quits

	watchReloadPending bool // a reload triggered by a Watch event is already scheduled
	watchFullReload    bool // a pending Watch event couldn't be patched into the Instances list
	watchSelectedHit   bool // a pending Watch event touched the selected instance
}

// Run dials anvild with dial and blocks running the TUI until the user quits or an error
// occurs. socketPath is the daemon socket, passed on to anvil commands run from the TUI.
func Run(dial func() (*client.Client, error), socketPath string) error {
	c, err := dial()
	if err != nil {
		return fmt.Errorf("tui: %w", err) // the dial error already names the socket or host
	}
	defer c.Close()
	addr := socketPath
	if c.Remote != "" {
		addr = c.Remote + ":" + socketPath
	}

	who := "unknown"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		who += "@" + h
	}

	m := model{
		client:         c,
		socket:         addr,
		socketPath:     socketPath,
		who:            who,
		clientVersion:  clientVersion(),
		screen:         screenInstances,
		sidebarFocused: true,
		instances:      newInstancesModel(),
		snapshots:      newSnapshotsModel(),
		images:         newImagesModel(),
		intents:        newIntentsModel(),
		launch:         newLaunchModel(),
		cloudInit:      newCloudInitModel(),
		manifests:      newManifestsModel(),
		mirrors:        newMirrorsModel(),
		ephemeral:      newEphemeralModel(),
		ci:             newCIModel(),
		migration:      newMigrationModel(),
		events:         newEventsModel(),
		devices:        newDevicesModel(),
		guiApps:        newGUIAppsModel(),
	}

	termbg.Detect()
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// Init loads the Instances screen, starts the status-clearing tick and subscribes to instance changes.
func (m model) Init() tea.Cmd {
	return tea.Batch(loadInstances(m.client), tickCmd(), startWatch(m.client), loadVersion(m.client))
}

// Update closes the stats stream while the Instances screen is hidden and reopens it on return.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	nm, ok := next.(model)
	if !ok {
		return next, cmd
	}
	switch {
	case nm.screen != screenInstances && nm.instances.statsStreaming != "":
		nm.instances.closeStatsStream()
	case nm.screen == screenInstances && m.screen != screenInstances:
		reload := nm.reloadSelected()
		cmd = tea.Batch(cmd, reload)
	}
	return nm, cmd
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		visible := statusVisible
		if m.statusBad {
			visible = errorVisible
		}
		if m.status != "" && time.Since(m.statusSetAt) > visible {
			m.status = ""
		}
		return m, tickCmd()
	case watchEventMsg:
		if msg.err != nil {
			// Daemon restarted or too old for Watch: try again later, the TUI still works without it.
			return m, tea.Tick(watchRetryInterval, func(time.Time) tea.Msg { return watchRetryMsg{} })
		}
		cmds := []tea.Cmd{recvWatch(msg.stream)}
		// The Instances list is patched from the event itself; other screens reload.
		prev := m.instances.selectedName()
		if m.screen != screenInstances || !m.instances.patch(msg.ev) {
			m.watchFullReload = true
		} else if msg.ev.GetInstance().GetName() == prev || m.instances.selectedName() != prev {
			m.watchSelectedHit = true
		}
		if !m.watchReloadPending {
			// Bursts of events (a launch changes state several times) collapse into one reload.
			m.watchReloadPending = true
			cmds = append(cmds, tea.Tick(watchReloadDelay, func(time.Time) tea.Msg { return watchReloadMsg{} }))
		}
		return m, tea.Batch(cmds...)
	case watchRetryMsg:
		return m, startWatch(m.client)
	case watchReloadMsg:
		full, selected := m.watchFullReload, m.watchSelectedHit
		m.watchReloadPending, m.watchFullReload, m.watchSelectedHit = false, false, false
		if m.screen == screenInstances && !full {
			if !selected {
				return m, nil
			}
			cmd := m.reloadSelected()
			return m, cmd
		}
		switch m.screen {
		case screenInstances, screenSnapshots, screenIntents, screenEvents:
			return m, m.loadCmdForScreen(m.screen)
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
		m.manifests.setSize(contentWidth, h)
		m.mirrors.list.SetSize(contentWidth-2, h)
		m.ephemeral.setSize(contentWidth, h)
		m.ci.setSize(contentWidth-2, h-1)
		m.tasks.setSize(contentWidth-2, h)
		m.events.list.SetSize(contentWidth-2, h-1) // -1: the help bar under the list
		m.devices.list.SetSize(contentWidth-2, h-1)
		m.guiApps.list.SetSize(contentWidth-2, h-1)
		m.migration.setSize(contentWidth, h)
		m.migration.migrateForm.SetHeight(h - 2)
		// launch/logs are full-width takeovers, not squeezed by the sidebar.
		m.launch.setSize(msg.Width, contentHeight(msg.Height))
		m.logs.viewport.Width, m.logs.viewport.Height = msg.Width, logsViewportHeight(msg.Height)
		return m, nil
	case versionMsg:
		m.daemonVersion = msg.version
		return m, nil
	case instancesLoadedMsg:
		m.daemonState = "ok"
		if msg.err != nil {
			m.daemonState = "down"
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m.quit()
		}
		// alt+<jump key> jumps to a screen from anywhere except the full-screen takeovers.
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
	switch typed := msg.(type) {
	case statsStreamMsg, instanceEventsLoadedMsg, selectionSettledMsg:
		return m.updateInstances(msg)
	case snapshotsSettledMsg:
		return m.updateSnapshots(msg)
	case eventsLoadedMsg:
		return m.updateEvents(msg)
	case imageChecksumMsg:
		return m.updateImages(msg)
	case applyStreamMsg:
		if typed.owner == screenManifests {
			return m.updateManifests(msg)
		}
		return m.updateIntents(msg)
	case manifestSavedMsg, manifestContentLoadedMsg:
		return m.updateManifests(msg)
	case warmLoadedMsg, clonesLoadedMsg, runEventMsg:
		return m.updateEphemeral(msg) // streams keep going while another screen shows
	case ciLoadedMsg, pipelineEventMsg, ciLogsMsg:
		return m.updateCI(msg)
	case hostTestedMsg, hostsDiscoveredMsg:
		return m.updateMigration(msg)
	case cloudInitImportStreamMsg, cloudInitSavedMsg:
		return m.updateCloudInit(msg)
	case yamlCheckDueMsg, yamlCheckedMsg:
		return m.updateYAMLCheck(msg)
	case taskDoneMsg, taskStepMsg, taskLogMsg, taskLogRetryMsg, taskLogRenderMsg:
		return m.updateTasks(msg) // tasks run on whatever screen shows
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
		case screenManifests:
			return m.updateManifests(msg)
		case screenCloudInit:
			return m.updateCloudInit(msg)
		case screenMirrors:
			return m.updateMirrors(msg)
		case screenMigration:
			return m.updateMigration(msg)
		case screenEphemeral:
			return m.updateEphemeral(msg)
		case screenCI:
			return m.updateCI(msg)
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
	case screenManifests:
		return m.updateManifests(msg)
	case screenCloudInit:
		return m.updateCloudInit(msg)
	case screenMirrors:
		return m.updateMirrors(msg)
	case screenMigration:
		return m.updateMigration(msg)
	case screenEvents:
		return m.updateEvents(msg)
	case screenDevices:
		return m.updateDevices(msg)
	case screenGUIApps:
		return m.updateGUIApps(msg)
	case screenEphemeral:
		return m.updateEphemeral(msg)
	case screenCI:
		return m.updateCI(msg)
	case screenTasks:
		return m.updateTasks(msg)
	}
	return m, nil
}

// quitWindow is how long a first quit with tasks running waits for the second one.
const quitWindow = 3 * time.Second

// quit leaves the TUI, asking first while tasks run: leaving ends their requests.
func (m model) quit() (tea.Model, tea.Cmd) {
	n := len(m.tasks.running())
	if n == 0 || time.Since(m.quitAsked) < quitWindow {
		return m, tea.Quit
	}
	m.quitAsked = time.Now()
	m.setStatus(fmt.Sprintf("%d task(s) still running, quitting cancels them (a launch is rolled back): press again to quit", n), true)
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
		return m, m.loadCmdForScreen(m.screen)
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
		return m.quit()
	default:
		if s, ok := screenForDigit(k); ok {
			return m.jumpTo(s)
		}
	}
	return m, nil
}

// screenForDigit maps a jump key of screenKeys to the sidebar entry at its position.
func screenForDigit(k string) (screen, bool) {
	if len(k) != 1 {
		return 0, false
	}
	i := strings.IndexByte(screenKeys, k[0])
	if i < 0 || i >= len(screenOrder) {
		return 0, false
	}
	return screenOrder[i], true
}

// screenDigit is the key that jumps to the sidebar entry at index i, blank past the last key.
func screenDigit(i int) string {
	if i < len(screenKeys) {
		return screenKeys[i : i+1]
	}
	return " "
}

// jumpTo opens s with focus on its content, reloading it.
func (m model) jumpTo(s screen) (tea.Model, tea.Cmd) {
	m.screen, m.sidebarFocused = s, false
	return m, m.loadCmdForScreen(s)
}

func screenIndex(s screen) int {
	for i, o := range screenOrder {
		if o == s {
			return i
		}
	}
	return 0
}

func (m model) loadCmdForScreen(s screen) tea.Cmd {
	c := m.client
	switch s {
	case screenInstances:
		return loadInstances(c)
	case screenSnapshots:
		return loadInstances(c)
	case screenImages:
		return tea.Batch(loadCachedImages(c), loadCatalog(c))
	case screenIntents:
		return loadIntents(c)
	case screenManifests:
		return loadManifestList(c)
	case screenCloudInit:
		return loadCloudInitList(c)
	case screenMirrors:
		return loadMirrors(c)
	case screenEphemeral:
		return loadEphemeral(c)
	case screenCI:
		return loadCI(c)
	case screenMigration:
		// Instances and intents feed the migrate form's Name suggestions.
		return tea.Batch(loadHosts(c), loadInstances(c), loadIntents(c))
	case screenEvents:
		return m.events.load(c)
	case screenDevices:
		return loadHostDevices(c)
	case screenGUIApps:
		return loadGUIApps()
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

// clientVersion is this binary's version as the header shows it.
func clientVersion() string {
	v, _ := version.Get()
	return v
}

// headerView is the top bar: app name and version, user, socket, then daemon state, instance counts and clock on the right.
func (m model) headerView() string {
	left := styleTitle.Render("⚒ anvil")
	if m.clientVersion != "" {
		left += styleSubtitle.Render(" " + m.clientVersion)
	}
	left += styleSubtitle.Render("  " + m.who + "  " + m.socket)

	var right []string
	switch m.daemonState {
	case "ok":
		label := "● daemon connected"
		if m.daemonVersion != "" {
			label += " " + m.daemonVersion
		}
		right = append(right, styleGood.Render(label))
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
		if strip := m.tasks.stripView(m.width - 1); strip != "" {
			return "\n" + strip
		}
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
	} else if strip := m.tasks.stripView(m.width - 1); strip != "" {
		s += "\n" + strip // the status line doesn't hide what is still running
	}
	return s
}

func (m model) screenView() string {
	switch m.screen {
	case screenInstances:
		return m.instances.viewWith(m.tasks)
	case screenSnapshots:
		return m.snapshots.View()
	case screenImages:
		return m.images.View()
	case screenIntents:
		return m.intents.View()
	case screenManifests:
		return m.manifests.View()
	case screenCloudInit:
		return m.cloudInit.View()
	case screenMirrors:
		return m.mirrors.View()
	case screenEphemeral:
		return m.ephemeral.View(m.width-sidebarWidth-sidebarGutter, contentHeight(m.height), m.tasks.get(m.ephemeral.watching))
	case screenCI:
		return m.ci.View()
	case screenTasks:
		return m.tasks.View()
	case screenMigration:
		return m.migration.View()
	case screenEvents:
		return m.events.View()
	case screenDevices:
		return m.devices.View()
	case screenGUIApps:
		return m.guiApps.View()
	}
	return ""
}

func (m model) sidebarView() string {
	h := contentHeight(m.height)
	// On a short terminal the gaps go first, then the section headers, so every entry stays.
	n := len(screenOrder)
	headers, gaps := n+len(screenGroups) <= h, n+2*len(screenGroups)-1 <= h
	var lines []string
	for i, s := range screenOrder {
		if g, ok := screenGroups[s]; ok && headers {
			if i > 0 && gaps {
				lines = append(lines, "")
			}
			lines = append(lines, styleSubtitle.Render(" "+g))
		}
		label := screenDigit(i) + " " + screenLabels[s]
		if n := len(m.tasks.running()); s == screenTasks && n > 0 {
			label += fmt.Sprintf(" ⟳%d", n)
		}
		switch {
		case s == m.screen && m.sidebarFocused:
			lines = append(lines, styleMenuItemSelected.Render("▸ "+label))
		case s == m.screen:
			lines = append(lines, lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Padding(0, 1).Render("▸ "+label))
		default:
			lines = append(lines, styleMenuItem.Render("  "+label))
		}
	}
	var keys []string
	if m.sidebarFocused {
		keys = helpItems("↑↓", "move", "1-9 0 a-d", "jump", "enter", "open", "q", "quit")
	} else {
		keys = helpItems("esc", "menu", "alt+key", "jump")
	}
	// The key help goes under the entries only when it fits; the entries always come first.
	if len(lines)+1+len(keys) <= h {
		lines = append(lines, "")
		for _, k := range keys {
			lines = append(lines, " "+k)
		}
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return lipgloss.NewStyle().
		Width(sidebarWidth - 1).Height(h).
		BorderStyle(lipgloss.NormalBorder()).BorderRight(true).BorderForeground(colorAccentDim).
		Render(strings.Join(lines, "\n"))
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
