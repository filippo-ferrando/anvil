package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/progress"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

const (
	finishedTasksKept = 50          // finished tasks the Tasks screen remembers
	taskLogRetry      = time.Second // wait before asking again for a log that isn't there yet
	taskStepsShown    = 8           // progress lines above the console in a task's view
	deleteLogTail     = 30          // console lines a delete starts from
)

var taskSeq atomic.Int64

// task is one request running in the background: a launch, a delete, any action.
type task struct {
	id      int
	title   string // what it does, e.g. "launching web"
	done    string // the status line once it worked, e.g. "launched web"
	origin  screen // the screen reloaded once it ends
	running bool
	err     error
	started time.Time
	ended   time.Time
	steps   taskSteps          // streamed progress, empty for a single call
	cancel  context.CancelFunc // aborts the request, nil when it can't be
	// cancelNote says what cancelling leaves behind, shown before it happens.
	cancelNote string

	// The console log of the instance the task is about, followed while it runs.
	instance  string
	logTail   int32
	console   logsModel
	logGen    int
	logCancel context.CancelFunc
}

// tasksModel is the Tasks screen and the registry of every task started from the TUI.
type tasksModel struct {
	items         []*task // oldest first
	cursor        int     // index in the newest-first list
	open          int     // id of the task shown in full, 0 for the list
	confirmCancel bool
	width, height int
}

type taskDoneMsg struct {
	id  int
	msg tea.Msg // the reply of the tracked call, handled as if it came alone
}

type taskStepMsg struct {
	id int
	lineStep
	name string // the instance the task is about, once known
}

type taskLogMsg struct {
	id, gen int
	streamStep[[]byte]
}

type taskLogRetryMsg struct{ id, gen int }

type taskLogRenderMsg struct{ id int }

func (t *task) elapsed() time.Duration {
	if t.running {
		return time.Since(t.started)
	}
	return t.ended.Sub(t.started)
}

// last is the task's latest progress line, "" for none.
func (t *task) last() string {
	if n := len(t.steps.lines); n > 0 {
		return progress.OneLine(t.steps.lines[n-1])
	}
	return ""
}

func (m *tasksModel) get(id int) *task {
	for _, t := range m.items {
		if t.id == id {
			return t
		}
	}
	return nil
}

// running returns the tasks still going, oldest first.
func (m tasksModel) running() []*task {
	var out []*task
	for _, t := range m.items {
		if t.running {
			out = append(out, t)
		}
	}
	return out
}

// newestFirst is the order the Tasks screen lists them in.
func (m tasksModel) newestFirst() []*task {
	out := make([]*task, len(m.items))
	for i, t := range m.items {
		out[len(m.items)-1-i] = t
	}
	return out
}

// forInstance returns the newest running task about name, nil for none.
func (m tasksModel) forInstance(name string) *task {
	for i := len(m.items) - 1; i >= 0; i-- {
		if t := m.items[i]; t.running && t.instance == name {
			return t
		}
	}
	return nil
}

func (m *tasksModel) add(t *task) {
	m.items = append(m.items, t)
	m.prune()
}

// prune forgets the oldest finished tasks past finishedTasksKept.
func (m *tasksModel) prune() {
	finished := 0
	for _, it := range m.items {
		if !it.running {
			finished++
		}
	}
	kept := m.items[:0]
	for _, it := range m.items {
		if !it.running && finished > finishedTasksKept && it.id != m.open {
			finished--
			continue
		}
		kept = append(kept, it)
	}
	m.items = kept
}

func (m *tasksModel) setSize(width, height int) {
	m.width, m.height = width, height
	for _, t := range m.items {
		t.console.viewport.Width, t.console.viewport.Height = width, m.consoleHeight()
	}
}

// consoleHeight leaves room for the title, the progress lines and the help bar.
func (m tasksModel) consoleHeight() int {
	return max(m.height-taskStepsShown-6, 3)
}

