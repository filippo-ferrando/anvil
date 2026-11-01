package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// The lists of the CI screen, switched with tab.
const (
	ciRunners = iota
	ciJobs
	ciRuns
	ciWatches
	ciTabs
)

var ciTitles = [ciTabs]string{"Runners", "Runner jobs", "Pipeline runs", "Pipeline watches"}

// ciTokenEnv is where the runner form reads the forge token when both token fields are empty.
const ciTokenEnv = "ANVIL_FORGE_TOKEN"

// ciLogsMax bounds the log text kept in the viewer; older output is cut.
const ciLogsMax = 4 << 20

type runnerItem struct{ r *anvilv1.Runner }

func (i runnerItem) FilterValue() string { return i.r.GetName() }
func (i runnerItem) Title() string {
	return i.r.GetName() + "  " + i.r.GetForge() + " " + i.r.GetScope()
}
func (i runnerItem) Description() string {
	state := "polling"
	switch {
	case i.r.GetPaused():
		state = "paused"
	case i.r.GetLastError() != "":
		state = "error: " + i.r.GetLastError()
	}
	var labels []string
	for l, d := range i.r.GetLabels() {
		labels = append(labels, l+"="+d)
	}
	sort.Strings(labels)
	return fmt.Sprintf("%d/%d jobs  •  %s  •  %s", i.r.GetRunning(), i.r.GetMaxJobs(), strings.Join(labels, ","), state)
}

type runnerJobItem struct{ j *anvilv1.RunnerJob }

func (i runnerJobItem) FilterValue() string { return i.j.GetName() }
func (i runnerJobItem) Title() string {
	return fmt.Sprintf("%s  job %d  %s", i.j.GetRunner(), i.j.GetJobId(), i.j.GetName())
}
func (i runnerJobItem) Description() string {
	s := fmt.Sprintf("%s  •  %s  •  %s  •  %s", i.j.GetState(), i.j.GetDistro(), time.Unix(i.j.GetStartedAtUnix(), 0).Format("01-02 15:04"),
		ciDuration(i.j.GetStartedAtUnix(), i.j.GetEndedAtUnix()))
	if i.j.GetCloneId() != "" {
		s += "  •  " + i.j.GetCloneId()
	}
	if e := lastLine(i.j.GetError()); e != "" {
		s += "  •  " + e
	}
	return s
}

type pipelineRunItem struct{ r *anvilv1.PipelineRun }

func (i pipelineRunItem) FilterValue() string { return i.r.GetId() }
func (i pipelineRunItem) Title() string       { return i.r.GetId() + "  " + i.r.GetSource() }
func (i pipelineRunItem) Description() string {
	ok := 0
	for _, j := range i.r.GetJobs() {
		if j.GetState() == "succeeded" {
			ok++
		}
	}
	s := fmt.Sprintf("%s  •  %d/%d jobs passed  •  %s  •  %s", i.r.GetState(), ok, len(i.r.GetJobs()),
		time.Unix(i.r.GetStartedAtUnix(), 0).Format("01-02 15:04"), ciDuration(i.r.GetStartedAtUnix(), i.r.GetEndedAtUnix()))
	if c := i.r.GetCommit(); len(c) >= 10 {
		s += "  •  " + c[:10]
	}
	return s
}

type watchItem struct{ w *anvilv1.PipelineWatch }

func (i watchItem) FilterValue() string { return i.w.GetName() }
func (i watchItem) Title() string {
	return i.w.GetName() + "  " + i.w.GetRepo() + " " + i.w.GetBranch()
}
func (i watchItem) Description() string {
	state := "watching"
	switch {
	case i.w.GetPaused():
		state = "paused"
	case i.w.GetLastError() != "":
		state = "error: " + lastLine(i.w.GetLastError())
	}
	last := "no run yet"
	if i.w.GetLastRun() != "" {
		last = "last run " + i.w.GetLastRun()
	}
	return fmt.Sprintf("every %s  •  %s  •  %s", time.Duration(i.w.GetIntervalSeconds())*time.Second, last, state)
}

