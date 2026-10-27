package tui

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

type instanceItem struct {
	inst   *anvilv1.Instance
	marked bool // picked with space for a multi-instance start/stop/delete
}

// stateGlyph is a plain, uncolored marker: bubbles' list.DefaultDelegate re-styles selected items wholesale, and an
// embedded ANSI reset from a colored Render() would cut that off mid-line. See stateDot, used only in the detail panel.
func stateGlyph(s anvilv1.State) string {
	switch s {
	case anvilv1.State_STATE_RUNNING:
		return "●"
	case anvilv1.State_STATE_STARTING, anvilv1.State_STATE_STOPPING:
		return "◐"
	case anvilv1.State_STATE_ERROR:
		return "✕"
	default:
		return "○"
	}
}

func (i instanceItem) FilterValue() string { return i.inst.GetName() }
func (i instanceItem) Title() string {
	mark := ""
	if i.marked {
		mark = "✓ "
	}
	return mark + stateGlyph(i.inst.GetState()) + " " + i.inst.GetName()
}
func (i instanceItem) Description() string {
	kind, image := "vm", i.inst.GetVm().GetImageRef()
	if i.inst.GetVm().GetHypervisor() == anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER {
		kind = "vm/firecracker"
	}
	if i.inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		kind, image = "container", i.inst.GetContainer().GetImageRef()
	}
	desc := fmt.Sprintf("%s  •  %s  •  %s", kind, format.State(i.inst.GetState()), image)
	if ip := format.InstanceIP(i.inst); ip != "" {
		desc += "  •  " + ip
	}
	return desc
}

// instancesPrompt identifies which overlay form, if any, is showing over the instances list.
type instancesPrompt int

const (
	instancesPromptNone instancesPrompt = iota
	instancesPromptMount
	instancesPromptUmount
	instancesPromptExec
	instancesPromptShellUser
	instancesPromptExport
	instancesPromptImport
	instancesPromptAddPort
	instancesPromptRemovePort
	instancesPromptFork
	instancesPromptSettings
	instancesPromptGUI
	instancesPromptStop
	instancesPromptLogs
)

type instancesModel struct {
	list          list.Model
	confirmDelete []*anvilv1.Instance // non-empty while the delete confirmation overlay is up
	loading       bool

	marked    map[string]bool // instance IDs picked with space
	stopNames []string        // what the stop options prompt stops

	prompt         instancesPrompt
	promptTarget   *anvilv1.Instance
	promptForm     simpleForm
	lastShellUser  string // remembered across shell/exec calls, pre-filled into the user field
	lastIdentity   string // remembered across shell/exec/gui calls, pre-filled into the identity field
	lastGUICommand string // remembered across GUI launches
	lastGUIX11     bool
	lastLogsTail   string // remembered across logs calls

	// Export, import and fork progress; a running one blocks other keys.
	export, imp, fork transcript

	waiting    bool               // a WaitReady stream is in flight; only esc (cancel) works
	waitLines  []string           // the finished (or in-flight) wait's progress transcript
	waitSince  time.Time          // when the last wait line's step began
	waitCancel context.CancelFunc // ends the in-flight wait

	detailWidth  int // width of the detail panel next to the list
	panelHeight  int // shared height for both side-by-side panels
	contentWidth int // full width given to this screen, for wrapping the help bar

	// Live stats for whichever instance is selected; see syncStatsStream/statsStreamMsg. statsFor names which
	// instance stats/statsErr belong to, so a stale reply is never shown against the wrong row.
	stats          *anvilv1.InstanceStats
	statsFor       string
	statsErr       string
	statsStreaming string             // instance the open stats stream is for, "" when none is open
	statsGen       int                // bumped per stream, see statsStreamMsg
	statsCancel    context.CancelFunc // closes the open stream

	// Recent events of the selected instance, for the detail panel; eventsFor names which.
	events    []*anvilv1.InstanceEvent
	eventsFor string
}

func newInstancesModel() instancesModel {
	l := newList()
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Instances"
	l.SetShowHelp(false) // one consistent helpBar instead of list's own
	return instancesModel{list: l, loading: true}
}

func (m *instancesModel) setSize(width, height int) {
	m.contentWidth = width
	const gutter = 2
	inner := width - 2*boxOverhead - gutter
	if inner < 20 {
		inner = 20
	}
	listWidth := inner * 2 / 5
	if listWidth < 22 {
		listWidth = 22
	}
	m.detailWidth = inner - listWidth
	if m.detailWidth < 24 {
		m.detailWidth = 24
	}
	m.panelHeight = height - boxHeightOverhead - lipgloss.Height(helpBarWrap(width, instancesHelp...)) + 1
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}
	m.list.SetSize(listWidth, m.panelHeight)
}

// counts summarizes how many known instances are currently running, for the header.
func (m *instancesModel) counts() (running, total int) {
	for _, it := range m.list.Items() {
		if ii, ok := it.(instanceItem); ok {
			total++
			if ii.inst.GetState() == anvilv1.State_STATE_RUNNING {
				running++
			}
		}
	}
	return running, total
}