// startTask registers a running task. Its title is the progressive form, done the past one.
func (m *model) startTask(title, done string, origin screen) *task {
	t := &task{
		id: int(taskSeq.Add(1)), title: title, done: done, origin: origin,
		running: true, started: time.Now(),
	}
	t.console = newLogsModel(nil, m.tasks.width, m.tasks.consoleHeight())
	m.tasks.add(t)
	return t
}

// track runs cmd as a task, shown under every screen until its reply comes back.
func (m *model) track(title string, origin screen, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	id := m.startTask(title, "", origin).id
	return func() tea.Msg { return taskDoneMsg{id: id, msg: cmd()} }
}

// streamTask starts a streamed task: start opens the stream on ctx, cancelled once it ends.
func (m *model) streamTask(title, done string, origin screen, start func(ctx context.Context, id int) tea.Cmd) (*task, tea.Cmd) {
	t := m.startTask(title, done, origin)
	t.steps.start()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	return t, start(ctx, t.id)
}

// runTask mirrors run view v as a task; esc on the view and x on the task both cancel it.
func (m *model) runTask(v *runView, title, done string, origin screen) {
	t := m.startTask(title, done, origin)
	t.steps.start()
	t.cancel = v.cancel
	t.cancelNote = "Cancels the run and deletes its clones."
	v.task = t.id
}

// runTaskStep adds line to the progress of v's task.
func (m *model) runTaskStep(v *runView, line string) {
	if t := m.tasks.get(v.task); t != nil && t.running && line != "" {
		t.steps.add(line)
	}
}

// endRunTask ends v's task with err.
func (m *model) endRunTask(v *runView, err error) tea.Cmd {
	if t := m.tasks.get(v.task); t != nil && t.running {
		return m.finishTask(t, err)
	}
	return nil
}

// lineTaskStep wraps the steps of a stream that only reports lines into task t's messages.
func lineTaskStep(id int) func(lineStep) tea.Msg {
	return func(s lineStep) tea.Msg { return taskStepMsg{id: id, lineStep: s} }
}

// launchTaskStep wraps the steps of a launch-like stream, ending with "<verb> <name>".
func launchTaskStep(id int, verb string) func(instanceStep) tea.Msg {
	return func(s instanceStep) tea.Msg {
		out := taskStepMsg{id: id, name: s.name}
		out.status, out.err, out.done, out.next = s.status, s.err, s.done, s.next
		if s.done && s.result != nil {
			line := verb + " " + s.result.GetName()
			if ips := s.result.GetGuest().GetIpAddresses(); len(ips) > 0 {
				line += " (" + strings.Join(ips, ", ") + ")"
			}
			out.status, out.name = styleGood.Render(line), s.result.GetName()
		}
		return out
	}
}

// followTaskLog starts following t's instance console from its last logTail lines.
func (m *model) followTaskLog(t *task) tea.Cmd {
	if t.instance == "" || !t.running {
		return nil
	}
	if t.logCancel != nil {
		t.logCancel()
	}
	t.logGen++
	t.console = newLogsModel(nil, m.tasks.width, m.tasks.consoleHeight())
	ctx, cancel := context.WithCancel(context.Background())
	t.logCancel = cancel
	return openTaskLog(ctx, m.client, t.id, t.logGen, t.instance, t.logTail)
}

func openTaskLog(ctx context.Context, c *client.Client, id, gen int, name string, tail int32) tea.Cmd {
	wrap := func(s streamStep[[]byte]) tea.Msg { return taskLogMsg{id: id, gen: gen, streamStep: s} }
	return func() tea.Msg {
		stream, err := c.Logs(ctx, &anvilv1.LogsRequest{Name: name, Follow: true, TailLines: tail})
		if err != nil {
			return wrap(failed[[]byte](err))
		}
		return recvStream(stream.Recv, func(ch *anvilv1.LogChunk) streamStep[[]byte] {
			return streamStep[[]byte]{result: ch.GetData()}
		}, wrap)()
	}
}

func (t *task) stopLog() {
	if t.logCancel != nil {
		t.logCancel()
		t.logCancel = nil
	}
	t.console.streaming = false
	t.console.render()
}

