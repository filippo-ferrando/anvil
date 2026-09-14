package tui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

type mirrorItem struct{ mirror *anvilv1.Mirror }

func (i mirrorItem) FilterValue() string { return i.mirror.GetName() }
func (i mirrorItem) Title() string       { return i.mirror.GetName() }
func (i mirrorItem) Description() string {
	kind, source := "vm", i.mirror.GetManifestUrl()
	if i.mirror.GetKind() == anvilv1.MirrorKind_MIRROR_KIND_CONTAINER {
		kind, source = "container", i.mirror.GetRegistry()
		if i.mirror.GetMirrorOf() != "" {
			source = fmt.Sprintf("%s (mirrors %s)", source, i.mirror.GetMirrorOf())
		}
	}
	enabled := "enabled"
	if !i.mirror.GetEnabled() {
		enabled = "disabled"
	}
	return fmt.Sprintf("%s  •  %s  •  priority %d  •  %s", kind, source, i.mirror.GetPriority(), enabled)
}

type mirrorsModel struct {
	list          list.Model
	adding        bool
	addKind       string // "vm" | "container"
	addForm       simpleForm
	confirmRemove *anvilv1.Mirror
}

func newMirrorsModel() mirrorsModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Mirrors"
	l.SetShowHelp(false)
	return mirrorsModel{list: l}
}

func newMirrorAddForm() simpleForm {
	return newSimpleForm("Add mirror  —  ctrl+k: switch vm/container", []formField{
		textField("Name", "", ""),
		textField("Manifest URL (vm only)", "", ""),
		textField("Registry (container only)", "host[:port]", ""),
		textField("Mirror of (container only, optional)", "upstream registry", ""),
		textField("Priority", "higher wins on a collision", "0"),
	})
}

func (m model) updateMirrors(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case mirrorsLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing mirrors: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.mirrors))
		for i, mm := range msg.mirrors {
			items[i] = mirrorItem{mirror: mm}
		}
		m.mirrors.list.SetItems(items)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("mirror "+msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("mirror "+msg.verb, false)
		}
		return m, loadMirrors(m.client)

	case tea.KeyMsg:
		return m.updateMirrorsKey(msg)
	}
	return m, nil
}

func (m model) updateMirrorsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	mm := &m.mirrors

	if mm.confirmRemove != nil {
		switch msg.String() {
		case "y", "enter":
			name := mm.confirmRemove.GetName()
			mm.confirmRemove = nil
			return m, removeMirror(m.client, name)
		default:
			mm.confirmRemove = nil
			return m, nil
		}
	}

	if mm.adding {
		if msg.String() == "ctrl+k" {
			if mm.addKind == "vm" {
				mm.addKind = "container"
			} else {
				mm.addKind = "vm"
			}
			return m, nil
		}
		var submitted, cancelled bool
		mm.addForm, submitted, cancelled = mm.addForm.update(msg)
		if cancelled {
			mm.adding = false
			return m, nil
		}
		if submitted {
			mirror, err := buildMirror(mm.addKind, mm.addForm)
			if err != nil {
				mm.addForm.errMsg = err.Error()
				return m, nil
			}
			mm.adding = false
			return m, addMirror(m.client, mirror)
		}
		return m, nil
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "a":
		mm.adding = true
		mm.addKind = "vm"
		mm.addForm = newMirrorAddForm()
		mm.addForm.SetHeight(contentHeight(m.height) - 2)
		return m, nil
	case "r":
		return m, loadMirrors(m.client)
	case "e":
		if mi := m.selectedMirror(); mi != nil {
			return m, setMirrorEnabled(m.client, mi.GetName(), !mi.GetEnabled())
		}
		return m, nil
	case "x":
		if mi := m.selectedMirror(); mi != nil {
			mm.confirmRemove = mi
		}
		return m, nil
	}

	var cmd tea.Cmd
	mm.list, cmd = mm.list.Update(msg)
	return m, cmd
}

func (m model) selectedMirror() *anvilv1.Mirror {
	item, ok := m.mirrors.list.SelectedItem().(mirrorItem)
	if !ok {
		return nil
	}
	return item.mirror
}

func buildMirror(kind string, form simpleForm) (*anvilv1.Mirror, error) {
	name := form.Value("Name")
	if name == "" {
		return nil, fmt.Errorf("Name is required")
	}
	priority, err := strconv.Atoi(form.Value("Priority"))
	if err != nil {
		return nil, fmt.Errorf("Priority must be a number")
	}
	mirror := &anvilv1.Mirror{Name: name, Priority: int32(priority), Enabled: true}
	if kind == "container" {
		mirror.Kind = anvilv1.MirrorKind_MIRROR_KIND_CONTAINER
		mirror.Registry = form.Value("Registry (container only)")
		mirror.MirrorOf = form.Value("Mirror of (container only, optional)")
	} else {
		mirror.Kind = anvilv1.MirrorKind_MIRROR_KIND_VM
		mirror.ManifestUrl = form.Value("Manifest URL (vm only)")
	}
	return mirror, nil
}

func (m mirrorsModel) View() string {
	if m.confirmRemove != nil {
		return styleWarn.Render("Remove mirror "+m.confirmRemove.GetName()+"?") + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.adding {
		return styleSubtitle.Render("kind: "+m.addKind) + "\n\n" + m.addForm.View()
	}
	return m.list.View() + "\n" + helpBar("a", "add", "e", "enable/disable", "x", "remove", "r", "refresh", "esc", "back")
}