// selected returns the currently highlighted instance, or nil if the list is empty.
func (m *instancesModel) selected() *anvilv1.Instance {
	item, ok := m.list.SelectedItem().(instanceItem)
	if !ok {
		return nil
	}
	return item.inst
}

// toggleMark marks or unmarks the selected instance.
func (m *instancesModel) toggleMark() {
	item, ok := m.list.SelectedItem().(instanceItem)
	if !ok {
		return
	}
	if m.marked == nil {
		m.marked = map[string]bool{}
	}
	id := item.inst.GetId()
	if m.marked[id] {
		delete(m.marked, id)
	} else {
		m.marked[id] = true
	}
	item.marked = m.marked[id]
	m.list.SetItem(m.list.Index(), item)
}

// clearMarks unmarks every instance.
func (m *instancesModel) clearMarks() {
	m.marked = nil
	for i, it := range m.list.Items() {
		if item, ok := it.(instanceItem); ok && item.marked {
			item.marked = false
			m.list.SetItem(i, item)
		}
	}
}

// targets returns the marked instances in list order, or the selected one when none is marked.
func (m *instancesModel) targets() []*anvilv1.Instance {
	var out []*anvilv1.Instance
	for _, it := range m.list.Items() {
		if item, ok := it.(instanceItem); ok && m.marked[item.inst.GetId()] {
			out = append(out, item.inst)
		}
	}
	if len(out) == 0 {
		if inst := m.selected(); inst != nil {
			out = append(out, inst)
		}
	}
	return out
}

// setInstances replaces the list, keeping marks only on instances that still exist.
func (m *instancesModel) setInstances(insts []*anvilv1.Instance) {
	items := make([]list.Item, len(insts))
	kept := map[string]bool{}
	for i, inst := range insts {
		marked := m.marked[inst.GetId()]
		if marked {
			kept[inst.GetId()] = true
		}
		items[i] = instanceItem{inst: inst, marked: marked}
	}
	m.marked = kept
	m.list.SetItems(items)
}

func instanceNames(insts []*anvilv1.Instance) []string {
	names := make([]string, len(insts))
	for i, inst := range insts {
		names[i] = inst.GetName()
	}
	return names
}

func (m *instancesModel) selectedName() string {
	return m.selected().GetName()
}

// patch applies a Watch event to the list in place. It returns false when the list
// needs a full reload instead: a new instance, whose place in the order isn't known.
func (m *instancesModel) patch(ev *anvilv1.WatchEvent) bool {
	inst := ev.GetInstance()
	if inst == nil || m.list.FilterState() != list.Unfiltered {
		return false
	}
	for i, item := range m.list.Items() {
		it, ok := item.(instanceItem)
		if !ok || it.inst.GetId() != inst.GetId() {
			continue
		}
		if ev.GetType() == anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED {
			delete(m.marked, inst.GetId())
			m.list.RemoveItem(i)
		} else {
			m.list.SetItem(i, instanceItem{inst: inst, marked: it.marked})
		}
		return true
	}
	return ev.GetType() == anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED
}

// syncStatsStream keeps one stats stream open for the selected instance while it runs,
// closing the stream of a previous selection. Returns nil when nothing changes.
func (m *instancesModel) syncStatsStream(c *client.Client) tea.Cmd {
	want := ""
	if inst := m.selected(); inst != nil && inst.GetState() == anvilv1.State_STATE_RUNNING {
		want = inst.GetName()
	}
	if want == m.statsStreaming {
		return nil
	}
	m.closeStatsStream()
	m.stats, m.statsErr, m.statsFor = nil, "", ""
	if want == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.statsStreaming, m.statsCancel = want, cancel
	m.statsGen++
	return openStatsStream(ctx, c, m.statsGen, want)
}

func (m *instancesModel) closeStatsStream() {
	if m.statsCancel != nil {
		m.statsCancel()
	}
	m.statsStreaming, m.statsCancel = "", nil
}

// reloadSelected refreshes what the detail panel shows live for the selected instance.
func (m *model) reloadSelected() tea.Cmd {
	cmds := []tea.Cmd{m.instances.syncStatsStream(m.client)}
	if inst := m.instances.selected(); inst != nil {
		cmds = append(cmds, loadInstanceEvents(m.client, inst.GetName()))
	}
	return tea.Batch(cmds...)
}

