package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// simpleForm is a shared form component: Tab/Shift+Tab between fields,
// Enter or a submit key to confirm, Esc to cancel.
type simpleForm struct {
	title     string
	fields    []formField
	focus     int
	errMsg    string
	maxHeight int // 0 means unconstrained; set via SetHeight
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

// textField and toggleField construct a formField of each kind.
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

// SetHeight constrains View() to at most h lines of field content. 0 means unconstrained.
func (f *simpleForm) SetHeight(h int) { f.maxHeight = h }

// Value returns the current text value of the field with the given label.
func (f simpleForm) Value(label string) string {
	for _, field := range f.fields {
		if field.Label == label {
			return strings.TrimSpace(field.input.Value())
		}
	}
	return ""
}

// Bool returns the current value of the toggle field with the given label.
func (f simpleForm) Bool(label string) bool {
	for _, field := range f.fields {
		if field.Label == label {
			return field.on
		}
	}
	return false
}

// update handles the form's keys and returns (updated form, submitted, cancelled).
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
		// Only the last field submits on Enter; others just advance focus.
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
		_ = cmd // the form doesn't need to relay textinput's blink cmd
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
	var lines []string
	lines = append(lines, styleTitle.Render(f.title), "")
	focusLine := 0

	for i, field := range f.fields {
		focused := i == f.focus
		labelStyle := styleFieldLabel
		bar := "  "
		if focused {
			labelStyle = labelStyle.Foreground(colorAccent).Bold(true)
			bar = lipgloss.NewStyle().Foreground(colorAccent).Render("┃ ")
			focusLine = len(lines)
		}
		lines = append(lines, labelStyle.Render(field.Label))

		switch field.Kind {
		case fieldToggle:
			mark := "[ ]"
			if field.on {
				mark = "[x]"
			}
			style := lipgloss.NewStyle()
			if focused {
				style = style.Foreground(colorAccent).Bold(true)
			}
			lines = append(lines, bar+style.Render(mark+" "+field.Hint))
		default:
			lines = append(lines, bar+field.input.View())
			if focused && field.Hint != "" {
				lines = append(lines, "  "+styleSubtitle.Render(field.Hint))
			}
		}
	}

	if f.errMsg != "" {
		lines = append(lines, "", styleError.Render(f.errMsg))
	}

	content := lines
	if f.maxHeight > 0 && len(lines) > f.maxHeight {
		content = windowLines(lines, focusLine, f.maxHeight)
	}

	return strings.Join(content, "\n") + "\n\n" + helpBar("tab", "next field", "enter", "submit", "esc", "cancel")
}

// windowLines returns at most maxHeight consecutive lines centered on focusLine.
func windowLines(lines []string, focusLine, maxHeight int) []string {
	if maxHeight < 1 {
		maxHeight = 1
	}
	start := focusLine - maxHeight/2
	if start < 0 {
		start = 0
	}
	end := start + maxHeight
	if end > len(lines) {
		end = len(lines)
		start = end - maxHeight
		if start < 0 {
			start = 0
		}
	}
	windowed := append([]string(nil), lines[start:end]...)
	if start > 0 && len(windowed) > 0 {
		windowed[0] = styleSubtitle.Render("↑ more above")
	}
	if end < len(lines) && len(windowed) > 0 {
		windowed[len(windowed)-1] = styleSubtitle.Render("↓ more below")
	}
	return windowed
}
