package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

type intentItem struct{ intent *anvilv1.Intent }

func (i intentItem) FilterValue() string { return i.intent.GetName() }
func (i intentItem) Title() string       { return i.intent.GetName() }
func (i intentItem) Description() string {
	net := "no shared network"
	if n := i.intent.GetNetwork(); n != nil {
		net = "network " + n.GetSubnet()
	}
	return fmt.Sprintf("%d member(s)  •  %s", len(i.intent.GetMembers()), net)
}

// intentsModel is the Intents page: lists intents, with `i`/`enter` to
// view members and `x` to remove a group.
type intentsModel struct {
	list          list.Model
	showingInfo   *anvilv1.Intent // non-nil while showing a member-list modal
	confirmDelete *anvilv1.Intent

	exportTarget *anvilv1.Intent // non-nil while the export output-path form is up
	exportForm   simpleForm
	exporting    bool     // an export stream is in flight, blocking other keys
	exportLines  []string // the finished (or in-flight) export's progress transcript

	importPrompting bool // the import bundle-path form is up
	importForm      simpleForm
	importing       bool     // an import stream is in flight, blocking other keys
	importLines     []string // the finished (or in-flight) import's progress transcript
}

func newIntentsModel() intentsModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false)
	l.Title = "Intents"
	l.SetShowHelp(false)
	return intentsModel{list: l}
}

func loadIntents(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Intent.List(context.Background(), &anvilv1.IntentListRequest{})
		if err != nil {
			return intentsLoadedMsg{err: err}
		}
		return intentsLoadedMsg{intents: reply.GetIntents()}
	}
}

func deleteIntent(c *client.Client, name string, purgeMembers bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Intent.Delete(context.Background(), &anvilv1.IntentDeleteRequest{Name: name, PurgeMembers: purgeMembers})
		verb := "removed"
		if purgeMembers {
			verb = "removed, members purged"
		}
		return actionDoneMsg{screen: screenIntents, verb: verb, err: err}
	}
}

func (m model) updateIntents(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case intentsLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing intents: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.intents))
		for i, it := range msg.intents {
			items[i] = intentItem{intent: it}
		}
		m.intents.list.SetItems(items)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("intent "+msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("intent "+msg.verb, false)
		}
		return m, loadIntents(m.client)

	case exportStreamMsg:
		if msg.line != "" {
			m.intents.exportLines = appendProgressLine(m.intents.exportLines, msg.line)
		}
		if msg.err != nil {
			m.intents.exporting = false
			m.intents.exportLines = append(m.intents.exportLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			m.intents.exporting = false
			return m, nil
		}
		return m, receiveExportEvent(msg.stream)

	case importStreamMsg:
		if msg.line != "" {
			m.intents.importLines = appendProgressLine(m.intents.importLines, msg.line)
		}
		if msg.err != nil {
			m.intents.importing = false
			m.intents.importLines = append(m.intents.importLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			m.intents.importing = false
			return m, loadIntents(m.client)
		}
		return m, receiveImportEvent(msg.stream)

	case tea.KeyMsg:
		return m.updateIntentsKey(msg)
	}
	return m, nil
}