func ciDuration(start, end int64) string {
	if end == 0 {
		return "running " + time.Since(time.Unix(start, 0)).Round(time.Second).String()
	}
	return (time.Duration(end-start) * time.Second).String()
}

type ciModel struct {
	tab       int
	lists     [ciTabs]list.Model
	detail    *anvilv1.PipelineRun // a run's jobs on screen, nil for the lists
	jobCursor int                  // the highlighted job of detail
	width     int
	height    int

	form     *simpleForm // the open form, nil for none
	formKind string      // "runner" | "watch" | "run" | "artifacts"
	formRun  string      // the run id the artifacts form is for
	confirm  string      // the runner or watch name waiting for a yes, on the current tab

	run          *runView             // a pipeline run started here, streaming
	runInfo      *anvilv1.PipelineRun // the streamed run as last reported
	runArtifacts string               // where to put the streamed run's files when it ends

	logs *ciLogs // the log viewer, nil when closed
}

// ciLogs is the log viewer of a run or of one of its jobs.
type ciLogs struct {
	title   string
	loading bool
	vp      viewport.Model
}

func newCIModel() ciModel {
	var m ciModel
	for i := range m.lists {
		m.lists[i] = newList()
		m.lists[i].Title = ciTitles[i]
		m.lists[i].SetShowHelp(false)
		m.lists[i].SetFilteringEnabled(false)
	}
	return m
}

func (c *ciModel) setSize(w, h int) {
	c.width, c.height = w, h
	for i := range c.lists {
		c.lists[i].SetSize(w, h)
	}
	if c.logs != nil {
		c.logs.vp.Width, c.logs.vp.Height = w, max(1, h-1)
	}
}

// ciLoadedMsg carries everything the CI screen lists.
type ciLoadedMsg struct {
	runners []*anvilv1.Runner
	jobs    []*anvilv1.RunnerJob
	runs    []*anvilv1.PipelineRun
	watches []*anvilv1.PipelineWatch
	err     error
}

// pipelineEventMsg carries one event of a pipeline run streamed here, or its end.
type pipelineEventMsg struct {
	ev   *anvilv1.PipelineEvent
	next tea.Cmd
	err  error
	done bool
}

// ciLogsMsg carries a run's logs for the viewer.
type ciLogsMsg struct {
	text string
	err  error
}

func loadCI(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		var msg ciLoadedMsg
		runners, err := c.Runner.List(ctx, &anvilv1.RunnerListRequest{})
		if err != nil {
			return ciLoadedMsg{err: err}
		}
		jobs, err := c.Runner.Jobs(ctx, &anvilv1.RunnerJobsRequest{})
		if err != nil {
			return ciLoadedMsg{err: err}
		}
		runs, err := c.Pipeline.ListRuns(ctx, &anvilv1.PipelineListRunsRequest{Limit: 100})
		if err != nil {
			return ciLoadedMsg{err: err}
		}
		watches, err := c.Pipeline.ListWatches(ctx, &anvilv1.PipelineWatchListRequest{})
		if err != nil {
			return ciLoadedMsg{err: err}
		}
		msg.runners, msg.jobs, msg.runs, msg.watches = runners.GetRunners(), jobs.GetJobs(), runs.GetRuns(), watches.GetWatches()
		return msg
	}
}

// set fills the lists from a load, keeping each list's selection where it was.
func (c *ciModel) set(msg ciLoadedMsg) {
	items := [ciTabs][]list.Item{}
	for _, r := range msg.runners {
		items[ciRunners] = append(items[ciRunners], runnerItem{r})
	}
	for _, j := range msg.jobs {
		items[ciJobs] = append(items[ciJobs], runnerJobItem{j})
	}
	for _, r := range msg.runs {
		items[ciRuns] = append(items[ciRuns], pipelineRunItem{r})
		if c.detail != nil && c.detail.GetId() == r.GetId() {
			c.detail = r
		}
	}
	for _, w := range msg.watches {
		items[ciWatches] = append(items[ciWatches], watchItem{w})
	}
	for i := range c.lists {
		idx := c.lists[i].Index()
		c.lists[i].SetItems(items[i])
		if idx < len(items[i]) {
			c.lists[i].Select(idx)
		}
	}
}

