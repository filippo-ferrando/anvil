package tui

import (
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
