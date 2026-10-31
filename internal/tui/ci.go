package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
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
	tab    int
	lists  [ciTabs]list.Model
	detail *anvilv1.PipelineRun // a run's jobs on screen, nil for the lists
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
	for i := range c.lists {
		c.lists[i].SetSize(w, h)
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

func (m model) updateCIKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	ci := &m.ci
	if ci.detail != nil {
		switch msg.String() {
		case "esc", "q", "enter":
			ci.detail = nil
		case "r":
			return m, loadCI(m.client)
		case "x":
			return m, cancelPipelineRun(m.client, ci.detail.GetId())
		}
		return m, nil
	}
	l := &ci.lists[ci.tab]
	switch msg.String() {
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
	case "enter":
		if it, ok := l.SelectedItem().(pipelineRunItem); ok {
			ci.detail = it.r
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
		if it, ok := l.SelectedItem().(pipelineRunItem); ok && it.r.GetState() == "running" {
			return m, cancelPipelineRun(m.client, it.r.GetId())
		}
		return m, nil
	}
	var cmd tea.Cmd
	*l, cmd = l.Update(msg)
	return m, cmd
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

func (c ciModel) View() string {
	if r := c.detail; r != nil {
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
		for _, j := range r.GetJobs() {
			state := j.GetState()
			switch state {
			case "succeeded":
				state = styleGood.Render(state)
			case "failed":
				state = styleError.Render(fmt.Sprintf("%s (exit %d)", state, j.GetExitCode()))
			case "skipped":
				state = styleWarn.Render(state)
			}
			fmt.Fprintf(&b, "  %s  %s  %s  %s", styleKey.Render(j.GetName()), j.GetDistro(), state,
				(time.Duration(j.GetDurationMs()) * time.Millisecond).Round(10*time.Millisecond))
			if e := lastLine(j.GetError()); e != "" {
				b.WriteString("  " + e)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n" + helpBar("x", "cancel", "r", "refresh", "esc", "back") + "\n")
		b.WriteString(styleSubtitle.Render("logs: anvil pipeline logs " + r.GetId()))
		return b.String()
	}
	keys := []string{"tab", "next list", "r", "refresh", "esc", "back"}
	switch c.tab {
	case ciRunners:
		keys = append([]string{"p", "pause/resume"}, keys...)
	case ciRuns:
		keys = append([]string{"enter", "jobs", "x", "cancel"}, keys...)
	case ciWatches:
		keys = append([]string{"t", "trigger", "p", "pause/resume"}, keys...)
	}
	return c.lists[c.tab].View() + "\n" + helpBar(keys...)
}