func (m model) updateCI(msg tea.Msg) (tea.Model, tea.Cmd) {
	ci := &m.ci
	switch msg := msg.(type) {
	case ciLoadedMsg:
		if msg.err != nil {
			m.setStatus("loading CI: "+msg.err.Error(), true)
			return m, nil
		}
		ci.set(msg)
		return m, nil
	case pipelineEventMsg:
		return m.pipelineEvent(msg)
	case ciLogsMsg:
		if ci.logs == nil {
			return m, nil
		}
		ci.logs.loading = false
		text := msg.text
		if msg.err != nil {
			text += "\n" + styleError.Render(msg.err.Error())
		}
		if strings.TrimSpace(text) == "" {
			text = styleSubtitle.Render("no output")
		}
		ci.logs.vp.SetContent(text)
		ci.logs.vp.GotoBottom()
		return m, nil
	case actionDoneMsg:
		m.setStatus(msg.verb, msg.err != nil)
		if msg.err != nil {
			m.setStatus(msg.verb+": "+msg.err.Error(), true)
		}
		return m, loadCI(m.client)
	case tea.KeyMsg:
		return m.updateCIKey(msg)
	}
	return m, nil
}

// pipelineEvent feeds a streamed run's event into its run view, the same way the CLI prints it.
func (m model) pipelineEvent(msg pipelineEventMsg) (tea.Model, tea.Cmd) {
	ci := &m.ci
	v := ci.run
	if v == nil {
		return m, nil
	}
	switch e := msg.ev.GetEvent().(type) {
	case *anvilv1.PipelineEvent_Run:
		ci.runInfo = e.Run
		v.title = "Pipeline run " + e.Run.GetId()
		if len(v.order) == 0 && e.Run.GetState() == "running" {
			var keys []string
			for _, j := range e.Run.GetJobs() {
				keys = append(keys, j.GetName()+"@"+j.GetDistro())
			}
			v.setPanes(keys)
		}
	case *anvilv1.PipelineEvent_Progress:
		p := e.Progress
		if p.GetJob() == "" {
			v.note = p.GetMessage()
			break
		}
		v.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Progress{Progress: &anvilv1.RunProgress{Distro: p.GetJob() + "@" + p.GetDistro(), Message: p.GetMessage()}}})
	case *anvilv1.PipelineEvent_Output:
		o := e.Output
		v.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: o.GetJob() + "@" + o.GetDistro(), Stream: o.GetStream(), Data: o.GetData()}}})
	case *anvilv1.PipelineEvent_Job:
		j := e.Job
		x := &anvilv1.RunExit{Distro: j.GetName() + "@" + j.GetDistro(), ExitCode: j.GetExitCode(), DurationMs: j.GetDurationMs(), Error: j.GetError()}
		switch j.GetState() {
		case "succeeded", "failed":
		case "skipped":
			x.ExitCode, x.Error = -1, "skipped: "+j.GetError()
		default:
			return m, msg.next
		}
		v.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: x}})
	}
	if !msg.done {
		return m, msg.next
	}
	v.done, v.err = true, msg.err
	cmds := []tea.Cmd{loadCI(m.client)}
	if r := ci.runInfo; r != nil {
		v.note = "run " + r.GetId() + " " + r.GetState()
		if r.GetError() != "" {
			v.note += ": " + r.GetError()
		}
		if r.GetApplied() != "" {
			v.note += "  •  apply: " + r.GetApplied()
		}
		if ci.runArtifacts != "" {
			cmds = append(cmds, downloadRunArtifacts(m.client, r.GetId(), ci.runArtifacts))
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) updateCIKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.ci
	key := msg.String()
	switch {
	case ci.run != nil:
		if key == "esc" || key == "q" {
			if !ci.run.done {
				ci.run.cancel() // a dropped stream cancels the run and deletes its clones
				return m, nil
			}
			ci.run, ci.runInfo, ci.runArtifacts = nil, nil, ""
			return m, loadCI(m.client)
		}
		return m, nil
	case ci.logs != nil:
		if key == "esc" || key == "q" {
			ci.logs = nil
			return m, nil
		}
		var cmd tea.Cmd
		ci.logs.vp, cmd = ci.logs.vp.Update(msg)
		return m, cmd
	case ci.confirm != "":
		name := ci.confirm
		ci.confirm = ""
		if key != "y" && key != "enter" {
			return m, nil
		}
		if ci.tab == ciWatches {
			return m, removeWatch(m.client, name)
		}
		return m, removeRunner(m.client, name)
	case ci.form != nil:
		return m.updateCIForm(msg)
	case ci.detail != nil:
		return m.updateCIDetailKey(key)
	}

	l := &ci.lists[ci.tab]
	switch key {
	case "esc", "q":
		m.sidebarFocused = true
		return m, nil
	case "tab":
		ci.tab = (ci.tab + 1) % ciTabs
		return m, nil
	case "shift+tab":
		ci.tab = (ci.tab + ciTabs - 1) % ciTabs
		return m, nil
	case "r":
		return m, loadCI(m.client)
	case "a":
		switch ci.tab {
		case ciRunners:
			m.openCIForm(newRunnerForm(), "runner")
		case ciWatches:
			m.openCIForm(newWatchForm(), "watch")
		}
		return m, nil
	case "n":
		if ci.tab == ciRuns {
			m.openCIForm(newPipelineRunForm(), "run")
		}
		return m, nil
	case "enter":
		if it, ok := l.SelectedItem().(pipelineRunItem); ok {
			ci.detail, ci.jobCursor = it.r, 0
		}
		return m, nil
	case "p":
		switch it := l.SelectedItem().(type) {
		case runnerItem:
			return m, pauseRunner(m.client, it.r.GetName(), !it.r.GetPaused())
		case watchItem:
			return m, pauseWatch(m.client, it.w.GetName(), !it.w.GetPaused())
		}
		return m, nil
	case "t":
		if it, ok := l.SelectedItem().(watchItem); ok {
			return m, triggerWatch(m.client, it.w.GetName())
		}
		return m, nil
	case "x":
		switch it := l.SelectedItem().(type) {
		case runnerItem:
			ci.confirm = it.r.GetName()
		case watchItem:
			ci.confirm = it.w.GetName()
		case pipelineRunItem:
			if it.r.GetState() == "running" {
				return m, cancelPipelineRun(m.client, it.r.GetId())
			}
		}
		return m, nil
	}
	if it, ok := l.SelectedItem().(pipelineRunItem); ok {
		if mm, cmd, ok := m.runAction(key, it.r, nil); ok {
			return mm, cmd
		}
	}
	var cmd tea.Cmd
	*l, cmd = l.Update(msg)
	return m, cmd
}

