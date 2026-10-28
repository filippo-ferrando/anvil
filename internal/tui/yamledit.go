package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"git.fferrando.cc/rdfilippo/anvil/internal/intent/apply"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/vars"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/cloudinit"
	"git.fferrando.cc/rdfilippo/anvil/internal/yamlcheck"
)

// Token classes the YAML highlighter gives each rune.
const (
	tokPlain = iota
	tokKey
	tokComment
	tokString
	tokConst // numbers, booleans, null, anchors and tags
	tokPunct
	tokVar
	tokHeader
)

var tokStyles = []lipgloss.Style{
	tokPlain:   lipgloss.NewStyle().Foreground(colorFg),
	tokKey:     lipgloss.NewStyle().Foreground(lipgloss.Color("#93C5FD")),
	tokComment: lipgloss.NewStyle().Foreground(colorMuted).Italic(true),
	tokString:  lipgloss.NewStyle().Foreground(lipgloss.Color("#A3E635")),
	tokConst:   lipgloss.NewStyle().Foreground(lipgloss.Color("#FDBA74")),
	tokPunct:   lipgloss.NewStyle().Foreground(colorAccent),
	tokVar:     lipgloss.NewStyle().Foreground(lipgloss.Color("#F0ABFC")).Bold(true),
	tokHeader:  lipgloss.NewStyle().Foreground(colorAccent).Bold(true),
}

// gutterWidth matches the textarea's own prompt and line numbers, so pages size it as before.
const gutterWidth = 6

// yamlEditor is a textarea drawn with YAML highlighting, marking the lines check finds problems on.
type yamlEditor struct {
	textarea.Model
	check    func(string) []yamlcheck.Problem
	okText   string // shown under the text when check finds nothing
	problems []yamlcheck.Problem
	top      int // first display row shown
}

func newYAMLEditor(placeholder, okText string, check func(string) []yamlcheck.Problem) yamlEditor {
	ta := textarea.New()
	ta.Placeholder = placeholder
	ta.MaxHeight = 0 // the default 99-line cap cuts longer documents on SetValue
	return yamlEditor{Model: ta, check: check, okText: okText}
}

func checkCloudInit(s string) []yamlcheck.Problem {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return cloudinit.Validate(s)
}

// checkManifest fills variables the way applying from the TUI does: shell env, then .env in the current dir.
func checkManifest(s string) []yamlcheck.Problem {
	values, err := vars.Load("", "", []byte(s), nil)
	if err != nil {
		return []yamlcheck.Problem{{Message: err.Error()}}
	}
	return apply.Check([]byte(s), values)
}

// SetValue starts a document taller than the view at its top; a short one keeps the cursor at the end, to type on.
func (e *yamlEditor) SetValue(s string) {
	e.Model.SetValue(s)
	if e.LineCount() > e.textHeight() {
		for e.Line() > 0 {
			e.CursorUp()
		}
		e.CursorStart()
	}
	e.top = 0
	e.revalidate()
}

func (e *yamlEditor) revalidate() {
	e.problems = nil
	if e.check != nil {
		e.problems = e.check(e.Value())
	}
}

func (e yamlEditor) Update(msg tea.Msg) (yamlEditor, tea.Cmd) {
	before := e.Value()
	var cmd tea.Cmd
	e.Model, cmd = e.Model.Update(msg)
	if e.Value() != before {
		e.revalidate()
	}
	e.top = e.scrollTop()
	return e, cmd
}

func (e yamlEditor) textHeight() int { return max(1, e.Height()-1) } // last row: the check result

// cursorRow is the cursor's display row, counting wrapped rows.
func (e yamlEditor) cursorRow() int {
	w, row := max(1, e.Width()), 0
	for i, line := range strings.Split(e.Value(), "\n") {
		if i == e.Line() {
			return row + e.cursorCol()/w
		}
		row += rowsFor(len([]rune(line)), w)
	}
	return row
}

func (e yamlEditor) cursorCol() int {
	li := e.LineInfo()
	return li.StartColumn + li.ColumnOffset
}

// scrollTop moves the window just enough to keep the cursor visible.
func (e yamlEditor) scrollTop() int {
	row, h := e.cursorRow(), e.textHeight()
	top := min(e.top, row)
	if row >= top+h {
		top = row - h + 1
	}
	return max(0, top)
}

func rowsFor(n, width int) int { return max(1, (n+width-1)/width) }

