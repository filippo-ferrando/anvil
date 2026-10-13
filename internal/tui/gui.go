package tui

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// guiLaunchedMsg reports how starting a detached `anvil gui` went.
type guiLaunchedMsg struct {
	output string
	err    error
}

// guiArgs builds the `anvil gui --detach` call for name, which runs in its own session.
func guiArgs(socketPath, remote, name, command string, x11 bool) []string {
	var args []string
	if socketPath != "" {
		args = append(args, "--socket", socketPath)
	}
	if remote != "" {
		args = append(args, "--remote", remote)
	}
	args = append(args, "gui", "--detach")
	if x11 {
		args = append(args, "--x11")
	}
	args = append(args, name, "--")
	return append(args, strings.Fields(command)...)
}

// launchGUIApp starts the app through the anvil CLI, which keeps running after the TUI quits.
func launchGUIApp(socketPath, remote, name, command string, x11 bool) tea.Cmd {
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			return guiLaunchedMsg{err: err}
		}
		out, err := exec.Command(exe, guiArgs(socketPath, remote, name, command, x11)...).CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err != nil {
			if text != "" {
				err = errors.New(strings.TrimPrefix(text, "anvil: "))
			}
			return guiLaunchedMsg{err: err}
		}
		return guiLaunchedMsg{output: text}
	}
}
