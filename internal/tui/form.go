package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// simpleForm is the one form component every screen's "add/launch/edit"
// flow builds on (the launch form, mirror-add, host-add, and the
// cloud-init new/import/rename prompts) — Tab/Shift+Tab between fields,
// Enter or a dedicated submit key to confirm, Esc to cancel. One
// implementation instead of six ad hoc ones is what keeps every form in
// this app behaving the same way.
type simpleForm struct {
	title  string
	fields []formField
	focus  int
	errMsg string
}

type fieldKind int

const (
	fieldText fieldKind = iota
	fieldToggle
)

type formField struct {
	Label string
	Hint  string // small muted line under the field, e.g. the expected format
	Kind  fieldKind

	input textinput.Model // fieldText
	on    bool            // fieldToggle
}

// textField/toggleField are the two ways to declare a formField —
// callers build a []formField with these instead of poking the
// unexported textinput.Model directly.
func textField(label, hint, value string) formField {
	ti := textinput.New()
	ti.Placeholder = hint
	ti.SetValue(value)
	ti.CharLimit = 256
	return formField{Label: label, Hint: hint, Kind: fieldText, input: ti}
}

func toggleField(label, hint string, on bool) formField {
	return formField{Label: label, Hint: hint, Kind: fieldToggle, on: on}
}

func newSimpleForm(title string, fields []formField) simpleForm {
	f := simpleForm{title: title, fields: fields}
	if len(f.fields) > 0 && f.fields[0].Kind == fieldText {
		f.fields[0].input.Focus()
	}
	return f
}

// formResult is what a screen reads back after the user submits — a
// simple label -> text map for text fields, checked separately via
// Bool() for toggles, so a screen doesn't need to know simpleForm's
// internals.
func (f simpleForm) Value(label string) string {
	for _, field := range f.fields {
		if field.Label == label {
			return strings.TrimSpace(field.input.Value())
		}
	}
	return ""
}

func (f simpleForm) Bool(label string) bool {
	for _, field := range f.fields {
		if field.Label == label {
			return field.on
		}
	}
	return false
}

// update handles the form's own keys (Tab/Shift+Tab/Space/typing) and
// returns (updated form, submitted, cancelled) — the caller (a screen's
// Update) checks submitted/cancelled to decide what to do next; nothing
// here calls back into application logic itself.
func (f simpleForm) update(msg tea.Msg) (simpleForm, bool, bool) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return f, false, false
	}

	switch keyMsg.String() {
	case "esc":
		return f, false, true
	case "tab", "down":
		f.blurCurrent()
		f.focus = (f.focus + 1) % len(f.fields)
		f.focusCurrent()
		return f, false, false
	case "shift+tab", "up":
		f.blurCurrent()
		f.focus = (f.focus - 1 + len(f.fields)) % len(f.fields)
		f.focusCurrent()
		return f, false, false
	case " ":
		if f.fields[f.focus].Kind == fieldToggle {
			f.fields[f.focus].on = !f.fields[f.focus].on
			return f, false, false
		}
	case "enter":
		// Only the last field submits on Enter, regardless of its kind —
		// a toggle field partway through the form (e.g. migrationModel's
		// Copy/Best-effort/Dry-run before Migrate would otherwise
		// misfire itself) should just move on, same as any text field.
		if f.focus == len(f.fields)-1 {
			return f, true, false
		}
		f.blurCurrent()
		f.focus++
		f.focusCurrent()
		return f, false, false
	case "ctrl+s":
		return f, true, false
	}

	if f.fields[f.focus].Kind == fieldText {
		var cmd tea.Cmd
		f.fields[f.focus].input, cmd = f.fields[f.focus].input.Update(msg)
		_ = cmd // textinput.Blink is the only cmd this ever produces; the form doesn't need to relay it
	}
	return f, false, false
}

func (f *simpleForm) blurCurrent() {
	if f.fields[f.focus].Kind == fieldText {
		f.fields[f.focus].input.Blur()
	}
}

func (f *simpleForm) focusCurrent() {
	if f.fields[f.focus].Kind == fieldText {
		f.fields[f.focus].input.Focus()
	}
}

func (f simpleForm) View() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render(f.title) + "\n\n")
	for i, field := range f.fields {
		labelStyle := styleFieldLabel
		if i == f.focus {
			labelStyle = labelStyle.Foreground(colorAccent).Bold(true)
		}
		b.WriteString(labelStyle.Render(field.Label) + "\n")
		switch field.Kind {
		case fieldToggle:
			mark := "[ ]"
			if field.on {
				mark = "[x]"
			}
			style := lipgloss.NewStyle()
			if i == f.focus {
				style = style.Foreground(colorAccent).Bold(true)
			}
			b.WriteString(style.Render(mark+" "+field.Hint) + "\n\n")
		default:
			box := styleBox
			if i == f.focus {
				box = styleBoxFocused
			}
			b.WriteString(box.Render(field.input.View()) + "\n")
			if field.Hint != "" {
				b.WriteString(styleSubtitle.Render(field.Hint) + "\n")
			}
			b.WriteString("\n")
		}
	}
	if f.errMsg != "" {
		b.WriteString(styleError.Render(f.errMsg) + "\n\n")
	}
	b.WriteString(helpBar("tab", "next field", "enter", "submit", "esc", "cancel"))
	return b.String()
}