// finishTask ends t with err and reloads the screen it came from.
func (m *model) finishTask(t *task, err error) tea.Cmd {
	t.running, t.err, t.ended = false, err, time.Now()
	t.steps.running = false
	if t.cancel != nil {
		t.cancel()
	}
	t.stopLog()
	m.tasks.prune()
	switch {
	case err != nil && status.Code(err) == codes.Canceled:
		t.steps.lines = append(t.steps.lines, styleSubtitle.Render("cancelled"))
		m.setStatus(t.title+": cancelled", true)
	case err != nil:
		t.steps.lines = append(t.steps.lines, errLine(err))
		m.setStatus(t.title+": "+err.Error(), true)
	case t.done != "":
		m.setStatus(t.done, false)
	}
	return m.loadCmdForScreen(t.origin)
}

// replyErr is the error a tracked call's reply carries, nil for none.
func replyErr(msg tea.Msg) error {
	switch r := msg.(type) {
	case actionDoneMsg:
		return r.err
	case hostTestedMsg:
		return r.err
	case hostsDiscoveredMsg:
		return r.err
	case cloudInitSavedMsg:
		return r.err
	case imageChecksumMsg:
		return r.err
	case catalogUpdatedMsg:
		return r.err
	}
	return nil
}

func (m model) updateTasks(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case taskDoneMsg:
		if t := m.tasks.get(msg.id); t != nil {
			t.running, t.err, t.ended = false, replyErr(msg.msg), time.Now()
			t.stopLog()
			m.tasks.prune()
		}
		return m.update(msg.msg) // the reply goes where it always went
	case taskStepMsg:
		t := m.tasks.get(msg.id)
		if t == nil || !t.running {
			return m, nil
		}
		var cmds []tea.Cmd
		if msg.name != "" && t.instance == "" {
			t.instance = msg.name
			if !msg.done && msg.err == nil {
				cmds = append(cmds, m.followTaskLog(t))
			}
		}
		if msg.status != "" {
			t.steps.add(msg.status)
		}
		switch {
		case msg.err != nil:
			cmds = append(cmds, m.finishTask(t, msg.err))
		case msg.done:
			cmds = append(cmds, m.finishTask(t, nil))
		default:
			cmds = append(cmds, msg.next)
		}
		return m, tea.Batch(cmds...)
	case taskLogMsg:
		t := m.tasks.get(msg.id)
		if t == nil || !t.running || msg.gen != t.logGen {
			return m, nil // the task ended or the log was reopened
		}
		if len(msg.result) > 0 {
			t.console.appendLines(string(msg.result))
		}
		if msg.err != nil || msg.done {
			// Not there yet (still provisioning) or the stream broke: ask again shortly.
			id, gen := t.id, t.logGen
			return m, tea.Tick(taskLogRetry, func(time.Time) tea.Msg { return taskLogRetryMsg{id: id, gen: gen} })
		}
		if t.console.dirty && !t.console.queued {
			t.console.queued = true
			id := t.id
			return m, tea.Batch(msg.next, tea.Tick(logsRenderEvery, func(time.Time) tea.Msg { return taskLogRenderMsg{id: id} }))
		}
		return m, msg.next
	case taskLogRetryMsg:
		if t := m.tasks.get(msg.id); t != nil && t.running && msg.gen == t.logGen {
			return m, m.followTaskLog(t)
		}
		return m, nil
	case taskLogRenderMsg:
		if t := m.tasks.get(msg.id); t != nil {
			t.console.queued = false
			t.console.render()
		}
		return m, nil
	case tea.KeyMsg:
		return m.updateTasksKey(msg)
	}
	return m, nil
}

