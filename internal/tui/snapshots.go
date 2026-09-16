package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

type snapshotItem struct{ snap *anvilv1.SnapshotInfo }

func (i snapshotItem) FilterValue() string { return i.snap.GetName() }
func (i snapshotItem) Title() string       { return i.snap.GetName() }
func (i snapshotItem) Description() string {
	state := "disk-only"
	if i.snap.GetHasVmState() {
		state = "disk+VM state"
	}
	return time.Unix(i.snap.GetCreatedAtUnix(), 0).Format("2006-01-02 15:04:05") + "  •  " + state
}

// snapshotsPrompt identifies which overlay form, if any, is showing over the snapshots page.
type snapshotsPrompt int

const (
	snapshotsPromptNone snapshotsPrompt = iota
	snapshotsPromptNew
)

// snapshotsModel is the Snapshots page: a VM instance list on the left,
// that instance's own QCOW2 snapshots on the right. Containers don't
// appear here — they have no equivalent primitive (see instance.Snapshotter).
type snapshotsModel struct {
	instances list.Model
	snaps     list.Model
	loading   bool // fetching the currently-selected instance's snapshots

	focusSnaps bool // false: instances list has focus; true: snaps list does

	prompt     snapshotsPrompt
	promptForm simpleForm

	confirmRestore *anvilv1.SnapshotInfo
	confirmDelete  *anvilv1.SnapshotInfo

	panelHeight  int
	contentWidth int
}

func newSnapshotsModel() snapshotsModel {
	instances := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	instances.SetFilteringEnabled(false)
	instances.Title = "VM instances"
	instances.SetShowHelp(false)

	snaps := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	snaps.SetFilteringEnabled(false)
	snaps.Title = "Snapshots"
	snaps.SetShowHelp(false)

	return snapshotsModel{instances: instances, snaps: snaps}
}

func (m *snapshotsModel) setSize(width, height int) {
	m.contentWidth = width
	const gutter = 2
	inner := width - 2*boxOverhead - gutter
	if inner < 20 {
		inner = 20
	}
	instancesWidth := inner * 2 / 5
	if instancesWidth < 22 {
		instancesWidth = 22
	}
	snapsWidth := inner - instancesWidth
	if snapsWidth < 24 {
		snapsWidth = 24
	}
	m.panelHeight = height - boxHeightOverhead
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}
	m.instances.SetSize(instancesWidth, m.panelHeight)
	m.snaps.SetSize(snapsWidth, m.panelHeight)
}

func (m *snapshotsModel) selectedInstance() *anvilv1.Instance {
	item, ok := m.instances.SelectedItem().(instanceItem)
	if !ok {
		return nil
	}
	return item.inst
}

func (m *snapshotsModel) selectedSnapshot() *anvilv1.SnapshotInfo {
	item, ok := m.snaps.SelectedItem().(snapshotItem)
	if !ok {
		return nil
	}
	return item.snap
}

func (m model) updateSnapshots(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case instancesLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing instances: "+msg.err.Error(), true)
			return m, nil
		}
		var items []list.Item
		for _, inst := range msg.instances {
			if inst.GetKind() == anvilv1.Kind_KIND_VM {
				items = append(items, instanceItem{inst: inst})
			}
		}
		m.snapshots.instances.SetItems(items)
		if inst := m.snapshots.selectedInstance(); inst != nil {
			m.snapshots.loading = true
			return m, loadSnapshots(m.client, inst.GetName())
		}
		m.snapshots.snaps.SetItems(nil)
		return m, nil

	case snapshotsLoadedMsg:
		m.snapshots.loading = false
		if inst := m.snapshots.selectedInstance(); inst == nil || inst.GetName() != msg.instanceName {
			return m, nil // stale: selection has since moved on
		}
		if msg.err != nil {
			m.setStatus("listing "+msg.instanceName+"'s snapshots: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.snapshots))
		for i, s := range msg.snapshots {
			items[i] = snapshotItem{snap: s}
		}
		m.snapshots.snaps.SetItems(items)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus(msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus(msg.verb, false)
		}
		if inst := m.snapshots.selectedInstance(); inst != nil {
			return m, loadSnapshots(m.client, inst.GetName())
		}
		return m, nil

	case tea.KeyMsg:
		return m.updateSnapshotsKey(msg)
	}
	return m, nil
}

