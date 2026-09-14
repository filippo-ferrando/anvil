package tui

import (
	"context"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// maxLogLines bounds how much log text logsModel keeps around during a
// long `-f`-style follow session — unbounded growth for a VM console
// that's been running for hours isn't something a terminal UI needs to
// hold onto, only the CLI's own `anvil logs -f` (which just streams
// straight to stdout, no buffering) does.
const maxLogLines = 2000

// logsModel is the Logs screen (was missing outright) — no
// suspend/resume handoff at all, unlike shell/exec: InstanceService.Logs
// is a plain gRPC server-streaming RPC the daemon answers directly (a
// VM's console/boot output, or a container's stdout/stderr), so this
// streams straight into a scrollable viewport the same way the launch
// and migration screens stream their own progress, with none of the
// real terminal-handoff/query-race risk tea.ExecProcess carries (see
// PLAN.md's notes on the stray-escape-sequence issue that affects
// shell/exec specifically).
type logsModel struct {
	inst      *anvilv1.Instance
	viewport  viewport.Model
	lines     []string
	following bool // auto-scroll to the bottom on new data; turned off once the user scrolls up manually
	streaming bool
}

func newLogsModel(inst *anvilv1.Instance, width, height int) logsModel {
	vp := viewport.New(width, height)
	return logsModel{inst: inst, viewport: vp, following: true, streaming: true}
}

func startLogsStream(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Logs(context.Background(), &anvilv1.LogsRequest{Name: name, Follow: true, TailLines: 200})
		if err != nil {
			return logsStreamMsg{err: err, done: true}
		}
		return receiveLogChunk(stream)()
	}
}

func receiveLogChunk(stream anvilv1.InstanceService_LogsClient) tea.Cmd {
	return func() tea.Msg {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return logsStreamMsg{done: true}
			}
			return logsStreamMsg{err: err, done: true}
		}
		return logsStreamMsg{stream: stream, data: chunk.GetData()}
	}
}

func (m model) updateLogs(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logsStreamMsg:
		if msg.err != nil {
			m.logs.streaming = false
			m.setStatus("logs: "+msg.err.Error(), true)
			return m, nil
		}
		if len(msg.data) > 0 {
			m.logs.appendLines(string(msg.data))
		}
		if msg.done {
			m.logs.streaming = false
			return m, nil
		}
		return m, receiveLogChunk(msg.stream)

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			m.screen = screenInstances
			return m, nil
		case "f":
			m.logs.following = !m.logs.following
			if m.logs.following {
				m.logs.viewport.GotoBottom()
			}
			return m, nil
		}
		wasAtBottom := m.logs.viewport.AtBottom()
		var cmd tea.Cmd
		m.logs.viewport, cmd = m.logs.viewport.Update(msg)
		if !m.logs.viewport.AtBottom() && wasAtBottom {
			m.logs.following = false // the user just scrolled up; stop yanking them back down
		}
		return m, cmd
	}
	return m, nil
}

// appendLines adds text (a raw LogChunk's bytes) to the buffer, capping
// total retained lines at maxLogLines, and re-renders the viewport,
// auto-scrolling to the bottom only while following is on.
func (m *logsModel) appendLines(text string) {
	m.lines = append(m.lines, strings.Split(strings.TrimRight(text, "\n"), "\n")...)
	if len(m.lines) > maxLogLines {
		m.lines = m.lines[len(m.lines)-maxLogLines:]
	}
	m.viewport.SetContent(strings.Join(m.lines, "\n"))
	if m.following {
		m.viewport.GotoBottom()
	}
}

func (m logsModel) View() string {
	title := "Logs: " + m.inst.GetName()
	if m.streaming {
		title += " (following)"
	}
	if !m.following {
		title += " (scrolled back, f to resume following)"
	}
	help := helpBar("f", "toggle follow", "↑/↓/pgup/pgdn", "scroll", "esc", "back")
	return styleTitle.Render(" "+title+" ") + "\n\n" + m.viewport.View() + "\n" + help
}