func (m model) updateInstances(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case instancesLoadedMsg:
		m.instances.loading = false
		if msg.err != nil {
			m.setStatus("listing instances: "+msg.err.Error(), true)
			return m, nil
		}
		m.instances.setInstances(msg.instances)
		cmd := m.reloadSelected()
		return m, cmd

	case selectionSettledMsg:
		if inst := m.instances.selected(); m.screen != screenInstances || inst == nil || inst.GetName() != msg.name {
			return m, nil // the cursor moved on, or the screen did
		}
		cmd := m.reloadSelected()
		return m, cmd

	case statsStreamMsg:
		ins := &m.instances
		if msg.gen != ins.statsGen || ins.statsStreaming == "" {
			return m, nil // stale: that stream was closed after the selection moved on
		}
		name := ins.statsStreaming
		if msg.err != nil {
			// io.EOF: the instance stopped; the next instance list shows it, and reopens on a restart.
			ins.closeStatsStream()
			if msg.err != io.EOF && status.Code(msg.err) != codes.Canceled {
				ins.stats, ins.statsFor, ins.statsErr = nil, name, msg.err.Error()
			}
			return m, nil
		}
		ins.stats, ins.statsFor, ins.statsErr = msg.stats, name, ""
		return m, msg.next

	case instanceEventsLoadedMsg:
		if inst := m.instances.selected(); inst == nil || inst.GetName() != msg.name {
			return m, nil // stale
		}
		if msg.err != nil {
			m.setStatus("events: "+msg.err.Error(), true)
			return m, nil
		}
		m.instances.events, m.instances.eventsFor = msg.events, msg.name
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus(msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("instance "+msg.verb, false)
		}
		return m, loadInstances(m.client)

	case shellDoneMsg:
		if msg.err != nil {
			m.setStatus("shell: "+msg.err.Error(), true)
		}
		return m, nil

	case guiLaunchedMsg:
		if msg.err != nil {
			m.setStatus("gui: "+msg.err.Error(), true)
		} else {
			m.setStatus(msg.output, false)
		}
		return m, nil

	case exportStreamMsg:
		return m, follow(&m.instances.export, msg.lineStep, nil)

	case importStreamMsg:
		return m, follow(&m.instances.imp, msg.lineStep, loadInstances(m.client))

	case forkStreamMsg:
		if msg.done && msg.result != nil {
			msg.status = styleGood.Render("forked: " + msg.result.GetName())
		}
		return m, follow(&m.instances.fork, msg.instanceStep, loadInstances(m.client))

	case waitStreamMsg:
		if msg.status != "" {
			m.instances.waitSince = stepStart(m.instances.waitLines, msg.status, m.instances.waitSince)
			m.instances.waitLines = appendProgressLine(m.instances.waitLines, msg.status)
		}
		if msg.done {
			m.instances.waiting = false
			if m.instances.waitCancel != nil {
				m.instances.waitCancel()
				m.instances.waitCancel = nil
			}
			switch {
			case msg.err != nil && status.Code(msg.err) == codes.Canceled:
				m.instances.waitLines = append(m.instances.waitLines, styleSubtitle.Render("stopped waiting (the VM keeps running)"))
			case msg.err != nil:
				m.instances.waitLines = append(m.instances.waitLines, errLine(msg.err))
			case msg.result != nil:
				line := "ready: " + msg.result.GetName()
				if ips := msg.result.GetGuest().GetIpAddresses(); len(ips) > 0 {
					line += " (" + strings.Join(ips, ", ") + ")"
				}
				m.instances.waitLines = append(m.instances.waitLines, styleGood.Render(line))
			}
			return m, loadInstances(m.client)
		}
		return m, msg.next

	case tea.KeyMsg:
		return m.updateInstancesKey(msg)
	}
	return m, nil
}