func (m model) updateTasksKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	tm := &m.tasks
	list := tm.newestFirst()
	if tm.confirmCancel {
		tm.confirmCancel = false
		if t := tm.selected(list); t != nil && t.running && t.cancel != nil && (msg.String() == "y" || msg.String() == "enter") {
			t.cancel()
		}
		return m, nil
	}
	if t := tm.get(tm.open); t != nil {
		switch msg.String() {
		case "esc", "q":
			tm.open = 0
			return m, nil
		case "x":
			if t.running && t.cancel != nil {
				tm.confirmCancel = true
			}
			return m, nil
		case "f":
			t.console.following = !t.console.following
			if t.console.following {
				t.console.viewport.GotoBottom()
			}
			return m, nil
		}
		wasAtBottom := t.console.viewport.AtBottom()
		var cmd tea.Cmd
		t.console.viewport, cmd = t.console.viewport.Update(msg)
		if wasAtBottom && !t.console.viewport.AtBottom() {
			t.console.following = false
		}
		return m, cmd
	}
	switch msg.String() {
	case "esc", "q":
		m.sidebarFocused = true
	case "up", "k":
		tm.cursor = max(tm.cursor-1, 0)
	case "down", "j":
		tm.cursor = min(tm.cursor+1, max(len(list)-1, 0))
	case "home", "g":
		tm.cursor = 0
	case "end", "G":
		tm.cursor = max(len(list)-1, 0)
	case "enter", "l":
		if t := tm.selected(list); t != nil {
			tm.open = t.id
			t.console.viewport.Width, t.console.viewport.Height = tm.width, tm.consoleHeight()
			t.console.dirty = true
			t.console.render()
		}
	case "x":
		if t := tm.selected(list); t != nil && t.running && t.cancel != nil {
			tm.confirmCancel = true
		}
	case "c":
		kept := tm.items[:0]
		for _, t := range tm.items {
			if t.running {
				kept = append(kept, t)
			}
		}
		tm.items, tm.cursor = kept, 0
	}
	return m, nil
}

// selected is the task under the cursor (or the open one), nil for none.
func (m *tasksModel) selected(list []*task) *task {
	if t := m.get(m.open); t != nil {
		return t
	}
	if m.cursor >= len(list) {
		m.cursor = max(len(list)-1, 0)
	}
	if len(list) == 0 {
		return nil
	}
	return list[m.cursor]
}

// taskGlyph marks a task's state in the list and the strip.
func taskGlyph(t *task) string {
	switch {
	case t.running:
		return styleWarn.Render("⟳")
	case t.err != nil:
		return styleError.Render("✕")
	default:
		return styleGood.Render("✓")
	}
}

// taskRow is one task on a single line: state, title, latest step and time.
func taskRow(t *task) string {
	row := taskGlyph(t) + " " + t.title
	if t.running && t.last() != "" {
		row += styleSubtitle.Render(" · " + t.last())
	} else if t.err != nil {
		msg, _ := friendlyError(t.err.Error())
		row += styleError.Render(": " + msg)
	}
	return row + styleSubtitle.Render("  "+t.elapsed().Truncate(time.Second).String())
}

func (m tasksModel) View() string {
	list := m.newestFirst()
	if t := m.get(m.open); t != nil {
		return m.taskView(t)
	}
	title := styleTitle.Render(" Tasks ")
	if len(list) == 0 {
		return title + "\n\n" + styleSubtitle.Render("Nothing started from this TUI yet. Launches, deletes and other actions show up here while they run.") +
			"\n\n" + helpBar("esc", "menu")
	}
	if m.confirmCancel {
		if t := m.selected(list); t != nil {
			return m.cancelPrompt(t)
		}
	}
	room := max(m.height-4, 1)
	start := 0
	if m.cursor >= room {
		start = m.cursor - room + 1
	}
	row := lipgloss.NewStyle().MaxWidth(max(m.width-2, 10))
	var rows []string
	for i := start; i < len(list) && i < start+room; i++ {
		r := taskRow(list[i])
		if i == m.cursor {
			r = styleMenuItemSelected.Render("▸") + " " + r
		} else {
			r = "  " + r
		}
		rows = append(rows, row.Render(r))
	}
	help := helpBar("↑↓", "move", "enter", "progress and console", "x", "cancel", "c", "clear finished", "esc", "menu")
	return title + "\n\n" + strings.Join(rows, "\n") + "\n\n" + help
}

