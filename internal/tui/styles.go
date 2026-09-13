package tui

import "github.com/charmbracelet/lipgloss"

// One shared palette + a handful of reusable styles, so every screen looks
// like part of the same application instead of six ad hoc layouts. Kept
// deliberately simple (a single accent color, one muted color for
// secondary text) rather than a full theme system — there's only one
// theme.
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

// helpBar renders a consistent "key: action" footer, the same shape on
// every screen, so navigation reads as one language instead of each view
// inventing its own — directly the thing that was missing before.
func helpBar(pairs ...string) string {
	var b []string
	for i := 0; i+1 < len(pairs); i += 2 {
		b = append(b, lipgloss.NewStyle().Foreground(colorAccent).Render(pairs[i])+styleHelp.Render(" "+pairs[i+1]))
	}
	line := ""
	for i, s := range b {
		if i > 0 {
			line += styleHelp.Render("  •  ")
		}
		line += s
	}
	return styleHelp.Render(" ") + line
}
