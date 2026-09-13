package tui

import (
	"context"
	"fmt"
	"io"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// migrationView is the M8 checklist's migration view: a host picker
// (known hosts, see HostService — mDNS discovery from the original plan
// was deferred, never built, so this is the manual known-hosts list
// only, same as the CLI) plus a form for what to migrate and how, and an
// output panel streaming the same progress `anvil migrate` prints. Laid
// out as one screen with a form and a running log rather than the
// plan's "sequence of modals" phrasing — simpler to keep one thing
// legible at a time in a table-and-form-heavy TUI, same information
// flow as the CLI's own migrate flags either way.
type migrationView struct {
	app      *App
	root     *tview.Flex
	hostList *tview.List
	output   *tview.TextView
	hosts    []*anvilv1.Host

	nameField       *tview.InputField
	toField         *tview.InputField
	copyCheck       *tview.Checkbox
	bestEffortCheck *tview.Checkbox
	dryRunCheck     *tview.Checkbox
}

func newMigrationView(a *App) *migrationView {
	v := &migrationView{app: a}

	v.hostList = tview.NewList().ShowSecondaryText(true)
	v.hostList.SetBorder(true).SetTitle(" Known hosts — a:add  x:remove  t:test ")
	v.hostList.SetSelectedFunc(func(_ int, alias, _ string, _ rune) { v.toField.SetText(alias) })
	v.hostList.SetInputCapture(v.handleHostKey)

	v.output = tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	v.output.SetBorder(true).SetTitle(" Progress ")

	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Migrate ")
	v.nameField = tview.NewInputField().SetLabel("Instance or intent name").SetFieldWidth(30)
	v.toField = tview.NewInputField().SetLabel("To (host alias or user@host)").SetFieldWidth(30)
	v.copyCheck = tview.NewCheckbox().SetLabel("Copy (keep source)")
	v.bestEffortCheck = tview.NewCheckbox().SetLabel("Best-effort (intent only)")
	v.dryRunCheck = tview.NewCheckbox().SetLabel("Dry run")
	form.AddFormItem(v.nameField)
	form.AddFormItem(v.toField)
	form.AddFormItem(v.copyCheck)
	form.AddFormItem(v.bestEffortCheck)
	form.AddFormItem(v.dryRunCheck)
	form.AddButton("Migrate", v.runMigrate)

	left := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(v.hostList, 0, 1, true).
		AddItem(form, 13, 0, false)
	v.root = tview.NewFlex().
		AddItem(left, 40, 0, true).
		AddItem(v.output, 0, 1, false)

	v.refreshHosts()
	return v
}

func (v *migrationView) refreshHosts() {
	go func() {
		reply, err := v.app.client.Host.List(context.Background(), &anvilv1.HostListRequest{})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Listing hosts", err.Error())
				return
			}
			v.renderHosts(reply.GetHosts())
		})
	}()
}

func (v *migrationView) renderHosts(hosts []*anvilv1.Host) {
	v.hosts = hosts
	v.hostList.Clear()
	for _, h := range hosts {
		v.hostList.AddItem(h.GetAlias(), h.GetTarget(), 0, nil)
	}
	if len(hosts) == 0 {
		v.hostList.AddItem("(none — press a to add one)", "", 0, nil)
	}
}

func (v *migrationView) selectedHost() *anvilv1.Host {
	idx := v.hostList.GetCurrentItem()
	if idx < 0 || idx >= len(v.hosts) {
		return nil
	}
	return v.hosts[idx]
}

func (v *migrationView) handleHostKey(event *tcell.EventKey) *tcell.EventKey {
	switch event.Rune() {
	case 'a':
		v.promptAddHost()
		return nil
	case 'x':
		if h := v.selectedHost(); h != nil {
			v.removeHost(h)
		}
		return nil
	case 't':
		if h := v.selectedHost(); h != nil {
			v.testHost(h)
		}
		return nil
	}
	return event
}

func (v *migrationView) promptAddHost() {
	promptForm(v.app, "Add known host", []promptField{
		{Label: "Alias"}, {Label: "user@host[:port]"}, {Label: "Identity path (optional)"},
	}, func(values map[string]string) {
		alias, target := values["Alias"], values["user@host[:port]"]
		if alias == "" || target == "" {
			return
		}
		go func() {
			_, err := v.app.client.Host.Add(context.Background(), &anvilv1.HostAddRequest{
				Host: &anvilv1.Host{Alias: alias, Target: target, Identity: values["Identity path (optional)"]},
			})
			v.app.tapp.QueueUpdateDraw(func() {
				if err != nil {
					v.app.showError("Adding host", err.Error())
					return
				}
				v.refreshHosts()
			})
		}()
	})
}

