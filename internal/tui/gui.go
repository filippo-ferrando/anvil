package tui

import (
	"context"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// guiRequest is what the GUI app form asks for.
type guiRequest struct {
	command        string
	x11            bool
	user, identity string // identity is VM only
	export         bool   // also add the app to this desktop's launcher
	title, icon    string // launcher name and icon file, with export
}

// guiFields builds the GUI app form; the identity field only shows for a VM.
func guiFields(inst *anvilv1.Instance, last instancesModel) []formField {
	fields := []formField{
		textField("Command", "e.g. ghidraRun", last.lastGUICommand),
		toggleField("X11 app", "Java/Swing and other X11 apps need XWayland", last.lastGUIX11),
		textField("As user (optional)", "VM: SSH user, container: docker exec --user", ""),
	}
	if inst.GetVm() != nil {
		fields = append(fields, identityField(last.lastIdentity))
	}
	return append(fields,
		toggleField("Add to desktop", "also add it to this desktop's app launcher", false),
		textField("Launcher title (optional)", "with Add to desktop: name in the launcher", ""),
		pathField("Launcher icon (optional)", "with Add to desktop: icon file on this machine", ""),
	)
}

func guiRequestFrom(form simpleForm) guiRequest {
	return guiRequest{
		command:  form.Value("Command"),
		x11:      form.Bool("X11 app"),
		user:     form.Value("As user (optional)"),
		identity: form.Value(identityLabel),
		export:   form.Bool("Add to desktop"),
		title:    form.Value("Launcher title (optional)"),
		icon:     form.Value("Launcher icon (optional)"),
	}
}

// guiArgs builds the `anvil gui --detach` call for name, which runs in its own session.
func guiArgs(socketPath, remote, name string, g guiRequest) []string {
	opts := []string{"gui", "--detach"}
	if g.identity != "" {
		opts = append(opts, "--identity", g.identity)
	}
	return guiCLIArgs(socketPath, remote, opts, name, g)
}

// guiExportArgs builds the `anvil gui export` call that adds the app to this desktop's launcher.
func guiExportArgs(socketPath, remote, name string, g guiRequest) []string {
	opts := []string{"gui", "export"}
	if g.title != "" {
		opts = append(opts, "--title", g.title)
	}
	if g.icon != "" {
		opts = append(opts, "--icon", g.icon)
	}
	return guiCLIArgs(socketPath, remote, opts, name, g)
}

func guiCLIArgs(socketPath, remote string, sub []string, name string, g guiRequest) []string {
	var args []string
	if socketPath != "" {
		args = append(args, "--socket", socketPath)
	}
	if remote != "" {
		args = append(args, "--remote", remote)
	}
	args = append(args, sub...)
	if g.x11 {
		args = append(args, "--x11")
	}
	if g.user != "" {
		args = append(args, "--user", g.user)
	}
	args = append(args, name, "--")
	return append(args, strings.Fields(g.command)...)
}

// appRequest is what the Ephemeral screen's GUI app form asks for.
type appRequest struct {
	distro, pkg, command string
	x11, export          bool
	cpus, memory         int64
}

// ephemeralAppArgs builds `anvil gui --detach --ephemeral` (sub "gui --detach") or
// `anvil gui export --ephemeral` (sub "gui export") for a.
func ephemeralAppArgs(socketPath string, sub []string, a appRequest) []string {
	var args []string
	if socketPath != "" {
		args = append(args, "--socket", socketPath)
	}
	args = append(append(args, sub...), "--ephemeral")
	if a.x11 {
		args = append(args, "--x11")
	}
	if a.cpus > 0 {
		args = append(args, "--cpus", strconv.FormatInt(a.cpus, 10))
	}
	if a.memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(a.memory, 10))
	}
	args = append(args, a.distro, a.pkg)
	if cmd := strings.Fields(a.command); len(cmd) > 0 {
		args = append(append(args, "--"), cmd...)
	}
	return args
}

// launchEphemeralApp starts a's app in a throwaway clone through the anvil CLI, after
// `anvil gui export --ephemeral` with export, following its progress until ctx ends.
func launchEphemeralApp(ctx context.Context, socketPath string, a appRequest, wrap func(lineStep) tea.Msg) tea.Cmd {
	calls := [][]string{ephemeralAppArgs(socketPath, []string{"gui", "--detach"}, a)}
	if a.export {
		calls = append([][]string{ephemeralAppArgs(socketPath, []string{"gui", "export"}, a)}, calls...)
	}
	return followGUICalls(ctx, calls, wrap)
}

// launchGUIApp starts the app through the anvil CLI, which keeps running after the TUI quits,
// following its progress until ctx ends. With export it first runs `anvil gui export`.
func launchGUIApp(ctx context.Context, socketPath, remote, name string, g guiRequest, wrap func(lineStep) tea.Msg) tea.Cmd {
	calls := [][]string{guiArgs(socketPath, remote, name, g)}
	if g.export {
		calls = append([][]string{guiExportArgs(socketPath, remote, name, g)}, calls...)
	}
	return followGUICalls(ctx, calls, wrap)
}

// guiTask starts g's app in instance name as a task.
func (m *model) guiTask(name string, g guiRequest) (*task, tea.Cmd) {
	what := g.command
	if f := strings.Fields(g.command); len(f) > 0 {
		what = f[0]
	}
	t, cmd := m.streamTask("starting "+what+" in "+name, "started "+what+" in "+name, screenInstances, func(ctx context.Context, id int) tea.Cmd {
		return launchGUIApp(ctx, m.socketPath, m.client.Remote, name, g, lineTaskStep(id))
	})
	t.cancelNote = "Stops following the launch. The app keeps starting in " + name + "."
	return t, cmd
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
