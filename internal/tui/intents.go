package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/format"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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
// view members, `U` to ungroup one member and `x` to remove a group.
type intentsModel struct {
	list          list.Model
	showingInfo   *anvilv1.Intent // non-nil while showing a member-list modal
	confirmDelete *anvilv1.Intent

	removeMemberOf *anvilv1.Intent // non-nil while the remove-member form is up
	removeForm     simpleForm

	exportTarget *anvilv1.Intent // non-nil while the export output-path form is up
	exportForm   simpleForm
	export       transcript // export progress; a running one blocks other keys

	importPrompting bool // the import bundle-path form is up
	importForm      simpleForm
	imp             transcript // import progress; a running one blocks other keys

	applyPrompting bool // the apply manifest form is up
	applyForm      simpleForm
	applyReq       *anvilv1.IntentApplyRequest // the request whose plan is shown, sent again for real on confirm
	applyRunning   bool
	applyConfirm   bool // a dry run finished cleanly and waits for enter to apply
	applyLines     []string
}

func newIntentsModel() intentsModel {
	l := newList()
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
		return m, follow(&m.intents.export, msg.lineStep, nil)

	case importStreamMsg:
		return m, follow(&m.intents.imp, msg.lineStep, loadIntents(m.client))

	case applyStreamMsg:
		in := &m.intents
		in.applyLines = append(in.applyLines, msg.result...)
		if msg.status != "" {
			in.applyLines = appendProgressLine(in.applyLines, msg.status)
		}
		if msg.err != nil {
			in.applyRunning = false
			in.applyLines = append(in.applyLines, errLine(msg.err))
			return m, nil
		}
		if !msg.done {
			return m, msg.next
		}
		in.applyRunning = false
		if in.applyReq.GetDryRun() {
			in.applyConfirm = true
			return m, nil
		}
		return m, loadIntents(m.client)

	case tea.KeyMsg:
		return m.updateIntentsKey(msg)
	}
	return m, nil
}

func (m model) updateIntentsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.intents

	if in.export.running || in.imp.running || in.applyRunning {
		return m, nil // block input until the stream finishes
	}
	// A finished export/import's transcript stays on screen until dismissed here.
	if in.export.dismiss(msg.String()) || in.imp.dismiss(msg.String()) {
		return m, nil
	}
	if len(in.applyLines) > 0 {
		switch msg.String() {
		case "enter", "y":
			if in.applyConfirm {
				in.applyReq.DryRun = false
				in.applyConfirm, in.applyRunning, in.applyLines = false, true, nil
				return m, startApplyStream(m.client, in.applyReq)
			}
			in.applyLines = nil
		case "esc", "q", "n":
			in.applyLines, in.applyConfirm = nil, false
		}
		return m, nil
	}
	if in.applyPrompting {
		var submitted, cancelled bool
		in.applyForm, submitted, cancelled = in.applyForm.update(msg)
		if cancelled || !submitted {
			in.applyPrompting = !cancelled
			return m, nil
		}
		in.applyPrompting = false
		req, err := newApplyRequest(in.applyForm)
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		in.applyReq, in.applyRunning = req, true
		return m, startApplyStream(m.client, req)
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
		in.imp.start()
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
		in.export.start()
		return m, startExportStream(m.client, target.GetName(), outputPath, true)
	}

	if in.removeMemberOf != nil {
		var submitted, cancelled bool
		in.removeForm, submitted, cancelled = in.removeForm.update(msg)
		if cancelled || !submitted {
			if cancelled {
				in.removeMemberOf = nil
			}
			return m, nil
		}
		name, member := in.removeMemberOf.GetName(), strings.TrimSpace(in.removeForm.Value("Member"))
		in.removeMemberOf = nil
		if member == "" {
			return m, nil
		}
		return m, removeIntentMember(m.client, name, member)
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
	case "U":
		if item, ok := in.list.SelectedItem().(intentItem); ok {
			member := textField("Member", "role or instance ID, kept as a standalone instance", "")
			for _, mem := range item.intent.GetMembers() {
				member.Suggestions = append(member.Suggestions, mem.GetRole())
			}
			in.removeMemberOf = item.intent
			in.removeForm = newSimpleForm("Remove a member from "+item.intent.GetName(), []formField{member})
		}
		return m, nil
	case "E":
		if item, ok := in.list.SelectedItem().(intentItem); ok {
			in.exportTarget = item.intent
			in.exportForm = newSimpleForm("Export "+item.intent.GetName(), []formField{
				m.exportPathField(item.intent.GetName()),
			})
		}
		return m, nil
	case "A":
		in.applyPrompting = true
		in.applyForm = newApplyForm()
		in.applyForm.SetHeight(contentHeight(m.height) - 2)
		return m, nil
	case "I":
		in.importPrompting = true
		in.importForm = newSimpleForm("Import bundle", []formField{
			m.hostPathField("Bundle path", "e.g. ./bundle.tar.zst", ""),
			textField("Rename (optional)", "renames the imported intent", ""),
		})
		return m, nil
	}

	var cmd tea.Cmd
	in.list, cmd = in.list.Update(msg)
	return m, cmd
}

