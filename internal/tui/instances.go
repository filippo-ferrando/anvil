package tui

import (
	"context"
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// instancesView is the M8 checklist's "instances table": every instance
// (VM or container), sortable-by-eye columns, bindings for the same
// lifecycle actions the CLI already exposes — no logic of its own beyond
// translating a keypress into the same RPC `anvil start`/`stop`/`delete`/
// `info` already call.
type instancesView struct {
	app       *App
	root      *tview.Flex
	table     *tview.Table
	instances []*anvilv1.Instance // index-aligned with table rows, offset by the header row
}

func newInstancesView(a *App) *instancesView {
	v := &instancesView{app: a}
	v.table = tview.NewTable().SetSelectable(true, false).SetFixed(1, 0)
	v.table.SetBorder(true).SetTitle(" Instances — l:launch  r:start/stop  d:delete  i:info  s:shell  R:refresh ")
	v.table.SetInputCapture(v.handleKey)
	v.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(v.table, 0, 1, true)
	v.refresh()
	return v
}

func (v *instancesView) refresh() {
	go func() {
		reply, err := v.app.client.List(context.Background(), &anvilv1.ListRequest{})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Listing instances", err.Error())
				return
			}
			v.render(reply.GetInstances())
		})
	}()
}

func (v *instancesView) render(instances []*anvilv1.Instance) {
	v.instances = instances
	v.table.Clear()

	for col, h := range []string{"NAME", "KIND", "STATE", "IMAGE"} {
		v.table.SetCell(0, col, tview.NewTableCell(h).
			SetSelectable(false).
			SetTextColor(tcell.ColorYellow).
			SetAttributes(tcell.AttrBold))
	}

	if len(instances) == 0 {
		v.table.SetCell(1, 0, tview.NewTableCell("(no instances — press l to launch one)").SetSelectable(false))
		return
	}

	for row, inst := range instances {
		kind, image := "vm", inst.GetVm().GetImageRef()
		if inst.GetKind() == anvilv1.Kind_KIND_CONTAINER {
			kind, image = "container", inst.GetContainer().GetImageRef()
		}
		v.table.SetCell(row+1, 0, tview.NewTableCell(inst.GetName()))
		v.table.SetCell(row+1, 1, tview.NewTableCell(kind))
		v.table.SetCell(row+1, 2, tview.NewTableCell(stateLabel(inst.GetState())))
		v.table.SetCell(row+1, 3, tview.NewTableCell(image))
	}
}

// selected returns the instance backing the currently-highlighted row, or
// nil if there's nothing selected yet (an empty list, or the header row).
func (v *instancesView) selected() *anvilv1.Instance {
	row, _ := v.table.GetSelection()
	idx := row - 1
	if idx < 0 || idx >= len(v.instances) {
		return nil
	}
	return v.instances[idx]
}

func (v *instancesView) handleKey(event *tcell.EventKey) *tcell.EventKey {
	switch event.Rune() {
	case 'l':
		showLaunchForm(v.app, v.refresh)
		return nil
	case 'R':
		v.refresh()
		return nil
	case 'i':
		if inst := v.selected(); inst != nil {
			v.showInfo(inst)
		}
		return nil
	case 'r':
		if inst := v.selected(); inst != nil {
			v.toggleRun(inst)
		}
		return nil
	case 'd':
		if inst := v.selected(); inst != nil {
			v.confirmDelete(inst)
		}
		return nil
	case 's':
		if inst := v.selected(); inst != nil {
			v.shellInto(inst)
		}
		return nil
	}
	return event
}

func (v *instancesView) showInfo(inst *anvilv1.Instance) {
	text := fmt.Sprintf("Name:  %s\nID:    %s\nKind:  %s\nState: %s",
		inst.GetName(), inst.GetId(), kindLabel(inst.GetKind()), stateLabel(inst.GetState()))
	if vm := inst.GetVm(); vm != nil {
		text += fmt.Sprintf("\nImage: %s\nCPUs:  %d\nMemory: %d MiB", vm.GetImageRef(), vm.GetCpus(), vm.GetMemoryMib())
	}
	if c := inst.GetContainer(); c != nil {
		text += fmt.Sprintf("\nImage: %s", c.GetImageRef())
	}
	modal := tview.NewModal().SetText(text).AddButtons([]string{"OK"}).
		SetDoneFunc(func(int, string) { v.app.pages.RemovePage("info") })
	v.app.pages.AddPage("info", modal, true, true)
}

func (v *instancesView) toggleRun(inst *anvilv1.Instance) {
	go func() {
		var err error
		if inst.GetState() == anvilv1.State_STATE_RUNNING {
			_, err = v.app.client.Stop(context.Background(), &anvilv1.StopRequest{Names: []string{inst.GetName()}})
		} else {
			_, err = v.app.client.Start(context.Background(), &anvilv1.StartRequest{Names: []string{inst.GetName()}})
		}
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Start/stop", err.Error())
				return
			}
			v.refresh()
		})
	}()
}

func (v *instancesView) confirmDelete(inst *anvilv1.Instance) {
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Delete %q? (recoverable via `anvil purge` until then)", inst.GetName())).
		AddButtons([]string{"Cancel", "Delete"}).
		SetDoneFunc(func(_ int, label string) {
			v.app.pages.RemovePage("confirm-delete")
			if label != "Delete" {
				return
			}
			go func() {
				_, err := v.app.client.Delete(context.Background(), &anvilv1.DeleteRequest{Names: []string{inst.GetName()}})
				v.app.tapp.QueueUpdateDraw(func() {
					if err != nil {
						v.app.showError("Delete", err.Error())
						return
					}
					v.refresh()
				})
			}()
		})
	v.app.pages.AddPage("confirm-delete", modal, true, true)
}

// shellInto hands the real terminal to a live SSH session into inst,
// suspending the TUI for the duration — see shell.go.
func (v *instancesView) shellInto(inst *anvilv1.Instance) {
	if inst.GetVm() == nil {
		v.app.showError("Shell", "only a VM has an SSH shell — a container's shell isn't wired up in the TUI yet, use `anvil exec` from a real terminal")
		return
	}
	v.app.tapp.Suspend(func() {
		if err := shellIntoVM(inst); err != nil {
			fmt.Printf("\nanvil: %v\npress enter to return to the TUI…", err)
			fmt.Scanln()
		}
	})
}

func stateLabel(s anvilv1.State) string {
	switch s {
	case anvilv1.State_STATE_STOPPED:
		return "stopped"
	case anvilv1.State_STATE_STARTING:
		return "starting"
	case anvilv1.State_STATE_RUNNING:
		return "running"
	case anvilv1.State_STATE_STOPPING:
		return "stopping"
	case anvilv1.State_STATE_DELETING:
		return "deleting"
	case anvilv1.State_STATE_DELETED:
		return "deleted"
	case anvilv1.State_STATE_ERROR:
		return "error"
	default:
		return "unknown"
	}
}

func kindLabel(k anvilv1.Kind) string {
	if k == anvilv1.Kind_KIND_CONTAINER {
		return "container"
	}
	return "vm"
}
