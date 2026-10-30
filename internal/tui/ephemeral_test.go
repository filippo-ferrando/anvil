package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestBuildRunRequest(t *testing.T) {
	f := newRunForm()
	if _, _, err := buildRunRequest(f); err == nil {
		t.Error("an empty run form was accepted")
	}
	setField(&f, "Distros", " ubuntu-24.04, debian-13 ,")
	setField(&f, "Command", "make test")
	setField(&f, "Memory (MiB)", "1024")
	toggle(&f, "Keep failed")
	req, path, err := buildRunRequest(f)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(req.GetDistros(), []string{"ubuntu-24.04", "debian-13"}) || !slices.Equal(req.GetCommand(), []string{"sh", "-c", "make test"}) ||
		req.GetMemoryMib() != 1024 || req.GetCpus() != 0 || !req.GetKeepFailed() || req.GetUpload() || path != "" {
		t.Errorf("request = %+v, path %q", req, path)
	}
	dir := t.TempDir()
	setField(&f, "Path", dir)
	if req, path, err := buildRunRequest(f); err != nil || !req.GetUpload() || path != dir {
		t.Errorf("with a path = %+v, %q, %v", req, path, err)
	}
	setField(&f, "Path", dir+"/missing")
	if _, _, err := buildRunRequest(f); err == nil {
		t.Error("a missing path was accepted")
	}
	setField(&f, "Path", "")
	setField(&f, "CPUs", "two")
	if _, _, err := buildRunRequest(f); err == nil {
		t.Error("CPUs \"two\" was accepted")
	}

	w := newWarmForm()
	setField(&w, "Distro", "alpine")
	if req, err := buildWarmRequest(w); err != nil || req.GetDistro() != "alpine" || req.GetCpus() != 0 {
		t.Errorf("warm request = %+v, %v", req, err)
	}
}

func TestRunViewPanes(t *testing.T) {
	v := newRunView([]string{"ubuntu", "alpine"}, func() {})
	out := func(d, data string) *anvilv1.RunEvent {
		return &anvilv1.RunEvent{Event: &anvilv1.RunEvent_Output{Output: &anvilv1.RunOutput{Distro: d, Data: []byte(data)}}}
	}
	v.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Progress{Progress: &anvilv1.RunProgress{Distro: "alpine", Message: "restoring a clone"}}})
	v.event(out("ubuntu", "one\ntw"))
	v.event(out("ubuntu", "o\nthr"))
	v.event(&anvilv1.RunEvent{Event: &anvilv1.RunEvent_Exit{Exit: &anvilv1.RunExit{Distro: "ubuntu", ExitCode: 2, Kept: true, CloneId: "eph-K"}}})
	if p := v.panes["ubuntu"]; !slices.Equal(p.lines, []string{"one", "two", "thr"}) || p.partial != "" || p.exit.GetExitCode() != 2 {
		t.Errorf("ubuntu pane = %+v", p)
	}
	view := v.view(80, 20)
	for _, want := range []string{"restoring a clone", "exit 2", "kept as eph-K", "two", "cancel"} {
		if !strings.Contains(view, want) {
			t.Errorf("run view lacks %q:\n%s", want, view)
		}
	}
	for i := 0; i < paneKeep+50; i++ {
		v.panes["alpine"].write([]byte("x\n"))
	}
	if n := len(v.panes["alpine"].lines); n != paneKeep {
		t.Errorf("a pane kept %d lines, want %d", n, paneKeep)
	}
}

func TestEphemeralScreenKeys(t *testing.T) {
	m := model{screen: screenEphemeral, ephemeral: newEphemeralModel()}
	next, _ := m.Update(warmLoadedMsg{snaps: []*anvilv1.WarmSnapshot{{Key: "abc123", Distro: "ubuntu-24.04", Cpus: 2}}})
	m = next.(model)
	next, _ = m.Update(clonesLoadedMsg{clones: []*anvilv1.EphemeralClone{{Id: "eph-1", Distro: "debian-13", Kept: true}}})
	m = next.(model)
	if len(m.ephemeral.warm.Items()) != 1 || len(m.ephemeral.clones.Items()) != 1 {
		t.Fatal("lists not filled")
	}
	next, _ = m.Update(key("x"))
	m = next.(model)
	if m.ephemeral.confirm != "abc123" {
		t.Errorf("x on a warm snapshot asks about %q", m.ephemeral.confirm)
	}
	next, _ = m.Update(key("n")) // any other key cancels the question
	m = next.(model)
	next, _ = m.Update(key("\t"))
	m = next.(model)
	if m.ephemeral.confirm != "" || m.ephemeral.form != nil {
		t.Errorf("after cancelling: confirm %q, form %v", m.ephemeral.confirm, m.ephemeral.form != nil)
	}
	m.ephemeral.showClones = true
	next, _ = m.Update(key("x"))
	m = next.(model)
	if m.ephemeral.confirm != "eph-1" {
		t.Errorf("x on a clone asks about %q", m.ephemeral.confirm)
	}
	if !slices.Contains(screenOrder, screenEphemeral) || screenLabels[screenEphemeral] == "" {
		t.Error("the Ephemeral screen is missing from the sidebar")
	}
}

func TestRunPaneSteps(t *testing.T) {
	p := &runPane{status: "waiting", since: time.Now()}
	p.setStatus("building the warm snapshot")
	p.since = p.since.Add(-5 * time.Second)
	p.setStatus("building the warm snapshot") // same step: no line yet
	p.setStatus("running: make")
	if len(p.lines) != 1 || p.lines[0] != "· building the warm snapshot  (5s)" {
		t.Fatalf("lines %q", p.lines)
	}
	p.setStatus("done") // quick step: nothing kept
	if len(p.lines) != 1 {
		t.Fatalf("lines %q", p.lines)
	}
}
