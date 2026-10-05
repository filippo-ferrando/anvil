package tui

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
)

// One shared color palette and set of reusable styles for every screen.
var (
	colorAccent    = lipgloss.Color("#7C6FF0") // headers, focused borders, primary actions
	colorAccentDim = lipgloss.Color("#4A4370")
	colorMuted     = lipgloss.Color("#6B7280") // help text, secondary detail
	colorGood      = lipgloss.Color("#4ADE80")
	colorBad       = lipgloss.Color("#F87171")
	colorWarn      = lipgloss.Color("#FBBF24")
	colorFg        = lipgloss.Color("#E5E7EB")
	colorDark      = lipgloss.Color("#0B0B12") // text on a colored background
	colorKeyBg     = lipgloss.Color("#2E2A4F") // background of a key in the shortcut legend

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorDark).
			Background(colorAccent).
			Padding(0, 1)

	styleSubtitle = lipgloss.NewStyle().Foreground(colorMuted)

	styleHelp = lipgloss.NewStyle().Foreground(colorMuted).Padding(0, 1)

	styleError = lipgloss.NewStyle().Foreground(colorBad).Bold(true)
	styleGood  = lipgloss.NewStyle().Foreground(colorGood)
	styleWarn  = lipgloss.NewStyle().Foreground(colorWarn)

	styleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorAccentDim).
			Padding(0, 1)

	styleBoxFocused = styleBox.BorderForeground(colorAccent)

	styleFieldLabel = lipgloss.NewStyle().Foreground(colorMuted)

	styleMenuItem         = lipgloss.NewStyle().Padding(0, 1).Foreground(colorFg)
	styleMenuItemSelected = lipgloss.NewStyle().Padding(0, 1).
				Bold(true).
				Foreground(lipgloss.Color("#0B0B12")).
				Background(colorAccent)
)

var (
	styleKey       = lipgloss.NewStyle().Bold(true).Foreground(colorFg).Background(colorKeyBg).Padding(0, 1)
	styleKeyAction = lipgloss.NewStyle().Foreground(colorMuted)
	helpSep        = "   "
)

// newList is a bubbles list themed with the shared palette, without its own help footer.
func newList() list.Model {
	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Foreground(colorAccent).BorderForeground(colorAccent)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Foreground(colorFg).BorderForeground(colorAccent)
	d.Styles.NormalTitle = d.Styles.NormalTitle.Foreground(colorFg)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Foreground(colorMuted)
	l := list.New(nil, d, 0, 0)
	l.Styles.Title = styleTitle
	l.SetShowHelp(false)
	return l
}

// helpItems renders each key/action pair as a key chip followed by its action.
func helpItems(pairs ...string) []string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		b = append(b, styleKey.Render(pairs[i])+" "+styleKeyAction.Render(pairs[i+1]))
	}
	return b
}

// helpBar renders a shortcut legend from alternating key/action pairs, on one line.
func helpBar(pairs ...string) string {
	return " " + strings.Join(helpItems(pairs...), helpSep)
}

// helpBarWrap is helpBar, wrapped onto as many lines as it takes to keep
// each one within width, for a screen with enough keybindings that a single line would run past the terminal's edge.
func helpBarWrap(width int, pairs ...string) string {
	if width <= 0 {
		return helpBar(pairs...)
	}
	items := helpItems(pairs...)
	sep := helpSep
	sepWidth := lipgloss.Width(sep)
	prefix := " "
	prefixWidth := lipgloss.Width(prefix)

	var lines []string
	line, lineWidth := "", prefixWidth
	for _, item := range items {
		itemWidth := lipgloss.Width(item)
		add := itemWidth
		if line != "" {
			add += sepWidth
		}
		if line != "" && lineWidth+add > width {
			lines = append(lines, prefix+line)
			line, lineWidth = "", prefixWidth
		}
		if line != "" {
			line += sep
			lineWidth += sepWidth
		}
		line += item
		lineWidth += itemWidth
	}
	if line != "" {
		lines = append(lines, prefix+line)
	}
	return strings.Join(lines, "\n")
}

// rpcPrefix matches the "rpc error: code = X desc = " noise gRPC puts in front of a daemon error.
var rpcPrefix = regexp.MustCompile(`rpc error: code = (\w+) desc = `)

// friendlyError strips gRPC framing from an error text and returns a hint for the common causes.
func friendlyError(text string) (msg, hint string) {
	if m := rpcPrefix.FindStringSubmatch(text); m != nil {
		switch m[1] {
		case "Unavailable":
			hint = "anvild is not reachable: check that it is running and the socket path is right"
		case "PermissionDenied":
			hint = "access denied: the user must be in the anvil group"
		case "DeadlineExceeded":
			hint = "the daemon took too long to answer, try again"
		case "NotFound":
			hint = "it may have been removed: press r to refresh"
		case "AlreadyExists":
			hint = "pick another name"
		}
	}
	return rpcPrefix.ReplaceAllString(text, ""), hint
}

// errLine renders err as a red line for a progress transcript, with a hint line when one applies.
func errLine(err error) string {
	msg, hint := friendlyError(err.Error())
	s := styleError.Render("✕ " + msg)
	if hint != "" {
		s += "\n" + styleSubtitle.Render("  "+hint)
	}
	return s
}