// updateCIDetailKey handles a run's job view: up/down pick a job for its logs.
func (m model) updateCIDetailKey(key string) (tea.Model, tea.Cmd) {
	ci := &m.ci
	r := ci.detail
	switch key {
	case "esc", "q":
		ci.detail = nil
	case "up", "k":
		ci.jobCursor = max(0, ci.jobCursor-1)
	case "down", "j":
		ci.jobCursor = max(0, min(len(r.GetJobs())-1, ci.jobCursor+1))
	case "r":
		return m, loadCI(m.client)
	case "x":
		return m, cancelPipelineRun(m.client, r.GetId())
	default:
		var job *anvilv1.PipelineJob
		if ci.jobCursor < len(r.GetJobs()) {
			job = r.GetJobs()[ci.jobCursor]
		}
		if mm, cmd, ok := m.runAction(key, r, job); ok {
			return mm, cmd
		}
	}
	return m, nil
}

// runAction runs the keys a run offers both in the list and in its job view.
func (m model) runAction(key string, r *anvilv1.PipelineRun, job *anvilv1.PipelineJob) (model, tea.Cmd, bool) {
	ci := &m.ci
	switch key {
	case "l", "L":
		req := &anvilv1.PipelineLogsRequest{Id: r.GetId()}
		title := "Logs of run " + r.GetId()
		if key == "l" && job != nil {
			req.Job, req.Distro = job.GetName(), job.GetDistro()
			title = "Logs of " + job.GetName() + "@" + job.GetDistro() + " in run " + r.GetId()
		}
		ci.logs = &ciLogs{title: title, loading: true, vp: viewport.New(ci.width, max(1, ci.height-1))}
		return m, loadPipelineLogs(m.client, req), true
	case "R":
		return m, m.startPipeline("rerun of "+r.GetId(), "", func(ctx context.Context) (func() (*anvilv1.PipelineEvent, error), error) {
			stream, err := m.client.Pipeline.Rerun(ctx, &anvilv1.PipelineRerunRequest{Id: r.GetId()})
			if err != nil {
				return nil, err
			}
			return stream.Recv, nil
		}), true
	case "d":
		f := newArtifactsForm(r.GetId())
		m.openCIForm(f, "artifacts")
		ci.formRun = r.GetId()
		return m, nil, true
	}
	return m, nil, false
}

