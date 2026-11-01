// Package progress formats the daemon's status lines for display, shared by the CLI and
// the TUI so both show the same bar and the same signs of life.
package progress

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const barWidth = 20

var (
	percent = regexp.MustCompile(`\b(\d{1,3})%`)
	spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
)

// Key is the part of status before the first ": ". Updates sharing a key redraw one line.
func Key(status string) string {
	if k, _, ok := strings.Cut(status, ": "); ok {
		return k
	}
	return status
}

// OneLine joins the lines of status with " ↵ ", since a status line drawn over itself
// must stay one line, and a multi-line `run` script would break that.
func OneLine(status string) string {
	if !strings.ContainsAny(status, "\r\n") {
		return status
	}
	var parts []string
	for _, l := range strings.FieldsFunc(status, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, " ↵ ")
}

// Bar puts a progress bar in front of the first percentage in status, if it has one.
func Bar(status string) string {
	loc := percent.FindStringSubmatchIndex(status)
	if loc == nil {
		return status
	}
	pct, _ := strconv.Atoi(status[loc[2]:loc[3]])
	filled := min(pct, 100) * barWidth / 100
	bar := "[" + strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled) + "] "
	return status[:loc[0]] + bar + status[loc[0]:]
}

// Live is Bar for the step still running: after a second without news it also shows a
// spinner frame and the time spent on the step, so a long wait still looks alive.
func Live(status string, elapsed time.Duration, frame int) string {
	s := Bar(OneLine(status))
	if elapsed < time.Second {
		return s
	}
	return fmt.Sprintf("%s  %c %s", s, spinner[frame%len(spinner)], elapsed.Truncate(time.Second))
}

// Done is how a finished step stays on screen: its bar, and how long it took when that
// was long enough to notice.
func Done(status string, elapsed time.Duration) string {
	s := Bar(OneLine(status))
	if elapsed < time.Second {
		return s
	}
	return fmt.Sprintf("%s  (%s)", s, elapsed.Truncate(time.Second))
}

// Bytes renders a byte count like "512.0 MiB".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