func (m model) updateInstancesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.instances.waiting {
		if msg.String() == "esc" && m.instances.waitCancel != nil {
			m.instances.waitCancel()
		}
		return m, nil
	}
	if len(m.instances.waitLines) > 0 {
		switch msg.String() {
		case "esc", "enter", "q":
			m.instances.waitLines = nil
		}
		return m, nil
	}
	if m.instances.export.running || m.instances.imp.running || m.instances.fork.running {
		return m, nil // block input until the stream finishes
	}
	// A finished export/import/fork's transcript stays on screen until dismissed here.
	for _, t := range []*transcript{&m.instances.export, &m.instances.imp, &m.instances.fork} {
		if t.dismiss(msg.String()) {
			return m, nil
		}
	}

	// The delete confirmation overlay eats every key: y=delete, p=delete with base image, else cancel.
	if len(m.instances.confirmDelete) > 0 {
		names := instanceNames(m.instances.confirmDelete)
		switch msg.String() {
		case "y", "enter", "p":
			m.instances.confirmDelete = nil
			m.instances.clearMarks()
			return m, deleteInstances(m.client, names, msg.String() == "p")
		default:
			m.instances.confirmDelete = nil
			return m, nil
		}
	}

	if m.instances.prompt != instancesPromptNone {
		return m.updateInstancesPrompt(msg)
	}

	switch msg.String() {
	case "esc", "q":
		if len(m.instances.marked) > 0 {
			m.instances.clearMarks()
			return m, nil
		}
		m.sidebarFocused = true
		return m, nil
	case " ":
		m.instances.toggleMark()
		m.instances.list.CursorDown()
		return m, nil
	case "n":
		m.screen = screenLaunch
		m.launch = newLaunchModel()
		m.launch.setSize(m.width, contentHeight(m.height))
		// Fetch fresh autocomplete candidates each time since they're fast local calls, each filling its own field's
		// Suggestions as it lands. tea.ClearScreen avoids stale content since this takeover's line count differs from Instances.
		return m, tea.Batch(
			tea.ClearScreen,
			loadIntents(m.client),
			loadCloudInitList(m.client),
			loadCatalog(m.client),
			loadCachedImages(m.client),
			loadContainerImages(m.client),
			loadHostDevices(m.client),
		)
	case "l":
		if inst := m.selectedInstance(); inst != nil {
			return m.openLogs(inst, 0)
		}
		return m, nil
	case "L":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.startPrompt(instancesPromptLogs, inst, newSimpleForm("Logs of "+inst.GetName(), []formField{
				textField(logsTailField, "only the last N lines, 0 = from the beginning", m.instances.lastLogsTail),
			}))
		}
		return m, nil
	case "r":
		m.instances.loading = true
		return m, loadInstances(m.client)
	case "s":
		targets := m.instances.targets()
		if len(targets) == 0 {
			return m, nil
		}
		m.instances.clearMarks()
		// With any target running, s stops the running ones; otherwise it starts them all.
		if running := runningNames(targets); len(running) > 0 {
			return m, stopInstance(m.client, &anvilv1.StopRequest{Names: running})
		}
		return m, startInstances(m.client, instanceNames(targets))
	case "S":
		if running := runningNames(m.instances.targets()); len(running) > 0 {
			m.instances.stopNames = running
			m.instances.startPrompt(instancesPromptStop, nil, newSimpleForm("Stop "+namesSummary(running), []formField{
				toggleField("Force", "skip the graceful shutdown", false),
				textField("Timeout seconds", "graceful shutdown wait before escalating", "30"),
			}))
		}
		return m, nil
	case "d":
		m.instances.confirmDelete = m.instances.targets()
		return m, nil
	case "f":
		if inst := m.selectedInstance(); inst != nil && inst.GetVm() != nil {
			m.instances.startPrompt(instancesPromptFork, inst, newSimpleForm("Fork "+inst.GetName(), []formField{
				textField("New name", "name for the forked instance", ""),
				toggleField("Start immediately", "start the fork right after creating it", false),
			}))
		}
		return m, nil
	case "x":
		if inst := m.selectedInstance(); inst != nil {
			if inst.GetVm() == nil {
				return m.shellInto(inst, "", "") // containers have no user concept
			}
			m.instances.startPrompt(instancesPromptShellUser, inst,
				newSimpleForm("Shell", []formField{
					textField("As user (optional, blank = default)", "", m.instances.lastShellUser),
					identityField(m.instances.lastIdentity),
				}))
		}
		return m, nil
	case "e":
		if inst := m.selectedInstance(); inst != nil {
			fields := []formField{textField("Command", "e.g. uptime", "")}
			if inst.GetVm() != nil {
				fields = append(fields, textField("As user (optional)", "", m.instances.lastShellUser), identityField(m.instances.lastIdentity))
			}
			m.instances.startPrompt(instancesPromptExec, inst, newSimpleForm("Exec", fields))
		}
		return m, nil
	case "g":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.startPrompt(instancesPromptGUI, inst, newSimpleForm("GUI app", guiFields(inst, m.instances)))
		}
		return m, nil
	case "m":
		if inst := m.selectedInstance(); inst != nil && inst.GetVm() != nil {
			fields := []formField{
				m.hostPathField("Host path", "directory to share", ""),
				textField("Guest path", "absolute path inside the guest", ""),
				toggleField("Read-only", "mount read-only", false),
			}
			// Same as `anvil create-dir`: a local filesystem change, so only for a local daemon.
			if m.remote() == "" {
				fields = append(fields, toggleField(createDirField, "create the host dir and grant the anvil user access", false))
			}
			m.instances.startPrompt(instancesPromptMount, inst, newSimpleForm("Mount", fields))
		}
		return m, nil
	case "M":
		if inst := m.selectedInstance(); inst != nil && inst.GetVm() != nil {
			guestPath := textField("Guest path", "must match what anvil mount used", "")
			for _, mt := range inst.GetVm().GetMounts() {
				guestPath.Suggestions = append(guestPath.Suggestions, mt.GetGuestPath())
			}
			m.instances.startPrompt(instancesPromptUmount, inst, newSimpleForm("Umount", []formField{guestPath}))
		}
		return m, nil
	case "u":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.startPrompt(instancesPromptSettings, inst, settingsForm(inst))
		}
		return m, nil
	case "w":
		if inst := m.selectedInstance(); inst != nil && inst.GetState() == anvilv1.State_STATE_RUNNING {
			ctx, cancel := context.WithCancel(context.Background())
			m.instances.waiting = true
			m.instances.waitLines = nil
			m.instances.waitCancel = cancel
			return m, startWaitStream(ctx, m.client, inst.GetName())
		}
		return m, nil
	case "p":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.startPrompt(instancesPromptAddPort, inst, newSimpleForm("Add port forward", []formField{
				textField("Host port", "e.g. 8080", ""),
				textField("Guest port", "e.g. 80", ""),
				toggleField("UDP", "use udp instead of tcp", false),
			}))
		}
		return m, nil
	case "P":
		if inst := m.selectedInstance(); inst != nil {
			hostPortField := textField("Host port", "e.g. 8080", "")
			hostPortField.Suggestions = hostPortStrings(inst)
			m.instances.startPrompt(instancesPromptRemovePort, inst, newSimpleForm("Remove port forward", []formField{
				hostPortField,
				toggleField("UDP", "use udp instead of tcp", false),
			}))
		}
		return m, nil
	case "E":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.startPrompt(instancesPromptExport, inst, newSimpleForm("Export "+inst.GetName(), []formField{
				m.exportPathField(inst.GetName()),
			}))
		}
		return m, nil
	case "i":
		m.instances.startPrompt(instancesPromptImport, nil, newSimpleForm("Import bundle", []formField{
			m.hostPathField("Bundle path", "e.g. ./bundle.tar.zst", ""),
			textField("Rename (optional)", "renames the imported intent, or the instance", ""),
		}))
		return m, nil
	}

	prevName := ""
	if inst := m.instances.selected(); inst != nil {
		prevName = inst.GetName()
	}
	var cmd tea.Cmd
	m.instances.list, cmd = m.instances.list.Update(msg)
	if inst := m.instances.selected(); inst != nil && inst.GetName() != prevName {
		m.instances.events, m.instances.eventsFor = nil, ""
		// Held arrow keys would otherwise fire an Events call and a stats stream per row.
		name := inst.GetName()
		settle := tea.Tick(selectSettleDelay, func(time.Time) tea.Msg { return selectionSettledMsg{name: name} })
		return m, tea.Batch(cmd, settle)
	}
	return m, cmd
}

