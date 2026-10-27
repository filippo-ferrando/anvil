package tui

import (
	"errors"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
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

	name  string // only this instance's events, "" for all
	limit int32  // newest events to load, 0 for all

	filtering  bool // the filter form is up
	filterForm simpleForm
}

func newEventsModel() eventsModel {
	l := newList()
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Events"
	l.SetShowHelp(false)
	return eventsModel{list: l, limit: eventsScreenLimit}
}

func (m eventsModel) load(c *client.Client) tea.Cmd {
	return loadEvents(c, m.name, m.limit)
}

// newEventsFilterForm offers the instance names in instances for the Instance field.
func newEventsFilterForm(name string, limit int32, instances []string) simpleForm {
	inst := textField("Instance", "only this instance's events, blank for all", name)
	inst.Suggestions = instances
	return newSimpleForm("Filter events", []formField{
		inst,
		textField("Limit", "newest events to load, 0 for all", strconv.Itoa(int(limit))),
	})
}

// applyFilter reads the filter form into m and titles the list after it.
func (m *eventsModel) applyFilter(form simpleForm) error {
	limit, err := parseInt32(form.Value("Limit"), "Limit")
	if err != nil {
		return err
	}
	if limit < 0 {
		return errors.New("limit must be 0 or more")
	}
	m.name, m.limit = form.Value("Instance"), limit
	m.list.Title = "Events"
	if m.name != "" {
		m.list.Title += " of " + m.name
	}
	return nil
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
		if m.events.filtering {
			var submitted, cancelled bool
			m.events.filterForm, submitted, cancelled = m.events.filterForm.update(msg)
			if cancelled {
				m.events.filtering = false
			}
			if !submitted {
				return m, nil
			}
			if err := m.events.applyFilter(m.events.filterForm); err != nil {
				m.events.filterForm.errMsg = err.Error()
				return m, nil
			}
			m.events.filtering = false
			return m, m.events.load(m.client)
		}
		switch msg.String() {
		case "esc", "q":
			m.sidebarFocused = true
			return m, nil
		case "r":
			return m, m.events.load(m.client)
		case "f":
			var names []string
			for _, it := range m.instances.list.Items() {
				if ii, ok := it.(instanceItem); ok {
					names = append(names, ii.inst.GetName())
				}
			}
			m.events.filtering = true
			m.events.filterForm = newEventsFilterForm(m.events.name, m.events.limit, names)
			return m, nil
		}
		var cmd tea.Cmd
		m.events.list, cmd = m.events.list.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m eventsModel) View() string {
	if m.filtering {
		return m.filterForm.View()
	}
	return m.list.View() + "\n" + helpBar("f", "filter", "r", "refresh", "esc", "back")
}