func (m tasksModel) cancelPrompt(t *task) string {
	note := t.cancelNote
	if note == "" {
		note = "The daemon stops the request where it is."
	}
	return styleBox.BorderForeground(colorWarn).Render(
		styleWarn.Bold(true).Render("⚠  Cancel "+t.title+"?")+"\n\n"+styleSubtitle.Render(note)) + "\n\n" +
		helpBar("y", "cancel it", "any other key", "keep it running")
}

// taskView is one task in full: its progress, then the console of its instance.
func (m tasksModel) taskView(t *task) string {
	if m.confirmCancel {
		return m.cancelPrompt(t)
	}
	head := styleTitle.Render(" "+t.title+" ") + " " + taskGlyph(t) +
		styleSubtitle.Render(" "+t.elapsed().Truncate(time.Second).String())
	var b strings.Builder
	b.WriteString(head + "\n\n")
	lines := t.steps.lines
	if len(lines) > taskStepsShown {
		lines = lines[len(lines)-taskStepsShown:]
	}
	switch {
	case len(lines) > 0:
		b.WriteString(progressView(lines, t.running, t.steps.since))
	case t.running:
		b.WriteString(progress.Live(t.title, time.Since(t.started), int(time.Since(t.started).Seconds())) + "\n")
	case t.err != nil:
		b.WriteString(errLine(t.err) + "\n")
	default:
		b.WriteString(styleGood.Render("done") + "\n")
	}
	help := []string{"esc", "back"}
	if t.instance != "" {
		label := "Console of " + t.instance
		switch {
		case t.running && !t.console.following:
			label += " (scrolled back, f to follow)"
		case t.running:
			label += " (following)"
		}
		b.WriteString("\n" + styleSubtitle.Render(label) + "\n")
		if len(t.console.lines) == 0 && t.console.pending == "" {
			wait := "no output yet"
			if t.running {
				wait = "waiting for the instance to write to its console…"
			}
			b.WriteString(styleSubtitle.Render(wait) + "\n")
		} else {
			b.WriteString(t.console.viewport.View() + "\n")
		}
		help = append(help, "f", "toggle follow", "↑↓ pgup/pgdn", "scroll")
	}
	if t.running && t.cancel != nil {
		help = append(help, "x", "cancel")
	}
	return b.String() + helpBar(help...)
}

// stripView is the one-line summary of the running tasks under every screen.
func (m tasksModel) stripView(width int) string {
	run := m.running()
	if len(run) == 0 {
		return ""
	}
	parts := make([]string, 0, len(run))
	for _, t := range run {
		s := t.title
		if l := t.last(); l != "" {
			s += ": " + l
		}
		el := time.Since(t.started)
		parts = append(parts, progress.Live(s, max(el, time.Second), int(el.Seconds())))
	}
	s := styleWarn.Render("⟳ ") + strings.Join(parts, styleSubtitle.Render("  •  "))
	hint := styleSubtitle.Render("  (" + tasksKey() + ": tasks)")
	return lipgloss.NewStyle().MaxWidth(max(width, 10)).Render(s + hint)
}

// tasksKey is the key that opens the Tasks screen from anywhere.
func tasksKey() string {
	return "alt+" + screenDigit(screenIndex(screenTasks))
}

// inBackground is the status line of a task just started.
func inBackground(title string) string {
	return title + " in the background, " + tasksKey() + " shows its progress"
}

// activityView is what the Instances detail panel shows about a running task on its instance.
func (t *task) activityView(width, lines int) string {
	rows := []string{styleWarn.Render("⟳ ") + t.title}
	if l := t.last(); l != "" {
		rows = append(rows, styleSubtitle.Render("  "+l))
	}
	tail := t.console.lines
	if n := max(lines, 0); len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	row := lipgloss.NewStyle().MaxWidth(max(width, 10))
	for _, l := range tail {
		rows = append(rows, row.Render(styleSubtitle.Render("  │ ")+l))
	}
	return strings.Join(rows, "\n")
}
