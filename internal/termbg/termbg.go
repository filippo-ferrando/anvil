// Package termbg stops bubbletea's init from asking the terminal for its background
// color, which costs every command 5 seconds on a terminal that never answers.
package termbg

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Runs before bubbletea's init: this import path sorts first and needs only lipgloss.
// The TUI asks for the real value with Detect.
func init() { lipgloss.SetHasDarkBackground(true) }

// Detect asks the terminal for its background color, before a Bubble Tea program takes it.
func Detect() { lipgloss.SetHasDarkBackground(termenv.NewOutput(os.Stdout).HasDarkBackground()) }