func (m intentsModel) View() string {
	if m.export.active() {
		return m.export.view("Exporting…")
	}
	if m.exportTarget != nil {
		return m.exportForm.View()
	}
	if m.imp.active() {
		return m.imp.view("Importing…")
	}
	if m.importPrompting {
		return m.importForm.View()
	}
	if m.applyRunning || len(m.applyLines) > 0 {
		s := styleTitle.Render(" Apply ") + "\n\n" + strings.Join(m.applyLines, "\n") + "\n\n"
		switch {
		case m.applyConfirm:
			s += helpBar("enter", "apply", "esc", "cancel")
		case !m.applyRunning:
			s += helpBar("esc", "dismiss")
		}
		return s
	}
	if m.applyPrompting {
		return m.applyForm.View()
	}
	if m.removeMemberOf != nil {
		return m.removeForm.View()
	}
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Remove intent %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "ungroup only", "p", "ungroup and purge members", "any other key", "cancel")
	}
	if m.showingInfo != nil {
		lines := networkLines(m.showingInfo.GetNetwork())
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
	return m.list.View() + "\n" + helpBar("i", "members", "x", "remove", "U", "remove member", "E", "export", "I", "import", "A", "apply", "r", "refresh", "esc", "back")
}

// networkLines renders an intent's shared network: engine network, bridge, subnet and DNS.
func networkLines(n *anvilv1.IntentNetwork) []string {
	if n == nil {
		return nil
	}
	var lines []string
	if n.GetEngineNetworkName() != "" || n.GetBridgeInterface() != "" {
		lines = append(lines, fmt.Sprintf("  Network %s, bridge %s", n.GetEngineNetworkName(), n.GetBridgeInterface()))
	}
	if n.GetSubnet() != "" {
		lines = append(lines, fmt.Sprintf("  Subnet %s, gateway %s", n.GetSubnet(), n.GetGateway()))
	}
	if n.GetDnsDomain() != "" {
		lines = append(lines, fmt.Sprintf("  DNS zone %s, server %s", n.GetDnsDomain(), n.GetDnsServer()))
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	return lines
}

// memberLine renders one member as "role (kind)", plus its instance ID, address and
// DNS name when it has them.
func memberLine(mem *anvilv1.IntentMember) string {
	line := fmt.Sprintf("  %s  (%s)", mem.GetRole(), format.Kind(mem.GetKind()))
	if id := mem.GetInstanceId(); id != "" {
		line += "  " + id
	}
	if mem.GetIp() != "" {
		line += "  " + mem.GetIp()
	}
	if mem.GetDnsName() != "" {
		line += "  " + mem.GetDnsName()
	}
	return line
}