func (m model) updateIntentsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.intents

	if in.exporting || in.importing {
		return m, nil // block input until the stream finishes
	}
	// A finished export/import's transcript stays on screen until dismissed here.
	if len(in.exportLines) > 0 {
		switch msg.String() {
		case "esc", "enter", "q":
			in.exportLines = nil
		}
		return m, nil
	}
	if len(in.importLines) > 0 {
		switch msg.String() {
		case "esc", "enter", "q":
			in.importLines = nil
		}
		return m, nil
	}
	if in.importPrompting {
		var submitted, cancelled bool
		in.importForm, submitted, cancelled = in.importForm.update(msg)
		if cancelled {
			in.importPrompting = false
			return m, nil
		}
		if !submitted {
			return m, nil
		}
		bundlePath := in.importForm.Value("Bundle path")
		renameTo := in.importForm.Value("Rename (optional)")
		in.importPrompting = false
		if bundlePath == "" {
			return m, nil
		}
		in.importing = true
		in.importLines = nil
		return m, startImportStream(m.client, bundlePath, renameTo)
	}
	if in.exportTarget != nil {
		var submitted, cancelled bool
		in.exportForm, submitted, cancelled = in.exportForm.update(msg)
		if cancelled {
			in.exportTarget = nil
			return m, nil
		}
		if !submitted {
			return m, nil
		}
		outputPath := in.exportForm.Value("Output path")
		target := in.exportTarget
		in.exportTarget = nil
		if outputPath == "" {
			return m, nil
		}
		in.exporting = true
		in.exportLines = nil
		return m, startExportStream(m.client, target.GetName(), outputPath, true)
	}

	if in.confirmDelete != nil {
		switch msg.String() {
		case "y", "enter":
			name := in.confirmDelete.GetName()
			in.confirmDelete = nil
			return m, deleteIntent(m.client, name, false)
		case "p":
			name := in.confirmDelete.GetName()
			in.confirmDelete = nil
			return m, deleteIntent(m.client, name, true)
		default:
			in.confirmDelete = nil
			return m, nil
		}
	}

	if in.showingInfo != nil {
		in.showingInfo = nil
		return m, nil
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "r":
		return m, loadIntents(m.client)
	case "i", "enter":
		if item, ok := in.list.SelectedItem().(intentItem); ok {
			in.showingInfo = item.intent
		}
		return m, nil
	case "x":
		if item, ok := in.list.SelectedItem().(intentItem); ok {
			in.confirmDelete = item.intent
		}
		return m, nil
	case "E":
		if item, ok := in.list.SelectedItem().(intentItem); ok {
			in.exportTarget = item.intent
			in.exportForm = newSimpleForm("Export "+item.intent.GetName(), []formField{
				pathField("Output path", "e.g. ./"+item.intent.GetName()+".tar.zst", item.intent.GetName()+".tar.zst"),
			})
		}
		return m, nil
	case "I":
		in.importPrompting = true
		in.importForm = newSimpleForm("Import bundle", []formField{
			pathField("Bundle path", "e.g. ./bundle.tar.zst", ""),
			textField("Rename (optional)", "renames the imported intent", ""),
		})
		return m, nil
	}

	var cmd tea.Cmd
	in.list, cmd = in.list.Update(msg)
	return m, cmd
}

func (m intentsModel) View() string {
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
	if m.exportTarget != nil {
		return m.exportForm.View()
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
	if m.importPrompting {
		return m.importForm.View()
	}
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Remove intent %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "ungroup only", "p", "ungroup and purge members", "any other key", "cancel")
	}
	if m.showingInfo != nil {
		var lines []string
		if n := m.showingInfo.GetNetwork(); n != nil && n.GetDnsDomain() != "" {
			lines = append(lines, fmt.Sprintf("  DNS zone %s, server %s", n.GetDnsDomain(), n.GetDnsServer()), "")
		}
		for _, mem := range m.showingInfo.GetMembers() {
			lines = append(lines, memberLine(mem))
		}
		body := "(no members)"
		if len(lines) > 0 {
			body = strings.Join(lines, "\n")
		}
		return styleTitle.Render(" "+m.showingInfo.GetName()+" ") + "\n\n" + body + "\n\n" +
			helpBar("any key", "back")
	}
	return m.list.View() + "\n" + helpBar("i", "members", "x", "remove", "E", "export", "I", "import", "r", "refresh", "esc", "back")
}

// memberLine renders one member as "role (kind)", plus its address and
// DNS name when it has them.
func memberLine(mem *anvilv1.IntentMember) string {
	line := fmt.Sprintf("  %s  (%s)", mem.GetRole(), kindLabel(mem.GetKind()))
	if mem.GetIp() != "" {
		line += "  " + mem.GetIp()
	}
	if mem.GetDnsName() != "" {
		line += "  " + mem.GetDnsName()
	}
	return line
}

func kindLabel(k anvilv1.Kind) string {
	if k == anvilv1.Kind_KIND_CONTAINER {
		return "container"
	}
	return "vm"
}
