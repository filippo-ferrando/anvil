package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

type instanceItem struct{ inst *anvilv1.Instance }

// stateGlyph is a plain (uncolored) marker: list items are re-styled
// wholesale by bubbles' own list.DefaultDelegate when selected, and an
// embedded ANSI reset from a colored inner Render() would cut that
// styling off partway through the line — see stateDot, used only in the
// detail panel, which this package fully renders itself instead.
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
	return stateGlyph(i.inst.GetState()) + " " + i.inst.GetName()
}
func (i instanceItem) Description() string {
	kind, image := "vm", i.inst.GetVm().GetImageRef()
	if i.inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		kind, image = "container", i.inst.GetContainer().GetImageRef()
	}
	return fmt.Sprintf("%s  •  %s  •  %s", kind, stateLabel(i.inst.GetState()), image)
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
)

// statsRefreshInterval is how often the selected running instance's live
// stats are re-polled — see maybeRefreshStats.
const statsRefreshInterval = 2 * time.Second

type instancesModel struct {
	list          list.Model
	confirmDelete *anvilv1.Instance // non-nil while the delete confirmation overlay is up
	loading       bool

	prompt        instancesPrompt
	promptTarget  *anvilv1.Instance
	promptForm    simpleForm
	lastShellUser string // remembered across shell/exec calls, pre-filled into the user field

	exporting   bool     // an export stream is in flight, blocking other keys
	exportLines []string // the finished (or in-flight) export's progress transcript

	importing   bool     // an import stream is in flight, blocking other keys
	importLines []string // the finished (or in-flight) import's progress transcript

	detailWidth  int // width of the detail panel next to the list
	panelHeight  int // shared height for both side-by-side panels
	contentWidth int // full width given to this screen, for wrapping the help bar

	// Live stats for whichever instance is currently selected, see
	// maybeRefreshStats/statsLoadedMsg. statsFor names which instance
	// stats/statsErr belong to, so a reply that arrives after the
	// selection has since moved on is never shown against the wrong row.
	stats          *anvilv1.InstanceStats
	statsFor       string
	statsErr       string
	statsFetchedAt time.Time
}

func newInstancesModel() instancesModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
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
	m.panelHeight = height - boxHeightOverhead
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}
	m.list.SetSize(listWidth, m.panelHeight)
}

