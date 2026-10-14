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
	return guiCLIArgs(socketPath, remote, []string{"gui", "--detach"}, name, command, x11)
}

// guiExportArgs builds the `anvil gui export` call that adds the app to this desktop's launcher.
func guiExportArgs(socketPath, remote, name, command string, x11 bool) []string {
	return guiCLIArgs(socketPath, remote, []string{"gui", "export"}, name, command, x11)
}

func guiCLIArgs(socketPath, remote string, sub []string, name, command string, x11 bool) []string {
	var args []string
	if socketPath != "" {
		args = append(args, "--socket", socketPath)
	}
	if remote != "" {
		args = append(args, "--remote", remote)
	}
	args = append(args, sub...)
	if x11 {
		args = append(args, "--x11")
	}
	args = append(args, name, "--")
	return append(args, strings.Fields(command)...)
}

// launchGUIApp starts the app through the anvil CLI, which keeps running after the TUI quits.
// With export it first adds the app to the desktop's launcher, the same as `anvil gui export`.
func launchGUIApp(socketPath, remote, name, command string, x11, export bool) tea.Cmd {
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			return guiLaunchedMsg{err: err}
		}
		var lines []string
		calls := [][]string{guiArgs(socketPath, remote, name, command, x11)}
		if export {
			calls = append([][]string{guiExportArgs(socketPath, remote, name, command, x11)}, calls...)
		}
		for _, args := range calls {
			out, err := exec.Command(exe, args...).CombinedOutput()
			text := strings.TrimSpace(string(out))
			if err != nil {
				if text != "" {
					err = errors.New(strings.TrimPrefix(text, "anvil: "))
				}
				return guiLaunchedMsg{err: err}
			}
			if text != "" {
				lines = append(lines, text)
			}
		}
		return guiLaunchedMsg{output: strings.Join(lines, "; ")}
	}
}
