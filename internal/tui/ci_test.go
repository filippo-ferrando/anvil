package tui

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestCIScreen(t *testing.T) {
	if !slices.Contains(screenOrder, screenCI) || screenLabels[screenCI] != "CI" {
		t.Fatal("the CI screen isn't in the sidebar")
	}
	m := model{screen: screenCI, ci: newCIModel()}
	m.ci.setSize(100, 30)
	now := time.Now().Unix()
	next, _ := m.updateCI(ciLoadedMsg{
		runners: []*anvilv1.Runner{{Name: "ci", Forge: "forgejo", Scope: "orgs/acme", Labels: map[string]string{"b": "2", "a": "1"}, MaxJobs: 2, Running: 1}},
		jobs:    []*anvilv1.RunnerJob{{Runner: "ci", JobId: 7, Name: "test", State: "failed", Distro: "debian-13", Error: "boom\nexit 1", StartedAtUnix: now - 60, EndedAtUnix: now}},
		runs: []*anvilv1.PipelineRun{{Id: "01RUN", Source: "watch:app", State: "failed", Commit: "0123456789abcdef", StartedAtUnix: now - 30, EndedAtUnix: now,
			Jobs: []*anvilv1.PipelineJob{{Name: "build", Distro: "u", State: "succeeded"}, {Name: "test", Distro: "u", State: "failed", ExitCode: 2, Error: "tests failed"}}}},
		watches: []*anvilv1.PipelineWatch{{Name: "app", Repo: "https://x/app", Branch: "main", IntervalSeconds: 60, LastRun: "01RUN"}},
	})
	m = next.(model)
	if v := m.ci.View(); !strings.Contains(v, "1/2 jobs") || !strings.Contains(v, "a=1,b=2") || !strings.Contains(v, "pause/resume") {
		t.Errorf("runners view:\n%s", v)
	}
	press := func(k string) {
		key := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		switch k {
		case "tab":
			key = tea.KeyMsg{Type: tea.KeyTab}
		case "enter":
			key = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			key = tea.KeyMsg{Type: tea.KeyEsc}
		}
		next, _ := m.updateCI(key)
		m = next.(model)
	}
	press("tab")
	if v := m.ci.View(); !strings.Contains(v, "job 7") || !strings.Contains(v, "exit 1") {
		t.Errorf("jobs view:\n%s", v)
	}
	press("tab")
	press("enter")
	if m.ci.detail == nil {
		t.Fatal("enter didn't open the run")
	}
	if v := m.ci.View(); !strings.Contains(v, "Run 01RUN") || !strings.Contains(v, "exit 2") || !strings.Contains(v, "tests failed") {
		t.Errorf("run view:\n%s", v)
	}
	press("esc")
	press("tab")
	if m.ci.detail != nil || m.ci.tab != ciWatches || !strings.Contains(m.ci.View(), "last run 01RUN") {
		t.Errorf("watches view:\n%s", m.ci.View())
	}
	press("tab")
	if m.ci.tab != ciRunners {
		t.Errorf("tab didn't wrap: %d", m.ci.tab)
	}
}

