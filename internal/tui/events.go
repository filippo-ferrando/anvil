package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

type eventItem struct{ ev *anvilv1.InstanceEvent }

func (i eventItem) FilterValue() string { return i.ev.GetInstanceName() }

// Title stays uncolored, like stateGlyph: the list re-styles selected items wholesale.
func (i eventItem) Title() string {
	mark := "•"
	if i.ev.GetError() != "" {
		mark = "✕"
	}
	at := time.Unix(i.ev.GetTimeUnix(), 0).Format(time.DateTime)
	return mark + " " + at + "  " + i.ev.GetInstanceName() + "  " + i.ev.GetAction()
}

func (i eventItem) Description() string {
	desc := "by " + i.ev.GetActor()
	if msg := i.ev.GetMessage(); msg != "" {
		desc += "  •  " + msg
	}
	if e := i.ev.GetError(); e != "" {
		desc += "  •  failed: " + e
	}
	return desc
}

type eventsModel struct {
	list list.Model
}

func newEventsModel() eventsModel {
	l := newList()
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Events"
	l.SetShowHelp(false)
	return eventsModel{list: l}
}

func (m model) updateEvents(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case eventsLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing events: "+msg.err.Error(), true)
			return m, nil
		}
		atTop := m.events.list.Index() == 0
		var selected uint64
		if it, ok := m.events.list.SelectedItem().(eventItem); ok {
			selected = it.ev.GetSeq()
		}
		// Newest first.
		items := make([]list.Item, len(msg.events))
		for i, ev := range msg.events {
			items[len(items)-1-i] = eventItem{ev: ev}
		}
		m.events.list.SetItems(items)
		// New events land on top: the cursor follows them from the top, else stays on its event.
		for i, it := range items {
			if !atTop && it.(eventItem).ev.GetSeq() == selected {
				m.events.list.Select(i)
				break
			}
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.sidebarFocused = true
			return m, nil
		case "r":
			return m, loadEvents(m.client)
		}
		var cmd tea.Cmd
		m.events.list, cmd = m.events.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m eventsModel) View() string {
	return m.list.View() + "\n" + helpBar("r", "refresh", "esc", "back")
}
