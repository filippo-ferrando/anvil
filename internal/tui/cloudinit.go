package tui

import (
	"context"
	"fmt"
	"os"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

// cloudInitView is the M8 checklist's cloud-init view: a list of saved
// configs (left) and a text editor (right), matching the plan's
// description of the pattern worth keeping from Hyperpass (not its
// code) — New/Import/Rename/Delete/Save, backed by the same
// CloudInitService CRUD RPCs `anvil cloud-init *` already uses, so the
// CLI and the TUI always agree on the same library.
type cloudInitView struct {
	app    *App
	root   *tview.Flex
	list   *tview.List
	editor *tview.TextArea
	hint   *tview.TextView

	names   []string
	current string
	dirty   bool
}

func newCloudInitView(a *App) *cloudInitView {
	v := &cloudInitView{app: a}

	v.list = tview.NewList().ShowSecondaryText(false)
	v.list.SetBorder(true).SetTitle(" Configs — n:new  m:import  r:rename  d:delete ")
	v.list.SetSelectedFunc(func(i int, name, _ string, _ rune) { v.load(name) })
	v.list.SetInputCapture(v.handleListKey)

	v.editor = tview.NewTextArea()
	v.editor.SetBorder(true).SetTitle(" Editor — Ctrl+S: save ")
	v.editor.SetChangedFunc(func() { v.dirty = true; v.updateEditorTitle() })
	v.editor.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlS {
			v.save()
			return nil
		}
		return event
	})

	v.hint = tview.NewTextView().SetDynamicColors(true)

	v.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(
			tview.NewFlex().
				AddItem(v.list, 28, 0, true).
				AddItem(v.editor, 0, 1, false),
			0, 1, true,
		).
		AddItem(v.hint, 1, 0, false)

	v.refresh()
	return v
}

func (v *cloudInitView) refresh() {
	go func() {
		reply, err := v.app.client.CloudInit.List(context.Background(), &anvilv1.CloudInitListRequest{})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Listing cloud-init configs", err.Error())
				return
			}
			v.render(reply.GetConfigs())
		})
	}()
}

func (v *cloudInitView) render(configs []*anvilv1.CloudInitConfigInfo) {
	v.list.Clear()
	v.names = v.names[:0]
	for _, c := range configs {
		v.names = append(v.names, c.GetName())
		v.list.AddItem(c.GetName(), "", 0, nil)
	}
	if len(configs) == 0 {
		v.hint.SetText("[grey]no saved cloud-init configs yet — press n to create one[-]")
	}
}

func (v *cloudInitView) load(name string) {
	go func() {
		reply, err := v.app.client.CloudInit.Get(context.Background(), &anvilv1.CloudInitGetRequest{Name: name})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Loading "+name, err.Error())
				return
			}
			v.current = name
			v.dirty = false
			v.editor.SetText(reply.GetContent(), false)
			v.updateEditorTitle()
		})
	}()
}

func (v *cloudInitView) updateEditorTitle() {
	title := " Editor — Ctrl+S: save "
	if v.current != "" {
		title = fmt.Sprintf(" Editor: %s%s — Ctrl+S: save ", v.current, dirtyMark(v.dirty))
	}
	v.editor.SetTitle(title)
}

func dirtyMark(dirty bool) string {
	if dirty {
		return " *"
	}
	return ""
}

func (v *cloudInitView) save() {
	if v.current == "" {
		v.hint.SetText("[yellow]select or create a config first[-]")
		return
	}
	name, content := v.current, v.editor.GetText()
	go func() {
		_, err := v.app.client.CloudInit.Save(context.Background(), &anvilv1.CloudInitSaveRequest{Name: name, Content: content})
		v.app.tapp.QueueUpdateDraw(func() {
			if err != nil {
				v.app.showError("Saving "+name, err.Error())
				return
			}
			v.dirty = false
			v.updateEditorTitle()
			v.hint.SetText("[green]saved[-]")
		})
	}()
}

func (v *cloudInitView) handleListKey(event *tcell.EventKey) *tcell.EventKey {
	switch event.Rune() {
	case 'n':
		v.promptNew()
		return nil
	case 'm':
		v.promptImport()
		return nil
	case 'r':
		v.promptRename()
		return nil
	case 'd':
		v.promptDelete()
		return nil
	}
	return event
}

func (v *cloudInitView) promptNew() {
	promptForm(v.app, "New cloud-init config", []promptField{{Label: "Name"}}, func(values map[string]string) {
		name := values["Name"]
		if name == "" {
			return
		}
		go func() {
			_, err := v.app.client.CloudInit.Save(context.Background(), &anvilv1.CloudInitSaveRequest{Name: name, Content: "#cloud-config\n"})
			v.app.tapp.QueueUpdateDraw(func() {
				if err != nil {
					v.app.showError("Creating "+name, err.Error())
					return
				}
				v.refresh()
				v.load(name)
			})
		}()
	})
}

func (v *cloudInitView) promptImport() {
	promptForm(v.app, "Import cloud-init config", []promptField{{Label: "Name"}, {Label: "Local file path"}}, func(values map[string]string) {
		name, path := values["Name"], values["Local file path"]
		if name == "" || path == "" {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			v.app.showError("Reading "+path, err.Error())
			return
		}
		go func() {
			_, err := v.app.client.CloudInit.Save(context.Background(), &anvilv1.CloudInitSaveRequest{Name: name, Content: string(data)})
			v.app.tapp.QueueUpdateDraw(func() {
				if err != nil {
					v.app.showError("Importing "+name, err.Error())
					return
				}
				v.refresh()
				v.load(name)
			})
		}()
	})
}

func (v *cloudInitView) promptRename() {
	if v.current == "" {
		return
	}
	old := v.current
	promptForm(v.app, "Rename "+old, []promptField{{Label: "New name"}}, func(values map[string]string) {
		newName := values["New name"]
		if newName == "" {
			return
		}
		go func() {
			_, err := v.app.client.CloudInit.Rename(context.Background(), &anvilv1.CloudInitRenameRequest{OldName: old, NewName: newName})
			v.app.tapp.QueueUpdateDraw(func() {
				if err != nil {
					v.app.showError("Renaming "+old, err.Error())
					return
				}
				v.current = newName
				v.refresh()
			})
		}()
	})
}

func (v *cloudInitView) promptDelete() {
	if v.current == "" {
		return
	}
	name := v.current
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Delete cloud-init config %q?", name)).
		AddButtons([]string{"Cancel", "Delete"}).
		SetDoneFunc(func(_ int, label string) {
			v.app.pages.RemovePage("confirm-ci-delete")
			if label != "Delete" {
				return
			}
			go func() {
				_, err := v.app.client.CloudInit.Delete(context.Background(), &anvilv1.CloudInitDeleteRequest{Name: name})
				v.app.tapp.QueueUpdateDraw(func() {
					if err != nil {
						v.app.showError("Deleting "+name, err.Error())
						return
					}
					v.current = ""
					v.editor.SetText("", false)
					v.updateEditorTitle()
					v.refresh()
				})
			}()
		})
	v.app.pages.AddPage("confirm-ci-delete", modal, true, true)
}