func (e yamlEditor) View() string {
	w, h := max(1, e.Width()), e.textHeight()
	marks := map[int]bool{} // line -> it only has warnings
	for _, p := range e.problems {
		if p.Line > 0 {
			warnOnly, seen := marks[p.Line]
			marks[p.Line] = p.Warning && (!seen || warnOnly)
		}
	}

	var rows []string
	if e.Value() == "" && !e.Focused() {
		rows = append(rows, strings.Repeat(" ", gutterWidth)+styleSubtitle.Render(e.Placeholder))
	} else {
		lines := strings.Split(e.Value(), "\n")
		classes := highlightYAML(lines)
		for i, line := range lines {
			rows = append(rows, e.renderLine(i, []rune(line), classes[i], w, marks)...)
		}
	}

	top := e.scrollTop()
	rows = rows[min(top, len(rows)):]
	if len(rows) > h {
		rows = rows[:h]
	}
	for len(rows) < h {
		rows = append(rows, styleSubtitle.Render("~"))
	}
	return strings.Join(append(rows, e.statusLine()), "\n")
}

// renderLine draws one logical line as one or more wrapped display rows.
func (e yamlEditor) renderLine(i int, runes []rune, classes []int, w int, marks map[int]bool) []string {
	cursor := -1
	if e.Focused() && i == e.Line() {
		cursor = e.cursorCol()
	}
	marker, numStyle := styleSubtitle.Render("┃"), styleSubtitle
	if warn, ok := marks[i+1]; ok {
		marker = styleError.Render("✕")
		if warn {
			marker = styleWarn.Render("!")
		}
	}
	if cursor >= 0 {
		numStyle = lipgloss.NewStyle().Foreground(colorAccent)
	}

	n := rowsFor(len(runes), w)
	if cursor == len(runes) && cursor > 0 && cursor%w == 0 {
		n++ // the cursor sits after a full row
	}
	out := make([]string, 0, n)
	for r := 0; r < n; r++ {
		gutter := marker + " " + numStyle.Render(fmt.Sprintf("%3d ", i+1))
		if r > 0 {
			gutter = marker + strings.Repeat(" ", gutterWidth-1)
		}
		var b strings.Builder
		b.WriteString(gutter)
		from, to := r*w, min((r+1)*w, len(runes))
		for j := from; j < to; {
			if j == cursor {
				b.WriteString(tokStyles[classes[j]].Reverse(true).Render(string(runes[j])))
				j++
				continue
			}
			k := j + 1
			for k < to && classes[k] == classes[j] && k != cursor {
				k++
			}
			b.WriteString(tokStyles[classes[j]].Render(string(runes[j:k])))
			j = k
		}
		if cursor == len(runes) && cursor >= from && cursor < from+w {
			b.WriteString(lipgloss.NewStyle().Reverse(true).Render(" "))
		}
		out = append(out, b.String())
	}
	return out
}

// statusLine shows the problem on the cursor's line, else the first one, else okText.
func (e yamlEditor) statusLine() string {
	if len(e.problems) == 0 {
		if e.check == nil || strings.TrimSpace(e.Value()) == "" {
			return ""
		}
		return styleGood.Render("✓ " + e.okText)
	}
	p := e.problems[0]
	for _, q := range e.problems {
		if q.Line == e.Line()+1 {
			p = q
			break
		}
	}
	text := p.Message
	if p.Line > 0 {
		text = fmt.Sprintf("line %d: %s", p.Line, text)
	}
	if more := len(e.problems) - 1; more > 0 {
		text += fmt.Sprintf(" (+%d more)", more)
	}
	style, icon := styleError, "✕ "
	if p.Warning {
		style, icon = styleWarn, "! "
	}
	return style.MaxWidth(e.Width() + gutterWidth).Render(icon + strings.Join(strings.Fields(text), " "))
}

// highlightYAML gives each rune of each line a token class. It is a line lexer,
// not a parser: block scalars (| and >) are the only state carried across lines.
func highlightYAML(lines []string) [][]int {
	out := make([][]int, len(lines))
	block := -1 // indent of the key that opened a block scalar, -1 outside one
	for i, line := range lines {
		r := []rune(line)
		c := make([]int, len(r))
		out[i] = c
		ind := indentOf(r)
		if block >= 0 && (ind == len(r) || ind > block) {
			fill(c, 0, len(r), tokString)
			markVars(r, c)
			continue
		}
		block = -1

		p := ind
		if p < len(r) && r[p] == '#' {
			cls := tokComment
			if i == 0 && strings.HasPrefix(line, "#cloud-config") {
				cls = tokHeader
			}
			fill(c, p, len(r), cls)
			continue
		}
		if s := strings.TrimRight(line, " "); s == "---" || s == "..." {
			fill(c, 0, len(r), tokPunct)
			continue
		}
		keyCol := p
		for p < len(r) && r[p] == '-' && (p+1 == len(r) || r[p+1] == ' ') {
			c[p] = tokPunct
			p = skipSpaces(r, p+1)
			keyCol = p
		}
		if end := keyEnd(r, p); end > 0 {
			fill(c, p, end, tokKey)
			c[end] = tokPunct
			p = skipSpaces(r, end+1)
		}
		if lexValue(r, c, p) {
			block = keyCol
		}
		markVars(r, c)
	}
	return out
}

