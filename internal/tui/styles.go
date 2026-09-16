package tui

import (
	"strings"

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

	styleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0B0B12")).
			Background(colorAccent).
			Padding(0, 2)

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

	styleMenuItem         = lipgloss.NewStyle().Padding(0, 2).Foreground(colorFg)
	styleMenuItemSelected = lipgloss.NewStyle().Padding(0, 2).
				Bold(true).
				Foreground(lipgloss.Color("#0B0B12")).
				Background(colorAccent)
)

// helpItems renders each key/action pair the way helpBar always has,
// without joining them onto a line — shared by helpBar and helpBarWrap.
func helpItems(pairs ...string) []string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		b = append(b, lipgloss.NewStyle().Foreground(colorAccent).Render(pairs[i])+styleHelp.Render(" "+pairs[i+1]))
	}
	return b
}

// helpBar renders a "key: action" footer from alternating key/action
// pairs, on one line.
func helpBar(pairs ...string) string {
	items := helpItems(pairs...)
	line := ""
	for i, s := range items {
		if i > 0 {
			line += styleHelp.Render("  •  ")
		}
		line += s
	}
	return styleHelp.Render(" ") + line
}

// helpBarWrap is helpBar, wrapped onto as many lines as it takes to keep
// each one within width — for a screen with enough keybindings that a
// single line would run past the terminal's edge.
func helpBarWrap(width int, pairs ...string) string {
	if width <= 0 {
		return helpBar(pairs...)
	}
	items := helpItems(pairs...)
	sep := styleHelp.Render("  •  ")
	sepWidth := lipgloss.Width(sep)
	prefix := styleHelp.Render(" ")
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
