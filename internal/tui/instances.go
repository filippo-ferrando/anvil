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

// instancesPrompt identifies which small form (if any) is up over the
// instances list — mount/umount/exec/shell-user-override each reuse the
// same simpleForm, just with different fields and a different action on
// submit, same pattern as cloudInitView's new/import/rename prompts.
type instancesPrompt int

const (
	instancesPromptNone instancesPrompt = iota
	instancesPromptMount
	instancesPromptUmount
	instancesPromptExec
	instancesPromptShellUser
)

type instancesModel struct {
	list          list.Model
	confirmDelete *anvilv1.Instance // non-nil while the delete confirmation overlay is up
	loading       bool

	prompt        instancesPrompt
	promptTarget  *anvilv1.Instance
	promptForm    simpleForm
	lastShellUser string // remembered across shell/exec calls, pre-filled into the user field
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
	// The delete confirmation overlay eats every key until answered —
	// two distinct outcomes, not just yes/no: `anvil delete` (recoverable,
	// state DELETED until a later purge) vs `anvil delete --purge`
	// (removed outright). Without the second one there was no way to
	// actually clean an instance up from the TUI at all.
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
		m.launch.form.SetHeight(contentHeight(m.height) - 2)
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
				return m.shellInto(inst, "") // containers: no user concept, shell straight in
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

	var cmd tea.Cmd
	m.instances.list, cmd = m.instances.list.Update(msg)
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
		return m, mountInstance(m.client, target.GetName(), hostPath, guestPath, ins.promptForm.Bool("Read-only"))

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
	item, ok := m.instances.list.SelectedItem().(instanceItem)
	if !ok {
		return nil
	}
	return item.inst
}

func (m instancesModel) View() string {
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Delete %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "delete (recoverable)", "p", "delete permanently", "any other key", "cancel")
	}
	if m.prompt != instancesPromptNone {
		return m.promptForm.View()
	}
	body := m.list.View()
	if m.loading {
		body = styleSubtitle.Render("loading…")
	}
	return body + "\n" + helpBar(
		"n", "launch", "s", "start/stop", "d", "delete", "x", "shell",
		"e", "exec", "m", "mount", "M", "umount", "r", "refresh", "esc", "back",
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
