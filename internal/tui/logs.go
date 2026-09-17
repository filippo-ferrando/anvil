package tui

import (
	"context"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

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

// startLogsStream returns the Cmd that kicks off the stream and the CancelFunc for its RPC context.
func startLogsStream(c *client.Client, name string) (tea.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	return func() tea.Msg {
		// TailLines: 0 means "everything available" server-side (see
		// vm.Backend.Logs); maxLogLines below is what actually bounds how
		// much of it this view keeps. A fixed tail here meant reopening
		// Logs after the first view lost everything before that cutoff.
		stream, err := c.Logs(ctx, &anvilv1.LogsRequest{Name: name, Follow: true, TailLines: 0})
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
			// Force a full repaint: Logs and Instances rarely render the
			// exact same total line count, and relying on Bubble Tea's
			// diff-based erase-below to always catch that gap left stale
			// log text on screen.
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

// appendLines feeds newly streamed bytes into the buffer and re-renders the
// viewport. Console output isn't line-oriented text: boot tools like
// systemd rewrite a status line in place with "\r" plus ANSI cursor/clear
// codes ("Starting foo...\r\x1b[K[ OK ] Started foo.") instead of emitting
// a fresh line. Replayed raw, those control bytes fight with Bubble Tea's
// own cursor control — that's what made the log view go ragged partway
// through boot. commitLine/resolveOverwrite collapse each completed line
// down to what a real terminal would end up displaying.
//
// "\n" boundaries are found in the raw, unstripped text first, so an ANSI
// escape sequence split across two gRPC chunks is always fully
// reassembled in pending before it's stripped.
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

// commitLine appends one complete raw (newline-terminated) line to the
// buffer, capping it at maxLogLines. Blank lines are dropped rather than
// stored: the real console output runs blank-line padding between some
// boot messages (e.g. around the UEFI-to-systemd handoff), and faithfully
// storing every one of them was flooding the cap and evicting genuine
// earlier history off the front.
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

// cleanLine strips ANSI escapes, then resolves "\r": console output uses
// "\r\n" line endings, so a lone trailing "\r" (the common case) is just
// that CRLF and is dropped outright, while any "\r" still left after that
// is a genuine in-place overwrite ("Starting foo...\r[ OK ] Started foo.")
// and collapses to what a real terminal would end up displaying:
// everything after the last "\r".
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