func (m *instancesModel) startPrompt(p instancesPrompt, target *anvilv1.Instance, form simpleForm) {
	m.prompt, m.promptTarget, m.promptForm = p, target, form
}

func (m model) updateInstancesPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ins := &m.instances
	var submitted, cancelled bool
	ins.promptForm, submitted, cancelled = ins.promptForm.update(msg)
	if cancelled {
		ins.prompt = instancesPromptNone
		return m, nil
	}
	if !submitted {
		return m, nil
	}

	prompt, target := ins.prompt, ins.promptTarget
	ins.prompt = instancesPromptNone

	switch prompt {
	case instancesPromptSettings:
		req, changed, err := buildUpdateRequest(target, ins.promptForm)
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		if !changed {
			m.setStatus("nothing changed", false)
			return m, nil
		}
		return m, updateInstance(m.client, req)
	case instancesPromptMount:
		hostPath, guestPath := ins.promptForm.Value("Host path"), ins.promptForm.Value("Guest path")
		if hostPath == "" || guestPath == "" {
			return m, nil
		}
		// Local: relative to this process's cwd. Remote: must be absolute on the daemon's host.
		absHostPath, err := m.client.HostPath(hostPath)
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		createDir := m.remote() == "" && ins.promptForm.Bool(createDirField)
		return m, mountInstance(m.client, target.GetName(), absHostPath, guestPath, ins.promptForm.Bool("Read-only"), createDir)

	case instancesPromptUmount:
		guestPath := ins.promptForm.Value("Guest path")
		if guestPath == "" {
			return m, nil
		}
		return m, umountInstance(m.client, target.GetName(), guestPath)

	case instancesPromptExec:
		command := ins.promptForm.Value("Command")
		if command == "" {
			return m, nil
		}
		user, identity := ins.promptForm.Value("As user (optional)"), ins.promptForm.Value(identityLabel)
		if target.GetVm() != nil {
			ins.lastShellUser, ins.lastIdentity = user, identity
		}
		return m.execInto(target, user, identity, command)

	case instancesPromptGUI:
		g := guiRequestFrom(ins.promptForm)
		if g.command == "" {
			return m, nil
		}
		ins.lastGUICommand, ins.lastGUIX11 = g.command, g.x11
		if target.GetVm() != nil {
			ins.lastIdentity = g.identity
		}
		m.setStatus("starting "+g.command+" in "+target.GetName()+"…", false)
		return m, launchGUIApp(m.socketPath, m.client.Remote, target.GetName(), g)

	case instancesPromptShellUser:
		user, identity := ins.promptForm.Value("As user (optional, blank = default)"), ins.promptForm.Value(identityLabel)
		ins.lastShellUser, ins.lastIdentity = user, identity
		return m.shellInto(target, user, identity)

	case instancesPromptStop:
		req, err := buildStopRequest(ins.stopNames, ins.promptForm)
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		ins.clearMarks()
		return m, stopInstance(m.client, req)

	case instancesPromptLogs:
		tail, err := parseInt32(ins.promptForm.Value(logsTailField), logsTailField)
		if err != nil || tail < 0 {
			m.setStatus("tail lines must be a number of 0 or more", true)
			return m, nil
		}
		ins.lastLogsTail = ins.promptForm.Value(logsTailField)
		return m.openLogs(target, tail)

	case instancesPromptExport:
		outputPath := ins.promptForm.Value("Output path")
		if outputPath == "" {
			return m, nil
		}
		ins.export.start()
		return m, startExportStream(m.client, target.GetName(), outputPath, false)

	case instancesPromptImport:
		bundlePath := ins.promptForm.Value("Bundle path")
		if bundlePath == "" {
			return m, nil
		}
		ins.imp.start()
		return m, startImportStream(m.client, bundlePath, ins.promptForm.Value("Rename (optional)"))

	case instancesPromptAddPort:
		hostPort, err := strconv.Atoi(ins.promptForm.Value("Host port"))
		if err != nil {
			m.setStatus("invalid host port: "+err.Error(), true)
			return m, nil
		}
		guestPort, err := strconv.Atoi(ins.promptForm.Value("Guest port"))
		if err != nil {
			m.setStatus("invalid guest port: "+err.Error(), true)
			return m, nil
		}
		protocol := "tcp"
		if ins.promptForm.Bool("UDP") {
			protocol = "udp"
		}
		return m, addPort(m.client, target.GetName(), hostPort, guestPort, protocol)

	case instancesPromptFork:
		newName := ins.promptForm.Value("New name")
		if newName == "" {
			return m, nil
		}
		ins.fork.start()
		return m, startForkStream(m.client, target.GetName(), newName, ins.promptForm.Bool("Start immediately"))

	case instancesPromptRemovePort:
		hostPort, err := strconv.Atoi(ins.promptForm.Value("Host port"))
		if err != nil {
			m.setStatus("invalid host port: "+err.Error(), true)
			return m, nil
		}
		protocol := "tcp"
		if ins.promptForm.Bool("UDP") {
			protocol = "udp"
		}
		return m, removePort(m.client, target.GetName(), hostPort, protocol)
	}
	return m, nil
}