func (m *model) openCIForm(f simpleForm, kind string) {
	f.SetHeight(contentHeight(m.height) - 2)
	m.ci.form, m.ci.formKind = &f, kind
}

func (m model) updateCIForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.ci
	form, submitted, cancelled := ci.form.update(msg)
	ci.form = &form
	switch {
	case cancelled:
		ci.form = nil
		return m, nil
	case !submitted:
		return m, nil
	}
	fail := func(err error) (tea.Model, tea.Cmd) {
		ci.form.errMsg = err.Error()
		return m, nil
	}
	switch ci.formKind {
	case "runner":
		req, err := buildRunnerRequest(form, os.Getenv(ciTokenEnv))
		if err != nil {
			return fail(err)
		}
		ci.form = nil
		return m, addRunner(m.client, req)
	case "watch":
		w, err := buildWatch(form)
		if err != nil {
			return fail(err)
		}
		ci.form = nil
		return m, addWatch(m.client, w)
	case "artifacts":
		dir := strings.TrimSpace(form.Value("Directory"))
		if dir == "" {
			return fail(errors.New("directory is required"))
		}
		ci.form = nil
		return m, downloadRunArtifacts(m.client, ci.formRun, dir)
	case "run":
		path := strings.TrimSpace(form.Value("Path"))
		if path == "" {
			path = "."
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return fail(fmt.Errorf("%s is not a directory", path))
		}
		file := strings.TrimSpace(form.Value("Pipeline file"))
		ci.form = nil
		return m, m.startPipeline("pipeline run of "+path, strings.TrimSpace(form.Value("Artifacts into")), func(ctx context.Context) (func() (*anvilv1.PipelineEvent, error), error) {
			return openPipelineRun(ctx, m.client, path, file)
		})
	}
	ci.form = nil
	return m, nil
}

// startPipeline opens the run view and the stream open returns; esc on a running view cancels it.
func (m *model) startPipeline(title, artifacts string, open func(ctx context.Context) (func() (*anvilv1.PipelineEvent, error), error)) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	m.ci.run = newRunView(nil, cancel)
	m.ci.run.title, m.ci.run.note = "Pipeline", title
	m.ci.runInfo, m.ci.runArtifacts, m.ci.detail = nil, artifacts, nil
	return func() tea.Msg {
		recv, err := open(ctx)
		if err != nil {
			cancel()
			return pipelineEventMsg{err: err, done: true}
		}
		var next tea.Cmd
		next = func() tea.Msg {
			ev, err := recv()
			if errors.Is(err, io.EOF) {
				cancel()
				return pipelineEventMsg{done: true}
			}
			if err != nil {
				cancel()
				return pipelineEventMsg{err: err, done: true}
			}
			return pipelineEventMsg{ev: ev, next: next}
		}
		return next()
	}
}