// counts summarizes how many known instances are currently running, for the header.
func (ins *instancesModel) counts() (running, total int) {
	for _, it := range ins.list.Items() {
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
func (ins *instancesModel) selected() *anvilv1.Instance {
	item, ok := ins.list.SelectedItem().(instanceItem)
	if !ok {
		return nil
	}
	return item.inst
}

// maybeRefreshStats returns a Cmd to (re)fetch the selected running
// instance's live stats, if one is due — nil otherwise (nothing selected,
// selection isn't running, or the last fetch is still fresh).
func (ins *instancesModel) maybeRefreshStats(c *client.Client) tea.Cmd {
	inst := ins.selected()
	if inst == nil || inst.GetState() != anvilv1.State_STATE_RUNNING {
		return nil
	}
	if inst.GetName() == ins.statsFor && time.Since(ins.statsFetchedAt) < statsRefreshInterval {
		return nil
	}
	ins.statsFetchedAt = time.Now()
	return loadStats(c, inst.GetName())
}

func (m model) updateInstances(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case instancesLoadedMsg:
		m.instances.loading = false
		if msg.err != nil {
			m.setStatus("listing instances: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.instances))
		for i, inst := range msg.instances {
			items[i] = instanceItem{inst: inst}
		}
		m.instances.list.SetItems(items)
		return m, nil

	case statsLoadedMsg:
		if inst := m.instances.selected(); inst == nil || inst.GetName() != msg.name {
			return m, nil // stale: selection has since moved on
		}
		m.instances.statsFor = msg.name
		if msg.err != nil {
			m.instances.stats = nil
			m.instances.statsErr = msg.err.Error()
		} else {
			m.instances.stats = msg.stats
			m.instances.statsErr = ""
		}
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

	case exportStreamMsg:
		if msg.line != "" {
			m.instances.exportLines = appendProgressLine(m.instances.exportLines, msg.line)
		}
		if msg.err != nil {
			m.instances.exporting = false
			m.instances.exportLines = append(m.instances.exportLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			m.instances.exporting = false
			return m, nil
		}
		return m, receiveExportEvent(msg.stream)

	case importStreamMsg:
		if msg.line != "" {
			m.instances.importLines = appendProgressLine(m.instances.importLines, msg.line)
		}
		if msg.err != nil {
			m.instances.importing = false
			m.instances.importLines = append(m.instances.importLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			m.instances.importing = false
			return m, loadInstances(m.client)
		}
		return m, receiveImportEvent(msg.stream)

	case tea.KeyMsg:
		return m.updateInstancesKey(msg)
	}
	return m, nil
}

func (m model) updateInstancesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.instances.exporting || m.instances.importing {
		return m, nil // block input until the stream finishes
	}
	// A finished export/import's transcript stays on screen until dismissed here.
	if len(m.instances.exportLines) > 0 {
		switch msg.String() {
		case "esc", "enter", "q":
			m.instances.exportLines = nil
		}
		return m, nil
	}
	if len(m.instances.importLines) > 0 {
		switch msg.String() {
		case "esc", "enter", "q":
			m.instances.importLines = nil
		}
		return m, nil
	}

	// The delete confirmation overlay eats every key: y=delete, p=purge, else cancel.
	if m.instances.confirmDelete != nil {
		inst := m.instances.confirmDelete
		switch msg.String() {
		case "y", "enter":
			m.instances.confirmDelete = nil
			return m, deleteInstance(m.client, inst.GetName(), false)
		case "p":
			m.instances.confirmDelete = nil
			return m, deleteInstance(m.client, inst.GetName(), true)
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
		m.sidebarFocused = true
		return m, nil
	case "n":
		m.screen = screenLaunch
		m.launch = newLaunchModel()
		m.launch.setSize(m.width, contentHeight(m.height))
		// Fetch autocomplete candidates fresh every time — fast local
		// daemon calls, and each fills in one field's Suggestions
		// independently of the others as it lands (see updateLaunch).
		// tea.ClearScreen forces a full repaint: this full-screen takeover
		// rarely renders the exact same total line count as Instances, and
		// relying on Bubble Tea's diff-based erase-below to always catch
		// that gap left stale content on screen.
		return m, tea.Batch(
			tea.ClearScreen,
			loadIntents(m.client),
			loadCloudInitList(m.client),
			loadCatalog(m.client),
			loadCachedImages(m.client),
			loadContainerImages(m.client),
		)
	case "l":
		if inst := m.selectedInstance(); inst != nil {
			m.screen = screenLogs
			m.logs = newLogsModel(inst, m.width, logsViewportHeight(m.height))
			cmd, cancel := startLogsStream(m.client, inst.GetName())
			m.logs.cancel = cancel
			return m, tea.Batch(tea.ClearScreen, cmd)
		}
		return m, nil
	case "r":
		m.instances.loading = true
		return m, loadInstances(m.client)
	case "s":
		if inst := m.selectedInstance(); inst != nil {
			if inst.GetState() == anvilv1.State_STATE_RUNNING {
				return m, stopInstance(m.client, inst.GetName())
			}
			return m, startInstance(m.client, inst.GetName())
		}
		return m, nil
	case "d":
		if inst := m.selectedInstance(); inst != nil {
			m.instances.confirmDelete = inst
		}
		return m, nil
	case "x":
		if inst := m.selectedInstance(); inst != nil {
			if inst.GetVm() == nil {
				return m.shellInto(inst, "") // containers have no user concept
			}
			m.instances.startPrompt(instancesPromptShellUser, inst,
				newSimpleForm("Shell", []formField{
					textField("As user (optional, blank = default)", "", m.instances.lastShellUser),
				}))
		}
		return m, nil
	case "e":
		if inst := m.selectedInstance(); inst != nil {
			fields := []formField{textField("Command", "e.g. uptime", "")}
			if inst.GetVm() != nil {
				fields = append(fields, textField("As user (optional)", "", m.instances.lastShellUser))
			}
			m.instances.startPrompt(instancesPromptExec, inst, newSimpleForm("Exec", fields))
		}
		return m, nil
	case "m":
		if inst := m.selectedInstance(); inst != nil && inst.GetVm() != nil {
			m.instances.startPrompt(instancesPromptMount, inst, newSimpleForm("Mount", []formField{
				pathField("Host path", "absolute path on this host", ""),
				textField("Guest path", "absolute path inside the guest", ""),
				toggleField("Read-only", "mount read-only", false),
			}))
		}
		return m, nil
	case "M":
		if inst := m.selectedInstance(); inst != nil && inst.GetVm() != nil {
			m.instances.startPrompt(instancesPromptUmount, inst, newSimpleForm("Umount", []formField{
				textField("Guest path", "must match what anvil mount used", ""),
			}))
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
				pathField("Output path", "e.g. ./"+inst.GetName()+".tar.zst", inst.GetName()+".tar.zst"),
			}))
		}
		return m, nil
	case "i":
		m.instances.startPrompt(instancesPromptImport, nil, newSimpleForm("Import bundle", []formField{
			pathField("Bundle path", "e.g. ./bundle.tar.zst", ""),
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
		// Selection moved: drop the old instance's stats immediately
		// instead of leaving them on screen against the new selection,
		// and kick off a fresh fetch right away rather than waiting for
		// the next 1s tick.
		m.instances.stats, m.instances.statsErr, m.instances.statsFor = nil, "", ""
		if inst.GetState() == anvilv1.State_STATE_RUNNING {
			m.instances.statsFetchedAt = time.Now()
			return m, tea.Batch(cmd, loadStats(m.client, inst.GetName()))
		}
	}
	return m, cmd
}

func (ins *instancesModel) startPrompt(p instancesPrompt, target *anvilv1.Instance, form simpleForm) {
	ins.prompt, ins.promptTarget, ins.promptForm = p, target, form
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
	case instancesPromptMount:
		hostPath, guestPath := ins.promptForm.Value("Host path"), ins.promptForm.Value("Guest path")
		if hostPath == "" || guestPath == "" {
			return m, nil
		}
		// Resolve a relative path against this process's cwd, client-side.
		absHostPath, err := filepath.Abs(hostPath)
		if err != nil {
			m.setStatus("resolving "+hostPath+": "+err.Error(), true)
			return m, nil
		}
		return m, mountInstance(m.client, target.GetName(), absHostPath, guestPath, ins.promptForm.Bool("Read-only"))

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
		user := ins.promptForm.Value("As user (optional)")
		ins.lastShellUser = user
		return m.execInto(target, user, command)

	case instancesPromptShellUser:
		user := ins.promptForm.Value("As user (optional, blank = default)")
		ins.lastShellUser = user
		return m.shellInto(target, user)

	case instancesPromptExport:
		outputPath := ins.promptForm.Value("Output path")
		if outputPath == "" {
			return m, nil
		}
		ins.exporting = true
		ins.exportLines = nil
		return m, startExportStream(m.client, target.GetName(), outputPath, false)

	case instancesPromptImport:
		bundlePath := ins.promptForm.Value("Bundle path")
		if bundlePath == "" {
			return m, nil
		}
		ins.importing = true
		ins.importLines = nil
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
	if m.exporting || len(m.exportLines) > 0 {
		s := styleTitle.Render(" Exporting… ") + "\n\n"
		for _, line := range m.exportLines {
			s += line + "\n"
		}
		if !m.exporting {
			s += "\n" + helpBar("esc", "dismiss")
		}
		return s
	}
	if m.importing || len(m.importLines) > 0 {
		s := styleTitle.Render(" Importing… ") + "\n\n"
		for _, line := range m.importLines {
			s += line + "\n"
		}
		if !m.importing {
			s += "\n" + helpBar("esc", "dismiss")
		}
		return s
	}
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Delete %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "delete (recoverable)", "p", "delete permanently", "any other key", "cancel")
	}
	if m.prompt != instancesPromptNone {
		return m.promptForm.View()
	}

	listBody := m.list.View()
	if m.loading {
		listBody = styleSubtitle.Render("loading…")
	}
	left := styleBoxFocused.Render(lipgloss.NewStyle().Height(m.panelHeight).Width(m.list.Width()).Render(listBody))
	right := styleBox.Render(lipgloss.NewStyle().Height(m.panelHeight).Width(m.detailWidth).Render(m.detailView()))

	help := helpBarWrap(m.contentWidth,
		"n", "launch", "s", "start/stop", "d", "delete", "x", "shell",
		"e", "exec", "m", "mount", "M", "umount", "p", "add port", "P", "remove port",
		"E", "export", "i", "import", "l", "logs", "r", "refresh", "esc", "back",
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}

// detailView renders the right-hand panel: the selected instance's static
// info plus, while it's running, live CPU/memory/disk/network gauges.
func (m instancesModel) detailView() string {
	inst := m.selected()
	if inst == nil {
		return styleSubtitle.Render("no instances yet — press n to launch one")
	}

	kind, image := "VM", inst.GetVm().GetImageRef()
	if inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		kind, image = "Container", inst.GetContainer().GetImageRef()
	}

	var b []string
	b = append(b, styleTitle.Render(" "+inst.GetName()+" "))
	b = append(b, "")
	b = append(b, detailRow("Kind", kind))
	b = append(b, detailRow("State", stateDot(inst.GetState())+" "+stateLabel(inst.GetState())))
	b = append(b, detailRow("Image", image))
	if intentName := inst.GetLabels()["intent"]; intentName != "" {
		b = append(b, detailRow("Intent", intentName+" ("+inst.GetLabels()["role"]+")"))
	}
	if ports := portsOf(inst); len(ports) > 0 {
		b = append(b, detailRow("Ports", formatPorts(ports)))
	}

	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		b = append(b, "", styleSubtitle.Render("not running — no live stats"))
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
	b = append(b, detailRow("Uptime", humanDuration(stats.GetUptimeSeconds())))
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
		fmt.Sprintf("  %s / %s", humanBytesTUI(stats.GetMemUsedBytes()), humanBytesTUI(stats.GetMemLimitBytes()))))

	if stats.GetDiskTotalBytes() > 0 {
		diskPercent := float64(stats.GetDiskUsedBytes()) / float64(stats.GetDiskTotalBytes()) * 100
		b = append(b, gauge("Disk", diskPercent, barWidth)+styleSubtitle.Render(
			fmt.Sprintf("  %s / %s", humanBytesTUI(stats.GetDiskUsedBytes()), humanBytesTUI(stats.GetDiskTotalBytes()))))
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

// hostPortStrings returns inst's currently exposed host ports, for the
// "Remove port forward" prompt's Suggestions — so removing one doesn't
// require already knowing it by heart.
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

func stateLabel(s anvilv1.State) string {
	switch s {
	case anvilv1.State_STATE_STOPPED:
		return "stopped"
	case anvilv1.State_STATE_STARTING:
		return "starting"
	case anvilv1.State_STATE_RUNNING:
		return "running"
	case anvilv1.State_STATE_STOPPING:
		return "stopping"
	case anvilv1.State_STATE_DELETING:
		return "deleting"
	case anvilv1.State_STATE_DELETED:
		return "deleted"
	case anvilv1.State_STATE_ERROR:
		return "error"
	default:
		return "unknown"
	}
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