func (m model) selectedInstance() *anvilv1.Instance {
	return m.instances.selected()
}

func (m instancesModel) View() string {
	if m.export.active() {
		return m.export.view("Exporting…")
	}
	if m.imp.active() {
		return m.imp.view("Importing…")
	}
	if m.waiting || len(m.waitLines) > 0 {
		s := styleTitle.Render(" Waiting for first-boot setup… ") + "\n\n" + progressView(m.waitLines, m.waiting, m.waitSince)
		if m.waiting {
			s += "\n" + helpBar("esc", "stop waiting")
		} else {
			s += "\n" + helpBar("esc", "dismiss")
		}
		return s
	}
	if m.fork.active() {
		return m.fork.view("Forking…")
	}
	if len(m.confirmDelete) > 0 {
		return styleBox.BorderForeground(colorWarn).Render(
			styleWarn.Bold(true).Render("⚠  Delete "+namesSummary(instanceNames(m.confirmDelete))+"?")+"\n\n"+
				styleSubtitle.Render("The disk is removed either way. p also removes the base image if nothing else uses it.")) + "\n\n" +
			helpBar("y", "delete", "p", "delete with base image", "any other key", "cancel")
	}
	if m.prompt != instancesPromptNone {
		return m.promptForm.View()
	}

	listBody := m.list.View()
	if m.loading {
		listBody = styleSubtitle.Render("loading instances…")
	}
	left := styleBoxFocused.Render(lipgloss.NewStyle().Height(m.panelHeight).Width(m.list.Width()).Render(listBody))
	right := styleBox.Render(lipgloss.NewStyle().Height(m.panelHeight).Width(m.detailWidth).Render(m.detailView()))

	help := helpBarWrap(m.contentWidth, instancesHelp...)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}

// runningNames returns the names of the running instances in insts.
func runningNames(insts []*anvilv1.Instance) []string {
	var names []string
	for _, inst := range insts {
		if inst.GetState() == anvilv1.State_STATE_RUNNING {
			names = append(names, inst.GetName())
		}
	}
	return names
}

// namesSummary renders one name quoted, or several as a count plus the names.
func namesSummary(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("%q", names[0])
	}
	return fmt.Sprintf("%d instances (%s)", len(names), strings.Join(names, ", "))
}

// instancesHelp is the Instances legend, most used actions first.
var instancesHelp = []string{
	"n", "launch", "space", "mark", "s", "start/stop", "S", "stop options", "x", "shell", "e", "exec", "g", "gui app", "l/L", "logs/tail", "d", "delete",
	"f", "fork", "u", "settings", "w", "wait cloud-init", "m/M", "mount/umount", "p/P", "add/rm port",
	"E/i", "export/import", "r", "refresh", "esc", "menu",
}

