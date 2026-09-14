package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
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
	return humanBytesTUI(int64(bytesPerSec)) + "/s"
}

// humanDuration renders a second count as a compact "1d 2h 3m"-shaped uptime.
func humanDuration(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	d := time.Duration(seconds) * time.Second
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	secs := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}