// openPipelineRun starts a Run stream and uploads path into it in the background.
func openPipelineRun(ctx context.Context, c *client.Client, path, file string) (func() (*anvilv1.PipelineEvent, error), error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.Pipeline.Run(ctx)
	if err == nil {
		err = stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_Request{Request: &anvilv1.PipelineRunRequest{File: file}}})
	}
	if err != nil {
		cancel()
		return nil, err
	}
	uploadErr := make(chan error, 1)
	go func() {
		err := client.SendUpload(path, func(b []byte) error {
			return stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadChunk{UploadChunk: b}})
		})
		if err == nil {
			err = stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadDone{UploadDone: true}})
		}
		if err != nil {
			uploadErr <- fmt.Errorf("uploading %s: %w", path, err)
			cancel() // the daemon would wait for the rest of the upload
		}
	}()
	return func() (*anvilv1.PipelineEvent, error) {
		ev, err := stream.Recv()
		if err != nil && !errors.Is(err, io.EOF) {
			select {
			case err = <-uploadErr:
			default:
			}
		}
		if err != nil {
			cancel()
		}
		return ev, err
	}, nil
}

func loadPipelineLogs(c *client.Client, req *anvilv1.PipelineLogsRequest) tea.Cmd {
	return func() tea.Msg {
		stream, err := c.Pipeline.Logs(context.Background(), req)
		if err != nil {
			return ciLogsMsg{err: err}
		}
		var b []byte
		for {
			ch, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return ciLogsMsg{text: string(b), err: err}
			}
			b = append(b, ch.GetData()...)
			if len(b) > ciLogsMax {
				b = b[len(b)-ciLogsMax:]
			}
		}
		return ciLogsMsg{text: string(b)}
	}
}

func downloadRunArtifacts(c *client.Client, id, dir string) tea.Cmd {
	return func() tea.Msg {
		err := client.DownloadArtifacts(context.Background(), c.Pipeline, id, dir)
		return actionDoneMsg{screen: screenCI, verb: "files of run " + id + " in " + dir, err: err}
	}
}

func addRunner(c *client.Client, req *anvilv1.RunnerAddRequest) tea.Cmd {
	return func() tea.Msg {
		reply, err := c.Runner.Add(context.Background(), req)
		verb := "added runner " + req.GetRunner().GetName()
		if err == nil {
			verb += ", polling " + reply.GetRunner().GetUrl() + " " + reply.GetRunner().GetScope()
		}
		return actionDoneMsg{screen: screenCI, verb: verb, err: err}
	}
}

func removeRunner(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Runner.Remove(context.Background(), &anvilv1.RunnerRemoveRequest{Name: name})
		return actionDoneMsg{screen: screenCI, verb: "removed runner " + name, err: err}
	}
}

func addWatch(c *client.Client, w *anvilv1.PipelineWatch) tea.Cmd {
	return func() tea.Msg {
		got, err := c.Pipeline.AddWatch(context.Background(), &anvilv1.PipelineWatchAddRequest{Watch: w})
		verb := "added watch " + w.GetName()
		if err == nil {
			verb = fmt.Sprintf("watching %s %s every %s", got.GetRepo(), got.GetBranch(), time.Duration(got.GetIntervalSeconds())*time.Second)
		}
		return actionDoneMsg{screen: screenCI, verb: verb, err: err}
	}
}

func removeWatch(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Pipeline.RemoveWatch(context.Background(), &anvilv1.PipelineWatchRemoveRequest{Name: name})
		return actionDoneMsg{screen: screenCI, verb: "removed watch " + name, err: err}
	}
}

func pauseRunner(c *client.Client, name string, paused bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Runner.SetPaused(context.Background(), &anvilv1.RunnerSetPausedRequest{Name: name, Paused: paused})
		return actionDoneMsg{screen: screenCI, verb: map[bool]string{true: "paused runner ", false: "resumed runner "}[paused] + name, err: err}
	}
}

func pauseWatch(c *client.Client, name string, paused bool) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Pipeline.SetWatchPaused(context.Background(), &anvilv1.PipelineWatchPauseRequest{Name: name, Paused: paused})
		return actionDoneMsg{screen: screenCI, verb: map[bool]string{true: "paused watch ", false: "resumed watch "}[paused] + name, err: err}
	}
}

func triggerWatch(c *client.Client, name string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Pipeline.TriggerWatch(context.Background(), &anvilv1.PipelineWatchTriggerRequest{Name: name})
		return actionDoneMsg{screen: screenCI, verb: "triggered watch " + name, err: err}
	}
}

