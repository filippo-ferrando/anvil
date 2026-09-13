package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

type instanceItem struct{ inst *anvilv1.Instance }

func (i instanceItem) FilterValue() string { return i.inst.GetName() }
func (i instanceItem) Title() string       { return i.inst.GetName() }
func (i instanceItem) Description() string {
	kind, image := "vm", i.inst.GetVm().GetImageRef()
	if i.inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
		kind, image = "container", i.inst.GetContainer().GetImageRef()
	}
	return fmt.Sprintf("%s  •  %s  •  %s", kind, stateLabel(i.inst.GetState()), image)
}

type instancesModel struct {
	list          list.Model
	confirmDelete *anvilv1.Instance // non-nil while the delete confirmation overlay is up
	loading       bool
}

func newInstancesModel() instancesModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false) // small lists; also avoids single-letter shortcuts (n/s/d/...) colliding with filter typing
	l.Title = "Instances"
	l.SetShowHelp(false) // one consistent helpBar instead of list's own
	return instancesModel{list: l, loading: true}
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
	// The delete confirmation overlay eats every key until answered.
	if m.instances.confirmDelete != nil {
		switch msg.String() {
		case "y", "enter":
			name := m.instances.confirmDelete.GetName()
			m.instances.confirmDelete = nil
			return m, deleteInstance(m.client, name)
		default:
			m.instances.confirmDelete = nil
			return m, nil
		}
	}

	switch msg.String() {
	case "esc", "q":
		m.screen = screenMenu
		return m, nil
	case "n":
		m.screen = screenLaunch
		m.launch = newLaunchModel()
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
			return m.shellInto(inst)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.instances.list, cmd = m.instances.list.Update(msg)
	return m, cmd
}

func (m model) selectedInstance() *anvilv1.Instance {
	item, ok := m.instances.list.SelectedItem().(instanceItem)
	if !ok {
		return nil
	}
	return item.inst
}

func (m instancesModel) View() string {
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Delete %q? (recoverable via `anvil purge` until then)", m.confirmDelete.GetName())) +
			"\n\n" + helpBar("y", "confirm", "any other key", "cancel")
	}
	body := m.list.View()
	if m.loading {
		body = styleSubtitle.Render("loading…")
	}
	return body + "\n" + helpBar(
		"n", "launch", "s", "start/stop", "d", "delete",
		"x", "shell", "r", "refresh", "esc", "back",
	)
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
