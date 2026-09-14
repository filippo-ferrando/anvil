package tui

import (
	"context"
	"fmt"
	"io"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

type hostItem struct{ host *anvilv1.Host }

func (i hostItem) FilterValue() string { return i.host.GetAlias() }
func (i hostItem) Title() string       { return i.host.GetAlias() }
func (i hostItem) Description() string { return i.host.GetTarget() }

type migrationModel struct {
	hosts         list.Model
	adding        bool
	addForm       simpleForm
	confirmRemove *anvilv1.Host

	focusForm     bool // false: hosts list has focus; true: the migrate form does
	migrateForm   simpleForm
	migrating     bool
	progressLines []string

	panelHeight int // height the hosts list is pinned to
}

func newMigrationModel() migrationModel {
	l := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	l.SetFilteringEnabled(false) // avoids single-letter shortcuts colliding with filter typing
	l.Title = "Known hosts"
	l.SetShowHelp(false)
	return migrationModel{hosts: l, migrateForm: newMigrateForm()}
}

func newMigrateForm() simpleForm {
	return newSimpleForm("Migrate", []formField{
		textField("Name", "an instance name, or a whole intent's name", ""),
		textField("To", "known host alias, or user@host[:port]", ""),
		toggleField("Copy", "keep the source instead of deleting it", false),
		toggleField("Best-effort", "intent only: keep whatever members succeed", false),
		toggleField("Dry run", "check connectivity and report the plan only", false),
	})
}

func (m *migrationModel) setSize(width, height int) {
	m.panelHeight = height - boxHeightOverhead
	if m.panelHeight < 3 {
		m.panelHeight = 3
	}
	m.hosts.SetSize(width/3, m.panelHeight)
}

func (m model) updateMigration(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case hostsLoadedMsg:
		if msg.err != nil {
			m.setStatus("listing hosts: "+msg.err.Error(), true)
			return m, nil
		}
		items := make([]list.Item, len(msg.hosts))
		for i, h := range msg.hosts {
			items[i] = hostItem{host: h}
		}
		m.migration.hosts.SetItems(items)
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.setStatus("host "+msg.verb+": "+msg.err.Error(), true)
		} else {
			m.setStatus("host "+msg.verb, false)
		}
		return m, loadHosts(m.client)

	case hostTestedMsg:
		if msg.err != nil {
			m.setStatus(msg.alias+": "+msg.err.Error(), true)
		} else if !msg.ok {
			m.setStatus(msg.alias+": "+msg.detail, true)
		} else {
			m.setStatus(msg.alias+": "+msg.detail, false)
		}
		return m, nil

	case migrateStreamMsg:
		if msg.line != "" {
			m.migration.progressLines = appendProgressLine(m.migration.progressLines, msg.line)
		}
		if msg.err != nil {
			m.migration.migrating = false
			m.migration.progressLines = append(m.migration.progressLines, styleError.Render(msg.err.Error()))
			return m, nil
		}
		if msg.done {
			m.migration.migrating = false
			return m, nil
		}
		return m, receiveMigrateEvent(msg.stream)

	case tea.KeyMsg:
		return m.updateMigrationKey(msg)
	}
	return m, nil
}

func (m model) updateMigrationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	mg := &m.migration

	if mg.migrating {
		return m, nil
	}

	// A finished migration's transcript stays on screen until dismissed here.
	if len(mg.progressLines) > 0 {
		switch msg.String() {
		case "tab", "esc", "q":
			mg.progressLines = nil
			mg.focusForm = false
			if msg.String() != "tab" {
				m.sidebarFocused = true
			}
		}
		return m, nil
	}

	if mg.confirmRemove != nil {
		switch msg.String() {
		case "y", "enter":
			alias := mg.confirmRemove.GetAlias()
			mg.confirmRemove = nil
			return m, removeHost(m.client, alias)
		default:
			mg.confirmRemove = nil
			return m, nil
		}
	}

	if mg.adding {
		var submitted, cancelled bool
		mg.addForm, submitted, cancelled = mg.addForm.update(msg)
		if cancelled {
			mg.adding = false
			return m, nil
		}
		if submitted {
			alias, target := mg.addForm.Value("Alias"), mg.addForm.Value("user@host[:port]")
			mg.adding = false
			if alias == "" || target == "" {
				return m, nil
			}
			return m, addHost(m.client, &anvilv1.Host{
				Alias: alias, Target: target, Identity: mg.addForm.Value("Identity path (optional)"),
			})
		}
		return m, nil
	}

	if mg.focusForm {
		var submitted, cancelled bool
		mg.migrateForm, submitted, cancelled = mg.migrateForm.update(msg)
		if cancelled {
			mg.focusForm = false
			return m, nil
		}
		if submitted {
			name, to := mg.migrateForm.Value("Name"), mg.migrateForm.Value("To")
			if name == "" || to == "" {
				mg.migrateForm.errMsg = "both Name and To are required"
				return m, nil
			}
			req := &anvilv1.MigrateRequest{
				Name:       name,
				To:         to,
				Copy:       mg.migrateForm.Bool("Copy"),
				BestEffort: mg.migrateForm.Bool("Best-effort"),
				DryRun:     mg.migrateForm.Bool("Dry run"),
			}
			mg.migrating = true
			mg.progressLines = nil
			return m, startMigrateStream(m.client, req)
		}
		return m, nil
	}

	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "tab":
		mg.focusForm = true
		return m, nil
	case "a":
		mg.adding = true
		mg.addForm = newSimpleForm("Add known host", []formField{
			textField("Alias", "", ""),
			textField("user@host[:port]", "", ""),
			textField("Identity path (optional)", "", ""),
		})
		mg.addForm.SetHeight(contentHeight(m.height) - 2)
		return m, nil
	case "x":
		if h := m.selectedHost(); h != nil {
			mg.confirmRemove = h
		}
		return m, nil
	case "t":
		if h := m.selectedHost(); h != nil {
			return m, testHost(m.client, h.GetAlias())
		}
		return m, nil
	case "r":
		return m, loadHosts(m.client)
	}

	var cmd tea.Cmd
	mg.hosts, cmd = mg.hosts.Update(msg)
	return m, cmd
}