func cancelPipelineRun(c *client.Client, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := c.Pipeline.Cancel(context.Background(), &anvilv1.PipelineCancelRequest{Id: id})
		return actionDoneMsg{screen: screenCI, verb: "cancelled run " + id, err: err}
	}
}

// Forms.

func newRunnerForm() simpleForm {
	token := textField("Token", "the forge API token, kept by anvild", "")
	token.input.EchoMode = textinput.EchoPassword
	return newSimpleForm("Add a runner", []formField{
		textField("Name", "", ""),
		textField("Forge", `"forgejo" (Forgejo 15+) or "github"`, "forgejo"),
		textField("URL", "the forge's base URL (default https://github.com for github)", ""),
		textField("Scope", "repos/<owner>/<repo>, orgs/<org> or admin (forgejo)", ""),
		textField("Labels", "job label=distro or @warm-key, comma separated", ""),
		token,
		pathField("Token file", "a file with the token, when Token is empty (default $"+ciTokenEnv+")", ""),
		textField("CPUs", "of each job's clone, default 2", ""),
		textField("Memory (MiB)", "of each job's clone, default 2048", ""),
		textField("Max jobs", "jobs at once, default 2", ""),
		textField("Version", "runner release installed in the clones (default the latest)", ""),
		toggleField("Paused", "add it paused", false),
	})
}

// buildRunnerRequest turns the runner form into a request; envToken is used when no token is given.
func buildRunnerRequest(form simpleForm, envToken string) (*anvilv1.RunnerAddRequest, error) {
	r := &anvilv1.Runner{
		Name:    strings.TrimSpace(form.Value("Name")),
		Forge:   strings.TrimSpace(form.Value("Forge")),
		Url:     strings.TrimSpace(form.Value("URL")),
		Scope:   strings.TrimSpace(form.Value("Scope")),
		Version: strings.TrimSpace(form.Value("Version")),
		Paused:  form.Bool("Paused"),
		Labels:  map[string]string{},
	}
	if r.Name == "" || r.Scope == "" {
		return nil, errors.New("name and scope are required")
	}
	for _, p := range strings.Split(form.Value("Labels"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		label, distro, ok := strings.Cut(p, "=")
		if !ok || label == "" || distro == "" {
			return nil, fmt.Errorf("label %q: want <job label>=<distro or @warm-key>", p)
		}
		r.Labels[label] = distro
	}
	for label, dst := range map[string]any{"CPUs": &r.Cpus, "Memory (MiB)": &r.MemoryMib, "Max jobs": &r.MaxJobs} {
		n, err := optionalInt(form, label)
		if err != nil {
			return nil, err
		}
		switch d := dst.(type) {
		case *int32:
			*d = int32(n)
		case *int64:
			*d = n
		}
	}
	token := strings.TrimSpace(form.Value("Token"))
	if file := strings.TrimSpace(form.Value("Token file")); token == "" && file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading the token: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	if token == "" {
		token = strings.TrimSpace(envToken)
	}
	if token == "" {
		return nil, errors.New("give the forge API token, a token file or $" + ciTokenEnv)
	}
	return &anvilv1.RunnerAddRequest{Runner: r, Token: token}, nil
}

func newWatchForm() simpleForm {
	return newSimpleForm("Watch a branch", []formField{
		textField("Name", "", ""),
		textField("Repo", "https://, ssh://, git:// or user@host:path URL, or a local path", ""),
		textField("Branch", "default main", ""),
		textField("Pipeline file", "in the repo, default anvil-ci.yaml", ""),
		textField("Interval", "how often to look for a new commit, e.g. 5m (default 1m, at least 10s)", ""),
		toggleField("Paused", "add it paused", false),
	})
}

func buildWatch(form simpleForm) (*anvilv1.PipelineWatch, error) {
	w := &anvilv1.PipelineWatch{
		Name:   strings.TrimSpace(form.Value("Name")),
		Repo:   strings.TrimSpace(form.Value("Repo")),
		Branch: strings.TrimSpace(form.Value("Branch")),
		File:   strings.TrimSpace(form.Value("Pipeline file")),
		Paused: form.Bool("Paused"),
	}
	if w.Name == "" || w.Repo == "" {
		return nil, errors.New("name and repo are required")
	}
	if s := strings.TrimSpace(form.Value("Interval")); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("interval %q: want a duration like 30s or 5m", s)
		}
		w.IntervalSeconds = int64(d / time.Second)
	}
	return w, nil
}

