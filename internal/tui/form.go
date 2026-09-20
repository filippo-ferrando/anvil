package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// simpleForm is a shared form component: Tab/Shift+Tab between fields, Enter or a submit key to confirm, Esc to
// cancel. A text field can also list Suggestions, which Tab cycles through bash-style instead of moving focus.
type simpleForm struct {
	title     string
	fields    []formField
	focus     int
	errMsg    string
	maxHeight int // 0 means unconstrained; set via SetHeight

	// Tab-completion cycling state: tabField is the field mid-cycle (-1 when idle), tabPrefix is what the user
	// typed before the first Tab, and tabIdx is the current match index, so a second Tab advances instead of re-filtering.
	tabField  int
	tabPrefix string
	tabIdx    int
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

	input       textinput.Model // fieldText
	on          bool            // fieldToggle
	Suggestions []string        // candidate values Tab can cycle through; nil disables it

	// PathComplete makes Tab cycle through this field's own directory's
	// entries instead of a fixed Suggestions list; see completePathMatches.
	PathComplete bool
}

// textField and toggleField construct a formField of each kind.
func textField(label, hint, value string) formField {
	ti := textinput.New()
	ti.Placeholder = hint
	ti.SetValue(value)
	ti.CharLimit = 256
	return formField{Label: label, Hint: hint, Kind: fieldText, input: ti}
}

// pathField is a textField whose Tab-completion lists real filesystem
// entries under whatever directory is currently typed, shell-style.
func pathField(label, hint, value string) formField {
	f := textField(label, hint, value)
	f.PathComplete = true
	return f
}

func toggleField(label, hint string, on bool) formField {
	return formField{Label: label, Hint: hint, Kind: fieldToggle, on: on}
}

// completePathMatches lists dir's entries whose name starts with base (prefix's last path segment), shell-style:
// sorted, directories suffixed with "/", dotfiles hidden unless base starts with a dot, resolved against the process's cwd.
func completePathMatches(prefix string) []string {
	dir, base := filepath.Split(prefix)
	readDir := dir
	if readDir == "" {
		readDir = "."
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base) {
			continue
		}
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".") {
			continue
		}
		if e.IsDir() {
			name += "/"
		}
		out = append(out, dir+name)
	}
	sort.Strings(out)
	return out
}

func newSimpleForm(title string, fields []formField) simpleForm {
	f := simpleForm{title: title, fields: fields, tabField: -1}
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

	// Any key other than Tab ends a completion cycle; the next Tab starts a fresh one from the field's current content.
	if keyMsg.String() != "tab" {
		f.tabField = -1
	}

	switch keyMsg.String() {
	case "esc":
		return f, false, true
	case "tab":
		if f.completeCurrent() {
			return f, false, false
		}
		fallthrough
	case "down":
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

// completeCurrent tab-cycles the focused field through its Suggestions matching what the user typed
// (case-insensitive), bash-menu-complete style. Returns false when there's nothing to complete, so the caller falls back to moving focus.
func (f *simpleForm) completeCurrent() bool {
	field := &f.fields[f.focus]
	if field.Kind != fieldText || (len(field.Suggestions) == 0 && !field.PathComplete) {
		return false
	}

	prefix := field.input.Value()
	continuing := f.tabField == f.focus
	if continuing {
		prefix = f.tabPrefix
	}
	matches := matchesFor(field, prefix)
	if len(matches) == 0 {
		return false
	}

	idx := 0
	if continuing {
		idx = (f.tabIdx + 1) % len(matches)
	}
	field.input.SetValue(matches[idx])
	field.input.CursorEnd()
	f.tabField, f.tabPrefix, f.tabIdx = f.focus, prefix, idx
	return true
}

// matchesFor returns field's current candidate list for prefix: real
// filesystem entries for a PathComplete field, its fixed Suggestions otherwise.
func matchesFor(field *formField, prefix string) []string {
	if field.PathComplete {
		return completePathMatches(prefix)
	}
	return filterSuggestions(field.Suggestions, prefix)
}

// filterSuggestions returns options containing query, case-insensitively. An empty query matches everything,
// so Tab on a blank field cycles through the full candidate list.
func filterSuggestions(options []string, query string) []string {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []string
	for _, s := range options {
		if q == "" || strings.Contains(strings.ToLower(s), q) {
			out = append(out, s)
		}
	}
	return out
}

// suggestionHint renders the focused field's matching Suggestions as a short "→ a, b, c  (tab to cycle)" line,
// so the feature is discoverable without already knowing it exists.
func suggestionHint(field formField) string {
	matches := matchesFor(&field, field.input.Value())
	if len(matches) == 0 {
		return ""
	}
	shown := matches
	suffix := "  (tab to cycle)"
	const maxShown = 5
	if len(shown) > maxShown {
		suffix = fmt.Sprintf("  +%d more (tab to cycle)", len(shown)-maxShown)
		shown = shown[:maxShown]
	}
	return "→ " + strings.Join(shown, ", ") + suffix
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
			if focused {
				if hint := suggestionHint(field); hint != "" {
					lines = append(lines, "  "+styleSubtitle.Render(hint))
				}
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

	return strings.Join(content, "\n") + "\n\n" + helpBar("tab", "next field / complete", "enter", "submit", "esc", "cancel")
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
