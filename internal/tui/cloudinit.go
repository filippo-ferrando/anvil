package tui

import (
	"os"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type cloudInitItem struct{ name string }

func (i cloudInitItem) FilterValue() string { return i.name }
func (i cloudInitItem) Title() string       { return i.name }
func (i cloudInitItem) Description() string { return "" }

// cloudInitPrompt identifies which overlay form, if any, is showing over the list+editor split.
type cloudInitPrompt int

const (
	cloudInitPromptNone cloudInitPrompt = iota
	cloudInitPromptNew
	cloudInitPromptImport
	cloudInitPromptImportRepo
	cloudInitPromptRename
	cloudInitPromptDelete
)

type cloudInitModel struct {
	list        list.Model
	editor      textarea.Model
	current     string
	dirty       bool
	editing     bool // focus is in the editor, not the list
	prompt      cloudInitPrompt
	promptFm    simpleForm
	panelHeight int // shared height for both side-by-side panels

	importingRepo bool // showing the ImportRepo streaming progress instead of the list+editor split
	importLines   []string
}

func newCloudInitModel() cloudInitModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Configs"
	l.SetShowHelp(false)
	ta := textarea.New()
	ta.Placeholder = "select or create a config to edit its cloud-init YAML…"
	return cloudInitModel{list: l, editor: ta}
}

// boxOverhead and boxHeightOverhead are the extra width/height styleBox's
// border and padding add beyond a panel's content.
const (
	boxOverhead       = 4
	boxHeightOverhead = 2
)

func (m *cloudInitModel) setSize(width, height int) {
	const gutter = 2 // spacer between the two boxes
	inner := width - 2*boxOverhead - gutter
	if inner < 20 {
		inner = 20
	}
	listWidth := inner / 3
	if listWidth < 16 {
		listWidth = 16
	}
	editorWidth := inner - listWidth
	if editorWidth < 12 {
		editorWidth = 12
	}

	// Both panels are pinned to this same content height.
	m.panelHeight = height - boxHeightOverhead - 1 // -1: the "Editor: name" title line
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}

	m.list.SetSize(listWidth, m.panelHeight+1) // +1: list has no separate title line
	m.editor.SetWidth(editorWidth)
	m.editor.SetHeight(m.panelHeight)
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

	case cloudInitImportStreamMsg:
		if msg.status != "" {
			m.cloudInit.importLines = appendProgressLine(m.cloudInit.importLines, msg.status)
		}
		if msg.result != nil {
			r := msg.result
			switch {
			case r.GetError() != "":
				m.cloudInit.importLines = append(m.cloudInit.importLines, styleError.Render(r.GetName()+": FAILED: "+r.GetError()))
			case r.GetSkipped():
				m.cloudInit.importLines = append(m.cloudInit.importLines, styleWarn.Render(r.GetName()+": skipped (already exists)"))
			default:
				m.cloudInit.importLines = append(m.cloudInit.importLines, styleGood.Render(r.GetName()+": imported"))
			}
		}
		if msg.err != nil {
			m.cloudInit.importLines = append(m.cloudInit.importLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			return m, loadCloudInitList(m.client)
		}
		return m, receiveCloudInitImportEvent(msg.stream)

	case tea.KeyMsg:
		return m.updateCloudInitKey(msg)
	}
	return m, nil
}

func (m model) updateCloudInitKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.cloudInit

	if ci.importingRepo {
		// Any key only dismisses the progress view; the import keeps running.
		ci.importingRepo = false
		return m, nil
	}

	if ci.prompt != cloudInitPromptNone {
		return m.updateCloudInitPrompt(msg)
	}

	if ci.editing {
		switch msg.String() {
		case "esc":
			ci.editing = false
			ci.editor.Blur()
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
		m.sidebarFocused = true
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
	case "R":
		ci.prompt = cloudInitPromptImportRepo
		ci.promptFm = newSimpleForm("Import from a repo (see docs/mirrors.md)", []formField{
			textField("Manifest URL", "", ""),
			toggleField("Force", "overwrite a name that's already in the library", false),
		})
		return m, nil
	case "enter", "tab":
		if item, ok := ci.list.SelectedItem().(cloudInitItem); ok {
			return m, loadCloudInitContent(m.client, item.name)
		}
		return m, nil
	case "e":
		if ci.current != "" {
			ci.editing = true
			return m, ci.editor.Focus()
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

	case cloudInitPromptImportRepo:
		url := ci.promptFm.Value("Manifest URL")
		force := ci.promptFm.Bool("Force")
		ci.prompt = cloudInitPromptNone
		if url == "" {
			return m, nil
		}
		ci.importingRepo = true
		ci.importLines = nil
		return m, startCloudInitImportRepo(m.client, url, force)
	}
	return m, nil
}

func (m cloudInitModel) View() string {
	if m.importingRepo {
		s := styleTitle.Render(" Importing… ") + "\n\n"
		for _, line := range m.importLines {
			s += line + "\n"
		}
		return s + "\n" + helpBar("any key", "dismiss (import keeps running)")
	}
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

	// Pin the list box to the same total height as the editor box.
	totalHeight := m.panelHeight + 1
	left := listBox.Render(lipgloss.NewStyle().Height(totalHeight).Render(m.list.View()))
	right := editorBox.Render(styleFieldLabel.Render(editorTitle) + "\n" + m.editor.View())

	help := helpBar("n", "new", "m", "import", "R", "import repo", "r", "rename",
		"d", "delete", "e", "edit", "ctrl+s", "save", "esc", "back")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}
