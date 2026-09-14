package tui

import (
	"fmt"
	"path/filepath"
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

	detailWidth int // width of the detail panel next to the list
	panelHeight int // shared height for both side-by-side panels

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

	case tea.KeyMsg:
		return m.updateInstancesKey(msg)
	}
	return m, nil
}

func (m model) updateInstancesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		return m, tea.Batch(
			loadIntents(m.client),
			loadCloudInitList(m.client),
			loadCatalog(m.client),
			loadCachedImages(m.client),
			loadContainerImages(m.client),
		)
	case "l":
		if inst := m.selectedInstance(); inst != nil {
			m.screen = screenLogs
			m.logs = newLogsModel(inst, m.width, contentHeight(m.height))
			cmd, cancel := startLogsStream(m.client, inst.GetName())
			m.logs.cancel = cancel
			return m, cmd
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
				textField("Host path", "absolute path on this host", ""),
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
	}
	return m, nil
}

func (m model) selectedInstance() *anvilv1.Instance {
	return m.instances.selected()
}

func (m instancesModel) View() string {
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

	help := helpBar(
		"n", "launch", "s", "start/stop", "d", "delete", "x", "shell",
		"e", "exec", "m", "mount", "M", "umount", "l", "logs", "r", "refresh", "esc", "back",
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
