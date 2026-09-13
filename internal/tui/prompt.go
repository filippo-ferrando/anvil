package tui

import (
	"strings"

	"github.com/rivo/tview"
)

// promptField is one text field in a promptForm modal.
type promptField struct {
	Label   string
	Default string
}

// promptForm shows a small modal form (title, one InputField per field,
// OK/Cancel) — the shared building block behind cloudInitView's New/
// Import/Rename and mirrorsView's Add, so each doesn't hand-roll its own
// overlay layout. onSubmit is called with label -> trimmed text only if
// the user pressed OK; a Cancel just closes the modal.
func promptForm(app *App, title string, fields []promptField, onSubmit func(values map[string]string)) {
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" " + title + " ")
	for _, f := range fields {
		form.AddInputField(f.Label, f.Default, 40, nil, nil)
	}

	closePrompt := func() { app.pages.RemovePage("prompt") }

	form.AddButton("OK", func() {
		values := make(map[string]string, len(fields))
		for _, f := range fields {
			if item := form.GetFormItemByLabel(f.Label); item != nil {
				if input, ok := item.(*tview.InputField); ok {
					values[f.Label] = strings.TrimSpace(input.GetText())
				}
			}
		}
		closePrompt()
		onSubmit(values)
	})
	form.AddButton("Cancel", closePrompt)

	height := 6 + 2*len(fields)
	modal := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(form, 60, 0, true).
			AddItem(nil, 0, 1, false),
			height, 0, true).
		AddItem(nil, 0, 1, false)

	app.pages.AddPage("prompt", modal, true, true)
	app.tapp.SetFocus(form)
}