// detailView renders the right-hand panel: detailInfo, then as many of the selected
// instance's recent events as fit below it, newest first.
func (m instancesModel) detailView() string {
	info := m.detailInfo()
	inst := m.selected()
	if inst == nil || m.eventsFor != inst.GetName() || len(m.events) == 0 {
		return info
	}
	room := m.panelHeight - lipgloss.Height(lipgloss.NewStyle().Width(m.detailWidth).Render(info)) - 2
	if room <= 0 {
		return info
	}
	rows := []string{info, "", styleSubtitle.Render("Recent events")}
	row := lipgloss.NewStyle().MaxWidth(m.detailWidth)
	for i := len(m.events) - 1; i >= 0 && room > 0; i, room = i-1, room-1 {
		rows = append(rows, row.Render(eventRow(m.events[i])))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// eventRow renders one event on a single line, with a failure in the error color.
func eventRow(ev *anvilv1.InstanceEvent) string {
	at := time.Unix(ev.GetTimeUnix(), 0).Format("01-02 15:04")
	line := styleFieldLabel.Render(at) + " " + ev.GetAction() + styleSubtitle.Render(" by "+ev.GetActor())
	if msg := ev.GetMessage(); msg != "" {
		line += " " + msg
	}
	if e := ev.GetError(); e != "" {
		line += " " + styleError.Render("failed: "+e)
	}
	return line
}

// detailInfo is the selected instance's static info plus, while it's running,
// live CPU/memory/disk/network gauges.
func (m instancesModel) detailInfo() string {
	inst := m.selected()
	if inst == nil {
		if m.loading {
			return ""
		}
		return styleSubtitle.Render("No instances yet.") + "\n\n" + helpBar("n", "launch one", "i", "import a bundle")
	}

	kind, image := "VM", inst.GetVm().GetImageRef()
	if inst.GetVm().GetHypervisor() == anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER {
		kind = "VM (firecracker)"
	}
	if inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		kind, image = "Container", inst.GetContainer().GetImageRef()
	}

	var b []string
	b = append(b, styleTitle.Render(" "+inst.GetName()+" "))
	b = append(b, "")
	b = append(b, detailRow("Kind", kind))
	b = append(b, detailRow("State", stateDot(inst.GetState())+" "+format.State(inst.GetState())))
	b = append(b, detailRow("Image", image))
	if c := inst.GetCreatedAtUnix(); c > 0 {
		t := time.Unix(c, 0)
		b = append(b, detailRow("Created", t.Format("2006-01-02 15:04")+styleSubtitle.Render(
			"  ("+format.Uptime(int64(time.Since(t).Seconds()), " ")+" ago)")))
	}
	if vm := inst.GetVm(); vm != nil {
		b = append(b, detailRow("Size", resourcesText(vm)))
		if devs := vm.GetDevices(); len(devs) > 0 {
			b = append(b, detailRow("Devices", strings.Join(devs, ", ")))
		}
		if port := vm.GetSshPort(); port != 0 {
			user := vm.GetDefaultUser()
			if user == "" {
				user = "root"
			}
			b = append(b, detailRow("SSH", fmt.Sprintf("%s@localhost, port %d", user, port)))
		}
	}
	b = append(b, containerRows(inst.GetContainer())...)
	policy := "restart " + format.RestartPolicy(inst.GetRestartPolicy())
	if inst.GetAutostart() {
		policy = "autostart  •  " + policy
	}
	b = append(b, detailRow("Policy", policy))
	gui := styleSubtitle.Render("not available (created before GUI support)")
	if inst.GetGuiEnabled() {
		gui = "g to run an app"
	}
	b = append(b, detailRow("GUI", gui))
	if sched := format.Schedule(inst.GetVm().GetSnapshotSchedule(), "01-02 15:04"); sched != "" {
		b = append(b, detailRow("Snaps", sched))
	}
	if intentName := inst.GetLabels()["intent"]; intentName != "" {
		b = append(b, detailRow("Intent", intentName+" ("+inst.GetLabels()["role"]+")"))
	}
	if ports := portsOf(inst); len(ports) > 0 {
		b = append(b, detailRow("Ports", formatPorts(ports)))
	}
	for _, mt := range inst.GetVm().GetMounts() {
		line := mt.GetGuestPath() + " ← " + mt.GetHostPath()
		if mt.GetReadOnly() {
			line += " (ro)"
		}
		b = append(b, detailRow("Mount", line))
	}
	b = append(b, extraHostRows(inst)...)

	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		b = append(b, "", styleSubtitle.Render("not running: no live stats"))
		return lipgloss.JoinVertical(lipgloss.Left, b...)
	}

	stats := m.stats
	if m.statsFor != inst.GetName() {
		stats = nil // last fetch belongs to a different instance, don't show it
	}
	if m.statsErr != "" {
		b = append(b, "", styleError.Render("stats: "+m.statsErr))
		return lipgloss.JoinVertical(lipgloss.Left, b...)
	}
	if stats == nil {
		b = append(b, "", styleSubtitle.Render("fetching live stats…"))
		return lipgloss.JoinVertical(lipgloss.Left, b...)
	}

	address := stats.GetAddress()
	if address == "" {
		address = "-"
	}
	b = append(b, detailRow("Address", address))
	b = append(b, detailRow("Uptime", format.Uptime(stats.GetUptimeSeconds(), " ")))
	b = append(b, guestRows(inst)...)
	b = append(b, "")

	barWidth := m.detailWidth - boxOverhead - 14
	if barWidth < 6 {
		barWidth = 6
	}

	b = append(b, gauge("CPU", stats.GetCpuPercent(), barWidth))

	memPercent := 0.0
	if stats.GetMemLimitBytes() > 0 {
		memPercent = float64(stats.GetMemUsedBytes()) / float64(stats.GetMemLimitBytes()) * 100
	}
	b = append(b, gauge("Mem", memPercent, barWidth)+styleSubtitle.Render(
		fmt.Sprintf("  %s / %s", progress.Bytes(stats.GetMemUsedBytes()), progress.Bytes(stats.GetMemLimitBytes()))))

	if stats.GetDiskTotalBytes() > 0 {
		diskPercent := float64(stats.GetDiskUsedBytes()) / float64(stats.GetDiskTotalBytes()) * 100
		b = append(b, gauge("Disk", diskPercent, barWidth)+styleSubtitle.Render(
			fmt.Sprintf("  %s / %s", progress.Bytes(stats.GetDiskUsedBytes()), progress.Bytes(stats.GetDiskTotalBytes()))))
	} else {
		b = append(b, styleFieldLabel.Render("Disk ")+fmt.Sprintf(
			"read %s  write %s", humanRate(stats.GetDiskReadBytesPerSec()), humanRate(stats.GetDiskWriteBytesPerSec())))
	}

	if stats.GetNetAvailable() {
		b = append(b, styleFieldLabel.Render("Net  ")+fmt.Sprintf(
			"↓ %s  ↑ %s", humanRate(stats.GetNetRxBytesPerSec()), humanRate(stats.GetNetTxBytesPerSec())))
	} else {
		b = append(b, styleFieldLabel.Render("Net  ")+styleSubtitle.Render("not available"))
	}

	return lipgloss.JoinVertical(lipgloss.Left, b...)
}

