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

// maxLogLines bounds how much log text logsModel keeps in memory while following.
const maxLogLines = 2000

// logsModel is the Logs screen: streams an instance's console/stdout output into a scrollable viewport.
type logsModel struct {
	inst      *anvilv1.Instance
	viewport  viewport.Model
	lines     []string
	following bool // auto-scroll to the bottom on new data; turned off once the user scrolls up manually
	streaming bool

	// cancel ends the Logs RPC's context; call it when the user leaves this screen.
	cancel context.CancelFunc
}

func newLogsModel(inst *anvilv1.Instance, width, height int) logsModel {
	vp := viewport.New(width, height)
	return logsModel{inst: inst, viewport: vp, following: true, streaming: true}
}

// startLogsStream returns the Cmd that kicks off the stream and the CancelFunc for its RPC context.
func startLogsStream(c *client.Client, name string) (tea.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return func() tea.Msg {
		stream, err := c.Logs(ctx, &anvilv1.LogsRequest{Name: name, Follow: true, TailLines: 200})
		if err != nil {
			return logsStreamMsg{err: err, done: true}
		}
		return receiveLogChunk(stream)()
	}, cancel
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
			if m.logs.cancel != nil {
				m.logs.cancel()
			}
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

// appendLines adds text to the buffer, capping it at maxLogLines, and re-renders the viewport.
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