func newPipelineRunForm() simpleForm {
	return newSimpleForm("Run a pipeline", []formField{
		pathField("Path", "the directory to upload, what git doesn't ignore in a repo (default .)", "."),
		textField("Pipeline file", "relative to path, default anvil-ci.yaml", ""),
		pathField("Artifacts into", "download the run's collected files here when it ends (optional)", ""),
	})
}

func newArtifactsForm(id string) simpleForm {
	return newSimpleForm("Download the files of run "+id, []formField{
		pathField("Directory", "the files land in <job>/<distro>/...", "."),
	})
}

// View.

func (c ciModel) View() string {
	switch {
	case c.run != nil:
		return c.run.view(c.width, c.height)
	case c.logs != nil:
		body := c.logs.vp.View()
		if c.logs.loading {
			body = styleSubtitle.Render("loading...")
		}
		return styleTitle.Render(" "+c.logs.title+" ") + "\n" + body + "\n" + helpBar("↑↓/pgup/pgdn", "scroll", "esc", "close")
	case c.confirm != "":
		what := "runner " + c.confirm + "? Its running jobs stop."
		if c.tab == ciWatches {
			what = "watch " + c.confirm + "? Its runs stay."
		}
		return styleWarn.Render("Remove "+what) + "\n\n" + helpBar("y", "confirm", "any other key", "cancel")
	case c.form != nil:
		return c.form.View()
	case c.detail != nil:
		return c.detailView()
	}
	keys := []string{"tab", "next list", "r", "refresh", "esc", "back"}
	switch c.tab {
	case ciRunners:
		keys = append([]string{"a", "add", "p", "pause/resume", "x", "remove"}, keys...)
	case ciRuns:
		keys = append([]string{"n", "run", "enter", "jobs", "l", "logs", "R", "rerun", "d", "files", "x", "cancel"}, keys...)
	case ciWatches:
		keys = append([]string{"a", "add", "t", "trigger", "p", "pause/resume", "x", "remove"}, keys...)
	}
	return c.lists[c.tab].View() + "\n" + helpBarWrap(c.width, keys...)
}

func (c ciModel) detailView() string {
	r := c.detail
	var b strings.Builder
	b.WriteString(styleTitle.Render(" Run "+r.GetId()+" ") + "\n")
	fmt.Fprintf(&b, "%s  •  %s  •  %s\n", r.GetState(), r.GetSource(), r.GetFile())
	if r.GetCommit() != "" {
		b.WriteString("commit " + r.GetCommit() + "\n")
	}
	if r.GetError() != "" {
		b.WriteString(styleError.Render(r.GetError()) + "\n")
	}
	if r.GetApplied() != "" {
		b.WriteString("apply: " + r.GetApplied() + "\n")
	}
	b.WriteString("\n")
	for i, j := range r.GetJobs() {
		state := j.GetState()
		switch state {
		case "succeeded":
			state = styleGood.Render(state)
		case "failed":
			state = styleError.Render(fmt.Sprintf("%s (exit %d)", state, j.GetExitCode()))
		case "skipped":
			state = styleWarn.Render(state)
		}
		cur := "  "
		if i == c.jobCursor {
			cur = styleKey.Render("▸ ")
		}
		fmt.Fprintf(&b, "%s%s  %s  %s  %s", cur, styleKey.Render(j.GetName()), j.GetDistro(), state,
			(time.Duration(j.GetDurationMs()) * time.Millisecond).Round(10*time.Millisecond))
		if e := lastLine(j.GetError()); e != "" {
			b.WriteString("  " + e)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + helpBarWrap(c.width, "↑↓", "job", "l", "job logs", "L", "all logs", "R", "rerun", "d", "files", "x", "cancel", "r", "refresh", "esc", "back"))
	return b.String()
}