func ciKey(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func TestBuildRunnerRequest(t *testing.T) {
	f := newRunnerForm()
	setField(&f, "Name", "ci")
	setField(&f, "Scope", "orgs/acme")
	setField(&f, "Labels", "debian=debian-13, ubuntu=@abc")
	setField(&f, "Max jobs", "4")
	setField(&f, "Memory (MiB)", "4096")
	if _, err := buildRunnerRequest(f, ""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("no token: %v", err)
	}
	req, err := buildRunnerRequest(f, " envtok ")
	if err != nil {
		t.Fatal(err)
	}
	r := req.GetRunner()
	if req.GetToken() != "envtok" || r.GetForge() != "forgejo" || r.GetMaxJobs() != 4 || r.GetMemoryMib() != 4096 ||
		r.GetLabels()["debian"] != "debian-13" || r.GetLabels()["ubuntu"] != "@abc" {
		t.Errorf("got %+v token %q", r, req.GetToken())
	}

	file := t.TempDir() + "/tok"
	if err := os.WriteFile(file, []byte("filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setField(&f, "Token file", file)
	if req, _ := buildRunnerRequest(f, "envtok"); req.GetToken() != "filetok" {
		t.Errorf("token file not used: %q", req.GetToken())
	}
	setField(&f, "Token", "typed")
	if req, _ := buildRunnerRequest(f, "envtok"); req.GetToken() != "typed" {
		t.Errorf("typed token not used: %q", req.GetToken())
	}
	setField(&f, "Labels", "nodistro")
	if _, err := buildRunnerRequest(f, "x"); err == nil {
		t.Error("expected a bad label to fail")
	}
}

func TestBuildWatch(t *testing.T) {
	f := newWatchForm()
	setField(&f, "Name", "app")
	if _, err := buildWatch(f); err == nil {
		t.Error("expected a missing repo to fail")
	}
	setField(&f, "Repo", "https://x/app")
	setField(&f, "Interval", "5m")
	w, err := buildWatch(f)
	if err != nil || w.GetIntervalSeconds() != 300 || w.GetRepo() != "https://x/app" {
		t.Errorf("got %+v, %v", w, err)
	}
	setField(&f, "Interval", "often")
	if _, err := buildWatch(f); err == nil {
		t.Error("expected a bad interval to fail")
	}
}

func TestCIFormsAndConfirm(t *testing.T) {
	m := model{screen: screenCI, ci: newCIModel(), height: 40}
	m.ci.setSize(100, 30)
	next, _ := m.updateCI(ciLoadedMsg{
		runners: []*anvilv1.Runner{{Name: "ci"}},
		runs:    []*anvilv1.PipelineRun{{Id: "01RUN", State: "succeeded", Jobs: []*anvilv1.PipelineJob{{Name: "a", Distro: "u"}, {Name: "b", Distro: "u"}}}},
		watches: []*anvilv1.PipelineWatch{{Name: "app"}},
	})
	m = next.(model)
	press := func(k string) tea.Cmd {
		next, cmd := m.updateCI(ciKey(k))
		m = next.(model)
		return cmd
	}

	press("a")
	if m.ci.form == nil || m.ci.formKind != "runner" || !strings.Contains(m.ci.View(), "Add a runner") {
		t.Fatalf("a on runners: %q\n%s", m.ci.formKind, m.ci.View())
	}
	press("esc")
	press("x")
	if m.ci.confirm != "ci" || !strings.Contains(m.ci.View(), "Remove runner ci") {
		t.Fatalf("x on runners:\n%s", m.ci.View())
	}
	if press("n") != nil || m.ci.confirm != "" {
		t.Error("any other key should cancel the remove")
	}

	m.ci.tab = ciWatches
	press("a")
	if m.ci.formKind != "watch" {
		t.Errorf("a on watches opened %q", m.ci.formKind)
	}
	press("esc")
	press("x")
	if !strings.Contains(m.ci.View(), "Remove watch app") {
		t.Errorf("x on watches:\n%s", m.ci.View())
	}
	press("esc")

	m.ci.tab = ciRuns
	press("n")
	if m.ci.formKind != "run" {
		t.Errorf("n on runs opened %q", m.ci.formKind)
	}
	press("esc")
	press("d")
	if m.ci.formKind != "artifacts" || m.ci.formRun != "01RUN" {
		t.Errorf("d on a run opened %q for %q", m.ci.formKind, m.ci.formRun)
	}
	press("esc")

	press("enter")
	press("down")
	press("down")
	if m.ci.jobCursor != 1 {
		t.Errorf("job cursor %d, want 1 (the last job)", m.ci.jobCursor)
	}
	if press("l") == nil || m.ci.logs == nil || !strings.Contains(m.ci.logs.title, "b@u") {
		t.Fatalf("l in a run didn't open the job's logs: %+v", m.ci.logs)
	}
	next, _ = m.updateCI(ciLogsMsg{text: "hello from b\n"})
	m = next.(model)
	if v := m.ci.View(); !strings.Contains(v, "hello from b") {
		t.Errorf("logs view:\n%s", v)
	}
	press("esc")
	if m.ci.logs != nil || m.ci.detail == nil {
		t.Error("esc should close the logs and go back to the run")
	}
}

func TestPipelineRunView(t *testing.T) {
	m := model{screen: screenCI, ci: newCIModel()}
	m.ci.setSize(100, 30)
	cancelled := false
	m.ci.run = newRunView(nil, func() { cancelled = true })
	feed := func(ev *anvilv1.PipelineEvent, done bool) {
		next, _ := m.updateCI(pipelineEventMsg{ev: ev, done: done})
		m = next.(model)
	}
	run := &anvilv1.PipelineRun{Id: "01RUN", State: "running", Jobs: []*anvilv1.PipelineJob{{Name: "build", Distro: "debian-13"}}}
	feed(&anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Run{Run: run}}, false)
	feed(&anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Output{Output: &anvilv1.PipelineOutput{Job: "build", Distro: "debian-13", Data: []byte("compiling\n")}}}, false)
	feed(&anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Job{Job: &anvilv1.PipelineJob{Name: "build", Distro: "debian-13", State: "failed", ExitCode: 3}}}, false)
	v := m.ci.View()
	if !strings.Contains(v, "build@debian-13") || !strings.Contains(v, "compiling") || !strings.Contains(v, "exit 3") {
		t.Errorf("run view:\n%s", v)
	}
	press := func(k string) {
		next, _ := m.updateCI(ciKey(k))
		m = next.(model)
	}
	press("esc")
	if !cancelled || m.ci.run == nil {
		t.Error("esc on a running run should cancel it and keep the view")
	}
	done := &anvilv1.PipelineRun{Id: "01RUN", State: "failed", Error: "build failed"}
	feed(&anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Run{Run: done}}, true)
	if v := m.ci.View(); !strings.Contains(v, "finished") || !strings.Contains(v, "run 01RUN failed: build failed") {
		t.Errorf("finished view:\n%s", v)
	}
	press("esc")
	if m.ci.run != nil {
		t.Error("esc on a finished run should close it")
	}
}
