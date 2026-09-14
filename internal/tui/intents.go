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

	case tea.KeyMsg:
		return m.updateIntentsKey(msg)
	}
	return m, nil
}

func (m model) updateIntentsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.intents

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
	}

	var cmd tea.Cmd
	in.list, cmd = in.list.Update(msg)
	return m, cmd
}

func (m intentsModel) View() string {
	if m.confirmDelete != nil {
		return styleWarn.Render(fmt.Sprintf("Remove intent %q?", m.confirmDelete.GetName())) + "\n\n" +
			helpBar("y", "ungroup only", "p", "ungroup and purge members", "any other key", "cancel")
	}
	if m.showingInfo != nil {
		var lines []string
		for _, mem := range m.showingInfo.GetMembers() {
			lines = append(lines, fmt.Sprintf("  %s  (%s)", mem.GetRole(), kindLabel(mem.GetKind())))
		}
		body := "(no members)"
		if len(lines) > 0 {
			body = strings.Join(lines, "\n")
		}
		return styleTitle.Render(" "+m.showingInfo.GetName()+" ") + "\n\n" + body + "\n\n" +
			helpBar("any key", "back")
	}
	return m.list.View() + "\n" + helpBar("i", "members", "x", "remove", "r", "refresh", "esc", "back")
}

func kindLabel(k anvilv1.Kind) string {
	if k == anvilv1.Kind_KIND_CONTAINER {
		return "container"
	}
	return "vm"
}