// keyEnd returns the index of the ':' ending a mapping key that starts at p, or -1.
func keyEnd(r []rune, p int) int {
	if p >= len(r) || strings.ContainsRune("[{#|>!&*", r[p]) {
		return -1
	}
	if r[p] == '"' || r[p] == '\'' {
		q := closeQuote(r, p)
		if q+1 < len(r) && r[q+1] == ':' && (q+2 == len(r) || r[q+2] == ' ') {
			return q + 1
		}
		return -1
	}
	for j := p; j < len(r); j++ {
		if r[j] == '#' && j > 0 && r[j-1] == ' ' {
			return -1
		}
		if r[j] == ':' && (j+1 == len(r) || r[j+1] == ' ') {
			return j
		}
	}
	return -1
}

// lexValue classes a value starting at p and reports whether it opens a block scalar.
func lexValue(r []rune, c []int, p int) bool {
	if p >= len(r) {
		return false
	}
	switch r[p] {
	case '|', '>':
		j := p + 1
		for j < len(r) && strings.ContainsRune("+-0123456789", r[j]) {
			j++
		}
		if j == len(r) || r[j] == ' ' {
			fill(c, p, j, tokPunct)
			lexComment(r, c, j)
			return true
		}
	case '"', '\'':
		q := closeQuote(r, p)
		fill(c, p, q+1, tokString)
		lexComment(r, c, q+1)
		return false
	case '[', '{':
		lexFlow(r, c, p)
		return false
	case '#':
		fill(c, p, len(r), tokComment)
		return false
	}
	end := len(r)
	for j := p; j < len(r); j++ {
		if r[j] == '#' && j > 0 && r[j-1] == ' ' {
			end = j
			break
		}
	}
	word := strings.TrimSpace(string(r[p:end]))
	fill(c, p, end, scalarClass(word))
	fill(c, end, len(r), tokComment)
	return false
}

// lexFlow classes an inline [list] or {map}, through to the end of the line.
func lexFlow(r []rune, c []int, p int) {
	for j := p; j < len(r); {
		switch {
		case strings.ContainsRune("[]{},:", r[j]):
			c[j] = tokPunct
			j++
		case r[j] == '"' || r[j] == '\'':
			q := closeQuote(r, j)
			fill(c, j, q+1, tokString)
			j = q + 1
		case r[j] == '#' && j > 0 && r[j-1] == ' ':
			fill(c, j, len(r), tokComment)
			return
		case r[j] == ' ':
			j++
		default:
			k := j
			for k < len(r) && !strings.ContainsRune("[]{},", r[k]) && !(r[k] == ':' && (k+1 == len(r) || r[k+1] == ' ')) {
				k++
			}
			fill(c, j, k, scalarClass(strings.TrimSpace(string(r[j:k]))))
			j = k
		}
	}
}

func lexComment(r []rune, c []int, p int) {
	for j := p; j < len(r); j++ {
		if r[j] == '#' {
			fill(c, j, len(r), tokComment)
			return
		}
	}
}

func scalarClass(word string) int {
	switch strings.ToLower(word) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return tokConst
	}
	if word == "" {
		return tokPlain
	}
	if strings.ContainsRune("&*!", rune(word[0])) {
		return tokConst
	}
	if _, err := fmt.Sscanf(word, "%g", new(float64)); err == nil && strings.Trim(word, "0123456789.-+eE_") == "" {
		return tokConst
	}
	return tokString
}

// markVars highlights ${VAR} references outside comments.
func markVars(r []rune, c []int) {
	for j := 0; j+1 < len(r); j++ {
		if r[j] != '$' || r[j+1] != '{' || c[j] == tokComment {
			continue
		}
		k := j + 2
		for k < len(r) && r[k] != '}' {
			k++
		}
		if k < len(r) {
			fill(c, j, k+1, tokVar)
			j = k
		}
	}
}

// closeQuote returns the index of the quote closing the one at p, or the last rune.
func closeQuote(r []rune, p int) int {
	for j := p + 1; j < len(r); j++ {
		if r[j] == '\\' && r[p] == '"' {
			j++
			continue
		}
		if r[j] == r[p] {
			return j
		}
	}
	return len(r) - 1
}

func indentOf(r []rune) int { return skipSpaces(r, 0) }

func skipSpaces(r []rune, p int) int {
	for p < len(r) && r[p] == ' ' {
		p++
	}
	return p
}

func fill(c []int, from, to, cls int) {
	for j := max(0, from); j < min(to, len(c)); j++ {
		c[j] = cls
	}
}
