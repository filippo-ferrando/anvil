package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
)

// appendProgressLine appends status to lines, replacing the last line instead
// of adding a new one when it shares the same progress.Key.
func appendProgressLine(lines []string, status string) []string {
	if len(lines) > 0 && progress.Key(lines[len(lines)-1]) == progress.Key(status) {
		lines[len(lines)-1] = status
		return lines
	}
	return append(lines, status)
}

// stepStart returns since when status continues the last line's step, else now: the start
// of the step that progressView times. Call it before appendProgressLine.
func stepStart(lines []string, status string, since time.Time) time.Time {
	if len(lines) > 0 && progress.Key(lines[len(lines)-1]) == progress.Key(status) {
		return since
	}
	return time.Now()
}

// progressView renders a transcript with a bar on every percentage. While running, the last
// line also shows a spinner and its step's time, redrawn by the one-second tick.
func progressView(lines []string, running bool, since time.Time) string {
	var b strings.Builder
	for i, line := range lines {
		if running && i == len(lines)-1 {
			elapsed := time.Since(since)
			line = progress.Live(line, elapsed, int(elapsed.Seconds()))
		} else {
			line = progress.Bar(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// The launch form's env/volumes/ports fields are each one comma-separated text field.

func parseInt32(s, field string) (int32, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return int32(n), nil
}

func parseInt64(s, field string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return n, nil
}

func splitCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// humanBytesTUI renders n as a short, human-readable size (KiB/MiB/...).
func humanBytesTUI(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
