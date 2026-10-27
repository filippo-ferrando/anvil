package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// maxLogLines bounds how much log text logsModel keeps in memory while following.
const maxLogLines = 2000

// logsModel is the Logs screen: streams an instance's console/stdout output into a scrollable viewport.
type logsModel struct {
	inst      *anvilv1.Instance
	viewport  viewport.Model
	lines     []string
	pending   string // bytes received since the last "\n"; not yet a committed line
	following bool   // auto-scroll to the bottom on new data; turned off once the user scrolls up manually
	streaming bool

	// cancel ends the Logs RPC's context; call it when the user leaves this screen.
	cancel context.CancelFunc
}

func newLogsModel(inst *anvilv1.Instance, width, height int) logsModel {
	vp := viewport.New(width, height)
	return logsModel{inst: inst, viewport: vp, following: true, streaming: true}
}

// logsTailField is the Logs prompt's field: how many of the last lines to start from.
const logsTailField = "Tail lines"

// openLogs switches to the Logs takeover for inst, starting from its last tail lines (0: all of it).
func (m model) openLogs(inst *anvilv1.Instance, tail int32) (tea.Model, tea.Cmd) {
	m.screen = screenLogs
	m.logs = newLogsModel(inst, m.width, logsViewportHeight(m.height))
	cmd, cancel := startLogsStream(m.client, inst.GetName(), tail)
	m.logs.cancel = cancel
	return m, tea.Batch(tea.ClearScreen, cmd)
}

// startLogsStream returns the Cmd that kicks off the stream and the CancelFunc for its RPC context.
func startLogsStream(c *client.Client, name string, tail int32) (tea.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return func() tea.Msg {
		// TailLines 0 means everything available; maxLogLines bounds memory instead.
		stream, err := c.Logs(ctx, &anvilv1.LogsRequest{Name: name, Follow: true, TailLines: tail})
		if err != nil {
			return logsStreamMsg{failed[[]byte](err)}
		}
		return recvStream(stream.Recv, func(c *anvilv1.LogChunk) streamStep[[]byte] {
			return streamStep[[]byte]{result: c.GetData()}
		}, func(s streamStep[[]byte]) tea.Msg { return logsStreamMsg{s} })()
	}, cancel
}

func (m model) updateLogs(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logsStreamMsg:
		if msg.err != nil {
			m.logs.streaming = false
			m.setStatus("logs: "+msg.err.Error(), true)
			return m, nil
		}
		if len(msg.result) > 0 {
			m.logs.appendLines(string(msg.result))
		}
		if msg.done {
			m.logs.streaming = false
			return m, nil
		}
		return m, msg.next

	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "q":
			if m.logs.cancel != nil {
				m.logs.cancel()
			}
			m.screen = screenInstances
			// Force a full repaint: Logs and Instances rarely render the same
			// total line count, and Bubble Tea's diff-based erase-below doesn't reliably catch that gap, leaving stale log text on screen.
			return m, tea.ClearScreen
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

// appendLines feeds streamed bytes into the buffer and re-renders the
// viewport. Boot tools rewrite lines in place with "\r"+ANSI codes rather than "\n", so commitLine/cleanLine collapse each to its final display; "\n" boundaries are found before stripping so a split ANSI escape is fully reassembled first.
func (m *logsModel) appendLines(text string) {
	m.pending += text
	for {
		i := strings.IndexByte(m.pending, '\n')
		if i < 0 {
			break
		}
		m.commitLine(m.pending[:i])
		m.pending = m.pending[i+1:]
	}

	lines := m.lines
	if live := cleanLine(m.pending); live != "" {
		// Show the in-progress line (e.g. "Starting foo...") before its
		// terminating newline arrives.
		lines = append(append([]string{}, m.lines...), live)
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if m.following {
		m.viewport.GotoBottom()
	}
}

// commitLine appends one complete raw line to the buffer, capping it at
// maxLogLines. Blank lines are dropped: boot output pads with enough of them to flood the cap and evict genuine earlier history.
func (m *logsModel) commitLine(rawLine string) {
	line := cleanLine(rawLine)
	if line == "" {
		return
	}
	m.lines = append(m.lines, line)
	if len(m.lines) > maxLogLines {
		m.lines = m.lines[len(m.lines)-maxLogLines:]
	}
}

// cleanLine strips ANSI escapes, then resolves "\r": a lone trailing "\r" is
// just the CRLF line ending and is dropped, while any "\r" left after that is a genuine overwrite and collapses to everything after the last one.
func cleanLine(line string) string {
	line = strings.TrimSuffix(ansi.Strip(line), "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		return line[i+1:]
	}
	return line
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