func (m model) updateSnapshotsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sn := &m.snapshots

	if sn.confirmRestore != nil {
		switch msg.String() {
		case "y", "enter":
			inst, snap := sn.selectedInstance(), sn.confirmRestore
			sn.confirmRestore = nil
			return m, restoreSnapshot(m.client, inst.GetName(), snap.GetName())
		default:
			sn.confirmRestore = nil
		}
		return m, nil
	}
	if sn.confirmDelete != nil {
		switch msg.String() {
		case "y", "enter":
			inst, snap := sn.selectedInstance(), sn.confirmDelete
			sn.confirmDelete = nil
			return m, deleteSnapshot(m.client, inst.GetName(), snap.GetName())
		default:
			sn.confirmDelete = nil
		}
		return m, nil
	}
	if sn.prompt != snapshotsPromptNone {
		var submitted, cancelled bool
		sn.promptForm, submitted, cancelled = sn.promptForm.update(msg)
		if cancelled {
			sn.prompt = snapshotsPromptNone
			return m, nil
		}
		if !submitted {
			return m, nil
		}
		name := sn.promptForm.Value("Snapshot name")
		sn.prompt = snapshotsPromptNone
		if name == "" {
			return m, nil
		}
		inst := sn.selectedInstance()
		if inst == nil {
			return m, nil
		}
		return m, createSnapshot(m.client, inst.GetName(), name)
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "tab":
		sn.focusSnaps = !sn.focusSnaps
		return m, nil
	case "r":
		if inst := sn.selectedInstance(); inst != nil {
			sn.loading = true
			return m, loadSnapshots(m.client, inst.GetName())
		}
		return m, nil
	case "n":
		if sn.selectedInstance() == nil {
			return m, nil
		}
		sn.prompt = snapshotsPromptNew
		sn.promptForm = newSimpleForm("New snapshot", []formField{
			textField("Snapshot name", "letters, digits, _, -, . only", ""),
		})
		return m, nil
	}

	if sn.focusSnaps {
		switch msg.String() {
		case "a", "enter":
			if snap := sn.selectedSnapshot(); snap != nil {
				sn.confirmRestore = snap
			}
			return m, nil
		case "d":
			if snap := sn.selectedSnapshot(); snap != nil {
				sn.confirmDelete = snap
			}
			return m, nil
		}
		var cmd tea.Cmd
		sn.snaps, cmd = sn.snaps.Update(msg)
		return m, cmd
	}

	prevName := ""
	if inst := sn.selectedInstance(); inst != nil {
		prevName = inst.GetName()
	}
	var cmd tea.Cmd
	sn.instances, cmd = sn.instances.Update(msg)
	if inst := sn.selectedInstance(); inst != nil && inst.GetName() != prevName {
		sn.snaps.SetItems(nil)
		sn.loading = true
		return m, tea.Batch(cmd, loadSnapshots(m.client, inst.GetName()))
	}
	return m, cmd
}

func (m snapshotsModel) View() string {
	if m.confirmRestore != nil {
		inst := m.selectedInstance()
		return styleWarn.Render(fmt.Sprintf("Restore %s to snapshot %q?", inst.GetName(), m.confirmRestore.GetName())) +
			"\n" + styleSubtitle.Render("This discards any disk changes since that snapshot was taken.") + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Delete snapshot %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.prompt != snapshotsPromptNone {
		return m.promptForm.View()
	}

	instancesBox, snapsBox := styleBox, styleBox
	if m.focusSnaps {
		snapsBox = styleBoxFocused
	} else {
		instancesBox = styleBoxFocused
	}

	snapsBody := m.snaps.View()
	if m.loading {
		snapsBody = styleSubtitle.Render("loading…")
	} else if m.selectedInstance() != nil && len(m.snaps.Items()) == 0 {
		snapsBody = styleSubtitle.Render("no snapshots yet — press n to create one")
	} else if m.selectedInstance() == nil {
		snapsBody = styleSubtitle.Render("no VM instances yet")
	}

	left := instancesBox.Render(lipgloss.NewStyle().Height(m.panelHeight).Render(m.instances.View()))
	right := snapsBox.Render(lipgloss.NewStyle().Height(m.panelHeight).Render(snapsBody))

	help := helpBarWrap(m.contentWidth,
		"tab", "switch focus", "n", "new", "a", "restore", "d", "delete", "r", "refresh", "esc", "back",
	)
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}