func (v *migrationView) removeHost(h *anvilv1.Host) {
	go func() {
		_, err := v.app.client.Host.Remove(context.Background(), &anvilv1.HostRemoveRequest{Alias: h.GetAlias()})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Removing host", err.Error())
				return
			}
			v.refreshHosts()
		})
	}()
}

func (v *migrationView) testHost(h *anvilv1.Host) {
	v.appendOutput(fmt.Sprintf("[grey]testing %s…[-]\n", h.GetAlias()))
	go func() {
		reply, err := v.app.client.Host.Test(context.Background(), &anvilv1.HostTestRequest{Alias: h.GetAlias()})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.appendOutput(fmt.Sprintf("[red]%s: %s[-]\n", h.GetAlias(), err.Error()))
				return
			}
			// Test reports failure via Ok/Message, not a gRPC error —
			// same as internal/cli/commands/host.go's own handling.
			if !reply.GetOk() {
				v.appendOutput(fmt.Sprintf("[red]%s: %s[-]\n", h.GetAlias(), reply.GetMessage()))
				return
			}
			v.appendOutput(fmt.Sprintf("[green]%s: %s[-]\n", h.GetAlias(), reply.GetMessage()))
		})
	}()
}

func (v *migrationView) appendOutput(line string) {
	fmt.Fprint(v.output, line)
	v.output.ScrollToEnd()
}

func (v *migrationView) runMigrate() {
	name, to := v.nameField.GetText(), v.toField.GetText()
	if name == "" || to == "" {
		v.app.showError("Migrate", "both the name and target are required")
		return
	}
	req := &anvilv1.MigrateRequest{
		Name:       name,
		To:         to,
		Copy:       v.copyCheck.IsChecked(),
		BestEffort: v.bestEffortCheck.IsChecked(),
		DryRun:     v.dryRunCheck.IsChecked(),
	}
	v.output.Clear()
	v.appendOutput(fmt.Sprintf("[grey]migrating %q to %q…[-]\n", name, to))

	go func() {
		stream, err := v.app.client.Migrate.Migrate(context.Background(), req)
		if err != nil {
			v.app.tapp.QueueUpdateDraw(func() { v.appendOutput(fmt.Sprintf("[red]%s[-]\n", err.Error())) })
			return
		}
		for {
			ev, err := stream.Recv()
			if err != nil {
				v.app.tapp.QueueUpdateDraw(func() {
					if err != io.EOF {
						v.appendOutput(fmt.Sprintf("[red]%s[-]\n", err.Error()))
					}
				})
				return
			}
			switch e := ev.GetEvent().(type) {
			case *anvilv1.MigrateProgress_Status:
				v.app.tapp.QueueUpdateDraw(func() { v.appendOutput(e.Status + "\n") })
			case *anvilv1.MigrateProgress_Error:
				v.app.tapp.QueueUpdateDraw(func() { v.appendOutput(fmt.Sprintf("[red]%s[-]\n", e.Error)) })
				return
			case *anvilv1.MigrateProgress_Done:
				v.app.tapp.QueueUpdateDraw(func() {
					v.appendOutput(fmt.Sprintf("[green]migrated: new instance %s[-]\n", e.Done))
				})
			case *anvilv1.MigrateProgress_MemberDone:
				v.app.tapp.QueueUpdateDraw(func() {
					mr := e.MemberDone
					if mr.GetError() != "" {
						v.appendOutput(fmt.Sprintf("[red]  %s: FAILED: %s[-]\n", mr.GetRole(), mr.GetError()))
					} else {
						v.appendOutput(fmt.Sprintf("[green]  %s: migrated as %s[-]\n", mr.GetRole(), mr.GetNewId()))
					}
				})
			case *anvilv1.MigrateProgress_IntentDone:
				v.app.tapp.QueueUpdateDraw(func() {
					done := e.IntentDone
					if done.GetRolledBack() {
						v.appendOutput(fmt.Sprintf("[red]rolled back: %q failed as a group[-]\n", done.GetIntentName()))
					} else {
						v.appendOutput(fmt.Sprintf("[green]migrated intent %q[-]\n", done.GetIntentName()))
					}
				})
			}
		}
	}()
}
