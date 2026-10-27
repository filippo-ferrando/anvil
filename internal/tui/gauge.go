package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// gauge renders a labeled percentage bar, e.g. "CPU  ████████░░░░░░░░░░░░  42.3%",
// colored green/yellow/red by how full it is.
func gauge(label string, percent float64, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}
	color := colorGood
	switch {
	case percent >= 90:
		color = colorBad
	case percent >= 70:
		color = colorWarn
	}
	bar := lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		lipgloss.NewStyle().Foreground(colorAccentDim).Render(strings.Repeat("░", width-filled))
	return fmt.Sprintf("%-5s%s %5.1f%%", label, bar, percent)
}

// humanRate renders a bytes/sec value as a short human-readable rate.
func humanRate(bytesPerSec float64) string {
	return progress.Bytes(int64(bytesPerSec)) + "/s"
}
