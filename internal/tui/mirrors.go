package tui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// mirrorsView is the M8 checklist's mirrors view: a table of configured
// image mirrors (name, kind, source, priority/enabled), with add/remove/
// enable-disable actions — backed by the same MirrorService RPCs `anvil
// mirror *` already uses.
type mirrorsView struct {
	app     *App
	root    *tview.Flex
	table   *tview.Table
	mirrors []*anvilv1.Mirror
}

func newMirrorsView(a *App) *mirrorsView {
	v := &mirrorsView{app: a}
	v.table = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	v.table.SetBorder(true).SetTitle(" Mirrors — a:add  e:enable/disable  x:remove  R:refresh ")
	v.table.SetInputCapture(v.handleKey)
	v.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(v.table, 0, 1, true)
	v.refresh()
	return v
}

func (v *mirrorsView) refresh() {
	go func() {
		reply, err := v.app.client.Mirror.List(context.Background(), &anvilv1.MirrorListRequest{})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Listing mirrors", err.Error())
				return
			}
			v.render(reply.GetMirrors())
		})
	}()
}

func (v *mirrorsView) render(mirrors []*anvilv1.Mirror) {
	v.mirrors = mirrors
	v.table.Clear()
	for col, h := range []string{"NAME", "KIND", "SOURCE", "PRIORITY", "ENABLED"} {
		v.table.SetCell(0, col, tview.NewTableCell(h).SetSelectable(false).SetTextColor(tcell.ColorYellow))
	}
	if len(mirrors) == 0 {
		v.table.SetCell(1, 0, tview.NewTableCell("(no mirrors — press a to add one)").SetSelectable(false))
		return
	}
	for row, m := range mirrors {
		kind, source := "vm", m.GetManifestUrl()
		if m.GetKind() == anvilv1.MirrorKind_MIRROR_KIND_CONTAINER {
			kind, source = "container", m.GetRegistry()
			if m.GetMirrorOf() != "" {
				source = fmt.Sprintf("%s (mirrors %s)", source, m.GetMirrorOf())
			}
		}
		v.table.SetCell(row+1, 0, tview.NewTableCell(m.GetName()))
		v.table.SetCell(row+1, 1, tview.NewTableCell(kind))
		v.table.SetCell(row+1, 2, tview.NewTableCell(source))
		v.table.SetCell(row+1, 3, tview.NewTableCell(fmt.Sprintf("%d", m.GetPriority())))
		v.table.SetCell(row+1, 4, tview.NewTableCell(fmt.Sprintf("%t", m.GetEnabled())))
	}
}

func (v *mirrorsView) selected() *anvilv1.Mirror {
	row, _ := v.table.GetSelection()
	idx := row - 1
	if idx < 0 || idx >= len(v.mirrors) {
		return nil
	}
	return v.mirrors[idx]
}

func (v *mirrorsView) handleKey(event *tcell.EventKey) *tcell.EventKey {
	switch event.Rune() {
	case 'a':
		v.showAddForm()
		return nil
	case 'R':
		v.refresh()
		return nil
	case 'e':
		if m := v.selected(); m != nil {
			v.toggleEnabled(m)
		}
		return nil
	case 'x':
		if m := v.selected(); m != nil {
			v.confirmRemove(m)
		}
		return nil
	}
	return event
}

func (v *mirrorsView) toggleEnabled(m *anvilv1.Mirror) {
	go func() {
		_, err := v.app.client.Mirror.SetEnabled(context.Background(), &anvilv1.MirrorSetEnabledRequest{
			Name: m.GetName(), Enabled: !m.GetEnabled(),
		})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Enable/disable", err.Error())
				return
			}
			v.refresh()
		})
	}()
}

func (v *mirrorsView) confirmRemove(m *anvilv1.Mirror) {
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Remove mirror %q?", m.GetName())).
		AddButtons([]string{"Cancel", "Remove"}).
		SetDoneFunc(func(_ int, label string) {
			v.app.pages.RemovePage("confirm-mirror-remove")
			if label != "Remove" {
				return
			}
			go func() {
				_, err := v.app.client.Mirror.Remove(context.Background(), &anvilv1.MirrorRemoveRequest{Name: m.GetName()})
				v.app.tapp.QueueUpdateDraw(func() {
					if err != nil {
						v.app.showError("Removing "+m.GetName(), err.Error())
						return
					}
					v.refresh()
				})
			}()
		})
	v.app.pages.AddPage("confirm-mirror-remove", modal, true, true)
}

func (v *mirrorsView) showAddForm() {
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Add mirror ")

	kindOptions := []string{"vm", "container"}
	kind := kindOptions[0]
	form.AddInputField("Name", "", 30, nil, nil)
	form.AddDropDown("Kind", kindOptions, 0, func(option string, _ int) { kind = option })
	form.AddInputField("Manifest URL (vm only)", "", 50, nil, nil)
	form.AddInputField("Registry host[:port] (container only)", "", 40, nil, nil)
	form.AddInputField("Mirror of (container only, optional)", "", 30, nil, nil)
	form.AddCheckbox("Insecure (container only)", false, nil)
	form.AddInputField("Priority", "0", 6, nil, nil)

	text := func(label string) string {
		if item := form.GetFormItemByLabel(label); item != nil {
			if input, ok := item.(*tview.InputField); ok {
				return input.GetText()
			}
		}
		return ""
	}
	checked := func(label string) bool {
		if item := form.GetFormItemByLabel(label); item != nil {
			if cb, ok := item.(*tview.Checkbox); ok {
				return cb.IsChecked()
			}
		}
		return false
	}

	closeForm := func() { v.app.pages.RemovePage("add-mirror") }

	form.AddButton("Add", func() {
		priority, err := strconv.Atoi(text("Priority"))
		if err != nil {
			v.app.showError("Add mirror", "Priority must be a number")
			return
		}
		m := &anvilv1.Mirror{
			Name:     text("Name"),
			Priority: int32(priority),
			Enabled:  true,
		}
		if kind == "container" {
			m.Kind = anvilv1.MirrorKind_MIRROR_KIND_CONTAINER
			m.Registry = text("Registry host[:port] (container only)")
			m.MirrorOf = text("Mirror of (container only, optional)")
			m.Insecure = checked("Insecure (container only)")
		} else {
			m.Kind = anvilv1.MirrorKind_MIRROR_KIND_VM
			m.ManifestUrl = text("Manifest URL (vm only)")
		}
		closeForm()
		go func() {
			_, err := v.app.client.Mirror.Add(context.Background(), &anvilv1.MirrorAddRequest{Mirror: m})
			v.app.tapp.QueueUpdateDraw(func() {
				if err != nil {
					v.app.showError("Adding mirror", err.Error())
					return
				}
				v.refresh()
			})
		}()
	})
	form.AddButton("Cancel", closeForm)

	modal := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(form, 70, 0, true).
			AddItem(nil, 0, 1, false),
			20, 0, true).
		AddItem(nil, 0, 1, false)

	v.app.pages.AddPage("add-mirror", modal, true, true)
	v.app.tapp.SetFocus(form)
}