func (m model) selectedHost() *anvilv1.Host {
	item, ok := m.migration.hosts.SelectedItem().(hostItem)
	if !ok {
		return nil
	}
	return item.host
}

func (m migrationModel) View() string {
	if m.confirmRemove != nil {
		return styleWarn.Render("Remove host "+m.confirmRemove.GetAlias()+"?") + "\n\n" +
			helpBar("y", "confirm", "any other key", "cancel")
	}
	if m.adding {
		return m.addForm.View()
	}
	if m.migrating || len(m.progressLines) > 0 {
		s := styleTitle.Render(" Migrating… ") + "\n\n"
		for _, line := range m.progressLines {
			s += line + "\n"
		}
		if !m.migrating {
			s += "\n" + helpBar("tab", "back to hosts", "esc", "back")
		}
		return s
	}

	hostsBox, formBox := styleBox, styleBox
	if m.focusForm {
		formBox = styleBoxFocused
	} else {
		hostsBox = styleBoxFocused
	}
	// Pin the hosts list to a fixed height so it matches the form box beside it.
	left := hostsBox.Render(lipgloss.NewStyle().Height(m.panelHeight).Render(m.hosts.View()))
	right := formBox.Render(m.migrateForm.View())

	help := helpBar("tab", "switch focus", "a", "add host", "x", "remove", "t", "test", "esc", "back")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right) + "\n" + help
}

func startMigrateStream(c *client.Client, req *anvilv1.MigrateRequest) tea.Cmd {
	return func() tea.Msg {
		// Skip guest-key injection for a dry run.
		if !req.GetDryRun() {
			if err := injectGuestKeys(context.Background(), c, req.GetName(), req.GetTo()); err != nil {
				return migrateStreamMsg{err: fmt.Errorf("preparing guest SSH access on the target: %w", err), done: true}
			}
		}
		stream, err := c.Migrate.Migrate(context.Background(), req)
		if err != nil {
			return migrateStreamMsg{err: err, done: true}
		}
		return receiveMigrateEvent(stream)()
	}
}

func receiveMigrateEvent(stream anvilv1.MigrateService_MigrateClient) tea.Cmd {
	return func() tea.Msg {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return migrateStreamMsg{done: true}
			}
			return migrateStreamMsg{err: err, done: true}
		}
		switch e := ev.GetEvent().(type) {
		case *anvilv1.MigrateProgress_Status:
			return migrateStreamMsg{stream: stream, line: e.Status}
		case *anvilv1.MigrateProgress_Error:
			return migrateStreamMsg{err: fmt.Errorf("%s", e.Error), done: true}
		case *anvilv1.MigrateProgress_Done:
			return migrateStreamMsg{stream: stream, line: styleGood.Render("migrated: new instance " + e.Done)}
		case *anvilv1.MigrateProgress_MemberDone:
			mr := e.MemberDone
			if mr.GetError() != "" {
				return migrateStreamMsg{stream: stream, line: styleError.Render("  " + mr.GetRole() + ": FAILED: " + mr.GetError())}
			}
			return migrateStreamMsg{stream: stream, line: styleGood.Render("  " + mr.GetRole() + ": migrated as " + mr.GetNewId())}
		case *anvilv1.MigrateProgress_IntentDone:
			done := e.IntentDone
			if done.GetRolledBack() {
				return migrateStreamMsg{stream: stream, line: styleError.Render("rolled back: " + done.GetIntentName() + " failed as a group")}
			}
			return migrateStreamMsg{stream: stream, line: styleGood.Render("migrated intent " + done.GetIntentName())}
		default:
			return migrateStreamMsg{stream: stream}
		}
	}
}