// containerRows renders a container's engine, ID, network alias and volumes; nothing for a VM.
func containerRows(c *anvilv1.ContainerSpec) []string {
	if c == nil {
		return nil
	}
	rows := []string{detailRow("Engine", format.Engine(c.GetEngine()))}
	if id := c.GetContainerId(); id != "" {
		rows = append(rows, detailRow("ID", format.ShortImageID(id)))
	}
	if alias := c.GetNetworkAlias(); alias != "" {
		rows = append(rows, detailRow("Alias", alias))
	}
	for _, v := range c.GetVolumes() {
		line := v.GetContainerPath() + " ← " + v.GetHostPath()
		if v.GetReadOnly() {
			line += " (ro)"
		}
		rows = append(rows, detailRow("Volume", line))
	}
	return rows
}

// extraHostRows renders the other intent members' names this instance resolves, sorted by name.
func extraHostRows(inst *anvilv1.Instance) []string {
	hosts := inst.GetVm().GetExtraHosts()
	if inst.GetContainer() != nil {
		hosts = inst.GetContainer().GetExtraHosts()
	}
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]string, len(names))
	for i, name := range names {
		rows[i] = detailRow("Host", name+" → "+hosts[name])
	}
	return rows
}

// guestRows renders what a running VM's guest agent reports; nothing for a container.
func guestRows(inst *anvilv1.Instance) []string {
	if inst.GetKind() != anvilv1.Kind_KIND_VM {
		return nil
	}
	g := inst.GetGuest()
	agent := styleSubtitle.Render("not connected")
	if g.GetAgentConnected() {
		agent = "connected"
	} else if inst.GetVm().GetNoGuestAgent() {
		agent = styleSubtitle.Render("disabled")
	}
	rows := []string{detailRow("Agent", agent), detailRow("Init", cloudInitText(g.GetCloudInit()))}
	for _, ip := range g.GetIpAddresses() {
		rows = append(rows, detailRow("Guest IP", ip))
	}
	return rows
}

func cloudInitText(s anvilv1.CloudInitStatus) string {
	switch l := format.CloudInit(s); l {
	case "error":
		return styleError.Render(l + " (see logs)")
	case "unknown":
		return styleSubtitle.Render(l)
	default:
		return l
	}
}

func detailRow(label, value string) string {
	return styleFieldLabel.Render(fmt.Sprintf("%-8s", label)) + value
}

// portsOf returns inst's host-to-guest port forwards, VM or container alike.
func portsOf(inst *anvilv1.Instance) []*anvilv1.PortMapping {
	if inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		return inst.GetContainer().GetPorts()
	}
	return inst.GetVm().GetPorts()
}

// hostPortStrings returns inst's currently exposed host ports, for the "Remove port forward" prompt's
// Suggestions, so removing one doesn't require already knowing it by heart.
func hostPortStrings(inst *anvilv1.Instance) []string {
	ports := portsOf(inst)
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = strconv.Itoa(int(p.GetHostPort()))
	}
	return out
}

// formatPorts renders ports as "8080:80/tcp, 2222:22/tcp", the same shape
// `anvil port add` takes.
func formatPorts(ports []*anvilv1.PortMapping) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		proto := p.GetProtocol()
		if proto == "" {
			proto = "tcp"
		}
		parts[i] = fmt.Sprintf("%d:%d/%s", p.GetHostPort(), p.GetGuestPort(), proto)
	}
	return strings.Join(parts, ", ")
}

// stateDot is a small colored marker giving an at-a-glance state indicator
// in the list and detail panel, on top of the plain text label.
func stateDot(s anvilv1.State) string {
	color := colorMuted
	switch s {
	case anvilv1.State_STATE_RUNNING:
		color = colorGood
	case anvilv1.State_STATE_STARTING, anvilv1.State_STATE_STOPPING:
		color = colorWarn
	case anvilv1.State_STATE_ERROR:
		color = colorBad
	}
	return lipgloss.NewStyle().Foreground(color).Render("●")
}
