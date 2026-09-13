package tui

import (
	"os"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

type cloudInitItem struct{ name string }

func (i cloudInitItem) FilterValue() string { return i.name }
func (i cloudInitItem) Title() string       { return i.name }
func (i cloudInitItem) Description() string { return "" }

// cloudInitPrompt identifies which small overlay form (if any) is
// currently up over the list+editor split — new/import/rename each
// reuse the same simpleForm, just with different fields and a different
// completion action.
type cloudInitPrompt int

const (
	cloudInitPromptNone cloudInitPrompt = iota
	cloudInitPromptNew
	cloudInitPromptImport
	cloudInitPromptRename
	cloudInitPromptDelete
)

type cloudInitModel struct {
	list     list.Model
	editor   textarea.Model
	current  string
	dirty    bool
	editing  bool // focus is in the editor, not the list
	prompt   cloudInitPrompt
	promptFm simpleForm
}

func newCloudInitModel() cloudInitModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false) // small lists; also avoids single-letter shortcuts (n/s/d/...) colliding with filter typing
	l.Title = "Configs"
	l.SetShowHelp(false)
	ta := textarea.New()
	ta.Placeholder = "select or create a config to edit its cloud-init YAML…"
	return cloudInitModel{list: l, editor: ta}
}

func (m *cloudInitModel) setSize(width, height int) {
	listWidth := width / 3
	if listWidth < 20 {
		listWidth = 20
	}
	m.list.SetSize(listWidth, height)
	m.editor.SetWidth(width - listWidth - 4)
	m.editor.SetHeight(height - 2)
}

func (m model) updateCloudInit(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case cloudInitListLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing cloud-init configs: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.configs))
		for i, c := range msg.configs {
			items[i] = cloudInitItem{name: c.GetName()}
		}
		m.cloudInit.list.SetItems(items)
		return m, nil

	case cloudInitContentLoadedMsg:
		if msg.err != nil {
			m.setStatus("loading "+msg.name+": "+msg.err.Error(), true)
			return m, nil
		}
		m.cloudInit.current = msg.name
		m.cloudInit.dirty = false
		m.cloudInit.editor.SetValue(msg.content)
		return m, nil

	case cloudInitSavedMsg:
		if msg.err != nil {
			m.setStatus("saving "+msg.name+": "+msg.err.Error(), true)
			return m, nil
		}
		m.cloudInit.current = msg.name
		m.cloudInit.dirty = false
		m.setStatus("saved "+msg.name, false)
		return m, loadCloudInitList(m.client)

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus(msg.verb+": "+msg.err.Error(), true)
			return m, nil
		}
		m.cloudInit.current = ""
		m.cloudInit.editor.SetValue("")
		m.setStatus("config "+msg.verb, false)
		return m, loadCloudInitList(m.client)

	case tea.KeyMsg:
		return m.updateCloudInitKey(msg)
	}
	return m, nil
}

func (m model) updateCloudInitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.cloudInit

	if ci.prompt != cloudInitPromptNone {
		return m.updateCloudInitPrompt(msg)
	}

	if ci.editing {
		switch msg.String() {
		case "esc":
			ci.editing = false
			return m, nil
		case "ctrl+s":
			if ci.current == "" {
				m.setStatus("select or create a config first", true)
				return m, nil
			}
			return m, saveCloudInit(m.client, ci.current, ci.editor.Value())
		}
		var cmd tea.Cmd
		ci.editor, cmd = ci.editor.Update(msg)
		ci.dirty = true
		return m, cmd
	}

	switch msg.String() {
	case "esc", "q":
		m.screen = screenMenu
		return m, nil
	case "n":
		ci.prompt = cloudInitPromptNew
		ci.promptFm = newSimpleForm("New config", []formField{textField("Name", "", "")})
		return m, nil
	case "m":
		ci.prompt = cloudInitPromptImport
		ci.promptFm = newSimpleForm("Import config", []formField{
			textField("Name", "", ""),
			textField("Local file path", "", ""),
		})
		return m, nil
	case "r":
		if ci.current == "" {
			return m, nil
		}
		ci.prompt = cloudInitPromptRename
		ci.promptFm = newSimpleForm("Rename "+ci.current, []formField{textField("New name", "", "")})
		return m, nil
	case "d":
		if ci.current == "" {
			return m, nil
		}
		ci.prompt = cloudInitPromptDelete
		return m, nil
	case "enter", "tab":
		if item, ok := ci.list.SelectedItem().(cloudInitItem); ok {
			return m, loadCloudInitContent(m.client, item.name)
		}
		return m, nil
	case "e":
		if ci.current != "" {
			ci.editing = true
		}
		return m, nil
	}

	var cmd tea.Cmd
	ci.list, cmd = ci.list.Update(msg)
	return m, cmd
}

func (m model) updateCloudInitPrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.cloudInit

	if ci.prompt == cloudInitPromptDelete {
		switch msg.String() {
		case "y", "enter":
			name := ci.current
			ci.prompt = cloudInitPromptNone
			return m, deleteCloudInit(m.client, name)
		default:
			ci.prompt = cloudInitPromptNone
			return m, nil
		}
	}

	var submitted, cancelled bool
	ci.promptFm, submitted, cancelled = ci.promptFm.update(msg)
	if cancelled {
		ci.prompt = cloudInitPromptNone
		return m, nil
	}
	if !submitted {
		return m, nil
	}

	switch ci.prompt {
	case cloudInitPromptNew:
		name := ci.promptFm.Value("Name")
		ci.prompt = cloudInitPromptNone
		if name == "" {
			return m, nil
		}
		return m, saveCloudInit(m.client, name, "#cloud-config\n")

	case cloudInitPromptImport:
		name, path := ci.promptFm.Value("Name"), ci.promptFm.Value("Local file path")
		ci.prompt = cloudInitPromptNone
		if name == "" || path == "" {
			return m, nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			m.setStatus("reading "+path+": "+err.Error(), true)
			return m, nil
		}
		return m, saveCloudInit(m.client, name, string(data))

	case cloudInitPromptRename:
		newName := ci.promptFm.Value("New name")
		oldName := ci.current
		ci.prompt = cloudInitPromptNone
		if newName == "" {
			return m, nil
		}
		return m, renameCloudInit(m.client, oldName, newName)
	}
	return m, nil
}

func (m cloudInitModel) View() string {
	if m.prompt == cloudInitPromptDelete {
		return styleWarn.Render("Delete cloud-init config "+m.current+"?") + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.prompt != cloudInitPromptNone {
		return m.promptFm.View()
	}

	editorTitle := "Editor"
	if m.current != "" {
		editorTitle = "Editor: " + m.current
		if m.dirty {
			editorTitle += " *"
		}
	}
	editorBox := styleBox
	listBox := styleBox
	if m.editing {
		editorBox = styleBoxFocused
	} else {
		listBox = styleBoxFocused
	}

	left := listBox.Render(m.list.View())
	right := editorBox.Render(styleFieldLabel.Render(editorTitle) + "\n" + m.editor.View())

	help := helpBar("n", "new", "m", "import", "r", "rename", "d", "delete", "e", "edit", "ctrl+s", "save", "esc", "back")
	return left + "  " + right + "\n" + help
}
