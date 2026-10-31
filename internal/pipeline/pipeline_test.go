package pipeline

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

func TestParse(t *testing.T) {
	p, err := Parse([]byte(`
jobs:
  test:
    needs: [build, lint]
    distros: [ubuntu-24.04, debian-13]
    run: make test
    timeout: 20m
  build:
    distro: ubuntu-24.04
    run: make build
    setup: ci/setup.sh
    key-files: [go.sum]
    env: {CGO_ENABLED: "0"}
  lint:
    distro: alpine-3.22
    run: make lint
apply: deploy/anvil.yaml
`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Order(), []string{"build", "lint", "test"}) {
		t.Errorf("order = %v", p.Order())
	}
	if j := p.Jobs["build"]; !slices.Equal(j.Distros, []string{"ubuntu-24.04"}) || j.timeout != DefaultTimeout {
		t.Errorf("build = %+v", j)
	}
	if p.Jobs["test"].timeout != 20*time.Minute || p.Apply != "deploy/anvil.yaml" {
		t.Errorf("test timeout %v, apply %q", p.Jobs["test"].timeout, p.Apply)
	}
	for name, bad := range map[string]string{
		"empty":         `jobs: {}`,
		"unknown field": "jobs:\n  a: {distro: x, run: y, image: z}",
		"no distro":     "jobs:\n  a: {run: y}",
		"both distros":  "jobs:\n  a: {distro: x, distros: [y], run: y}",
		"repeated":      "jobs:\n  a: {distros: [x, x], run: y}",
		"no run":        "jobs:\n  a: {distro: x}",
		"unknown need":  "jobs:\n  a: {distro: x, run: y, needs: [b]}",
		"cycle":         "jobs:\n  a: {distro: x, run: y, needs: [b]}\n  b: {distro: x, run: y, needs: [a]}",
		"escape":        "jobs:\n  a: {distro: x, run: y, setup: ../s.sh}",
		"key no setup":  "jobs:\n  a: {distro: x, run: y, key-files: [go.sum]}",
		"timeout":       "jobs:\n  a: {distro: x, run: y, timeout: soon}",
		"apply abs":     "jobs:\n  a: {distro: x, run: y}\napply: /etc/anvil.yaml",
		"job name":      "jobs:\n  a/b: {distro: x, run: y}",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// fakeRuns fails a distro whose name starts with "bad", and records each request.
type fakeRuns struct {
	mu    sync.Mutex
	reqs  []ephemeral.RunRequest
	order []string // the command of each run, as it started
	block chan struct{}
}

func (f *fakeRuns) Run(ctx context.Context, r ephemeral.RunRequest, emit func(ephemeral.Event)) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.order = append(f.order, r.Argv[2])
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	for _, d := range r.Distros {
		emit(ephemeral.Event{Progress: &ephemeral.Progress{CloneID: "eph-" + d, Distro: d, Message: "running"}})
		emit(ephemeral.Event{Output: &ephemeral.Output{Distro: d, Data: []byte("out of " + r.Argv[2] + " on " + d + "\n")}})
		for i := range r.Collect {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "result.txt", Mode: 0o644, Size: int64(len(d))})
			_, _ = tw.Write([]byte(d))
			_ = tw.Close()
			emit(ephemeral.Event{Collect: &ephemeral.Collect{CloneID: "eph-" + d, Distro: d, Index: i, Data: b.Bytes()}})
			emit(ephemeral.Event{Collect: &ephemeral.Collect{CloneID: "eph-" + d, Distro: d, Index: i, Done: true}})
		}
		code := 0
		if strings.HasPrefix(d, "bad") || ctx.Err() != nil {
			code = 1
		}
		emit(ephemeral.Event{Exit: &ephemeral.Exit{CloneID: "eph-" + d, Distro: d, Code: code, Duration: time.Second}})
	}
	return nil
}

func newTestExecutor(t *testing.T, runs Runs, apply ApplyFunc) *Executor {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return NewExecutor(s, runs, filepath.Join(t.TempDir(), "runs"), apply)
}

// sourceTar packs files into a tar file for a run.
func sourceTar(t *testing.T, files map[string]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "src-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, data := range files {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write([]byte(data))
	}
	_ = tw.Close()
	f.Close()
	return f.Name()
}

func TestExecutorRun(t *testing.T) {
	runs := &fakeRuns{}
	var applied struct {
		uid  uint32
		data string
	}
	e := newTestExecutor(t, runs, func(_ context.Context, uid uint32, data []byte, progress func(string)) error {
		applied.uid, applied.data = uid, string(data)
		progress("launched web")
		return nil
	})
	src := sourceTar(t, map[string]string{
		"anvil-ci.yaml": `
jobs:
  build:
    distro: ubuntu
    run: build
    setup: ci/setup.sh
    key-files: [go.sum]
    env: {B: "2", A: "1"}
    collect: [dist]
  test:
    needs: [build]
    distros: [ubuntu, debian]
    run: test
apply: deploy/anvil.yaml
`,
		"ci/setup.sh":       "make deps",
		"go.sum":            "sums",
		"deploy/anvil.yaml": "intent: web",
	})
	var events []Event
	rec, err := e.Run(context.Background(), Source{Tar: src, Origin: "manual", Owner: 1000}, func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != "succeeded" || rec.Applied != "applied deploy/anvil.yaml" || len(rec.Jobs) != 3 {
		t.Fatalf("run = %+v", rec)
	}
	for _, j := range rec.Jobs {
		if j.State != "succeeded" || j.Duration != time.Second {
			t.Errorf("job %+v", j)
		}
	}
	if applied.uid != 1000 || applied.data != "intent: web" {
		t.Errorf("applied = %+v", applied)
	}
	if !slices.Equal(runs.order, []string{"build", "test"}) {
		t.Errorf("order = %v", runs.order)
	}
	b := runs.reqs[0]
	if b.Setup == nil || string(b.Setup.Script) != "make deps" || b.Setup.Files[0].Name != "go.sum" || !slices.Equal(b.Env, []string{"A=1", "B=2"}) ||
		b.Upload != filepath.Join(e.RunDir(rec.ID), "source.tar") {
		t.Errorf("build request = %+v", b)
	}
	if !slices.Equal(runs.reqs[1].Distros, []string{"ubuntu", "debian"}) {
		t.Errorf("test request = %+v", runs.reqs[1])
	}
	if got, _ := os.ReadFile(e.LogPath(rec.ID, "test", "debian")); string(got) != "out of test on debian\n" {
		t.Errorf("log = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(e.RunDir(rec.ID), "artifacts", "build", "ubuntu", "result.txt")); string(got) != "ubuntu" {
		t.Errorf("artifact = %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.RunDir(rec.ID), "src")); !os.IsNotExist(err) {
		t.Error("the unpacked source stayed")
	}
	if first, last := events[0], events[len(events)-1]; first.Run == nil || first.Run.State != "running" || last.Run == nil || last.Run.State != "succeeded" {
		t.Errorf("first and last events = %+v, %+v", first, last)
	}
	if got, _ := e.Store.GetPipelineRun(rec.ID); got.State != "succeeded" || got.Owner != 1000 {
		t.Errorf("stored = %+v", got)
	}
}

func TestExecutorFailures(t *testing.T) {
	applyCalls := 0
	e := newTestExecutor(t, &fakeRuns{}, func(context.Context, uint32, []byte, func(string)) error { applyCalls++; return nil })
	src := sourceTar(t, map[string]string{"ci/p.yaml": `
jobs:
  build: {distros: [ubuntu, bad-debian], run: build}
  test: {needs: [build], distro: ubuntu, run: test}
  lint: {distro: ubuntu, run: lint}
apply: deploy.yaml
`})
	rec, err := e.Run(context.Background(), Source{Tar: src, File: "ci/p.yaml", Origin: "manual"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, j := range rec.Jobs {
		states[j.Name+"@"+j.Distro] = j.State
	}
	want := map[string]string{"build@ubuntu": "succeeded", "build@bad-debian": "failed", "test@ubuntu": "skipped", "lint@ubuntu": "succeeded"}
	if rec.State != "failed" || !mapsEqual(states, want) || applyCalls != 0 {
		t.Errorf("run %s, states %v, %d applies", rec.State, states, applyCalls)
	}

	// A missing or broken pipeline file fails the run, with a record.
	for _, files := range []map[string]string{{"other": "x"}, {"anvil-ci.yaml": "jobs: [1]"}} {
		rec, err := e.Run(context.Background(), Source{Tar: sourceTar(t, files), Origin: "manual"}, nil)
		if err != nil || rec.State != "failed" || rec.Error == "" {
			t.Errorf("bad source %v: %+v, %v", files, rec, err)
		}
	}
	if _, err := e.Run(context.Background(), Source{Tar: sourceTar(t, nil), File: "../x"}, nil); err == nil {
		t.Error("a pipeline file outside the source was accepted")
	}

	// A failed apply fails the run.
	e.Apply = func(context.Context, uint32, []byte, func(string)) error { return errors.New("path not owned") }
	rec, _ = e.Run(context.Background(), Source{Tar: sourceTar(t, map[string]string{"anvil-ci.yaml": "jobs:\n  a: {distro: u, run: x}\napply: d.yaml", "d.yaml": "x"})}, nil)
	if rec.State != "failed" || !strings.Contains(rec.Applied, "path not owned") {
		t.Errorf("failed apply: %+v", rec)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestExecutorCancel(t *testing.T) {
	runs := &fakeRuns{block: make(chan struct{})}
	e := newTestExecutor(t, runs, nil)
	src := sourceTar(t, map[string]string{"anvil-ci.yaml": "jobs:\n  a: {distro: u, run: x}\n  b: {distro: u, run: y, needs: [a]}"})
	done := make(chan store.PipelineRun)
	var id string
	var once sync.Once
	started := make(chan struct{})
	go func() {
		rec, _ := e.Run(context.Background(), Source{Tar: src}, func(ev Event) {
			if ev.Run != nil && ev.Run.State == "running" {
				id = ev.Run.ID
				once.Do(func() { close(started) })
			}
		})
		done <- rec
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	if err := e.Cancel(id); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if rec.State != "cancelled" || len(runs.reqs) != 1 {
		t.Errorf("cancelled run = %+v, %d runs", rec, len(runs.reqs))
	}
	if err := e.Cancel(id); err == nil {
		t.Error("cancelling a finished run worked")
	}

	// A run an earlier anvild left running is failed at start.
	_ = e.Store.PutPipelineRun(store.PipelineRun{ID: "01OLD", State: "running", Jobs: []store.PipelineJob{{Name: "a", State: "running"}}})
	e.MarkInterrupted()
	if got, _ := e.Store.GetPipelineRun("01OLD"); got.State != "failed" || got.Jobs[0].State != "failed" {
		t.Errorf("interrupted = %+v", got)
	}
}

func TestNormalizeWatch(t *testing.T) {
	w := store.PipelineWatch{Name: "app", Repo: "git@code.example.com:acme/app.git"}
	if err := Normalize(&w); err != nil || w.Branch != "main" || w.File != DefaultFile || w.Interval != DefaultInterval {
		t.Errorf("Normalize = %+v, %v", w, err)
	}
	for _, repo := range []string{"https://x/y.git", "ssh://git@x/y", "/srv/git/app.git"} {
		w := store.PipelineWatch{Name: "a", Repo: repo}
		if err := Normalize(&w); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for _, bad := range []store.PipelineWatch{
		{Name: "a b", Repo: "https://x"},
		{Name: "a", Repo: "-uhack"},
		{Name: "a", Repo: "ext::sh -c touch% /tmp/x"},
		{Name: "a", Repo: "relative/path"},
		{Name: "a", Repo: "https://x", Branch: "-x"},
		{Name: "a", Repo: "https://x", Branch: "a/../b"},
		{Name: "a", Repo: "https://x", File: "/etc/passwd"},
		{Name: "a", Repo: "https://x", Interval: time.Second},
	} {
		b := bad
		if err := Normalize(&b); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if IsRemote("/srv/x") || !IsRemote("git://x/y") {
		t.Error("IsRemote")
	}
}

// TestWatcher runs a watch against a local git repository.
func TestWatcher(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@x", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(repo, "anvil-ci.yaml"), []byte("jobs:\n  a: {distro: u, run: first}\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "one")

	runs := &fakeRuns{}
	e := newTestExecutor(t, runs, nil)
	w := NewWatcher(e, t.TempDir())
	w.Tick = 10 * time.Millisecond
	pw := store.PipelineWatch{Name: "app", Repo: repo, Interval: minInterval, Owner: 1000}
	if err := Normalize(&pw); err != nil {
		t.Fatal(err)
	}
	_ = e.Store.PutPipelineWatch(pw)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	waitRuns := func(n int) []store.PipelineRun {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			list, _ := e.Store.ListPipelineRuns()
			if len(list) >= n && list[0].State != "running" {
				return list
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %d runs, have %+v", n, list)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	list := waitRuns(1)
	if list[0].Source != "watch:app" || list[0].State != "succeeded" || len(list[0].Commit) != 40 || list[0].Owner != 1000 {
		t.Errorf("first run = %+v", list[0])
	}
	// No new commit, no new run, until a trigger.
	time.Sleep(50 * time.Millisecond)
	w.Wait()
	if l, _ := e.Store.ListPipelineRuns(); len(l) != 1 {
		t.Errorf("%d runs without a new commit", len(l))
	}
	_ = os.WriteFile(filepath.Join(repo, "anvil-ci.yaml"), []byte("jobs:\n  a: {distro: u, run: second}\n"), 0o644)
	git("commit", "-q", "-am", "two")
	if err := w.Trigger(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	list = waitRuns(2)
	if list[0].Commit == list[1].Commit || runs.order[len(runs.order)-1] != "second" {
		t.Errorf("second run = %+v, commands %v", list[0], runs.order)
	}
	got, _ := e.Store.GetPipelineWatch("app")
	if got.LastCommit != list[0].Commit || got.LastRun != list[0].ID || got.LastError != "" {
		t.Errorf("watch = %+v", got)
	}

	// A broken repo shows as the watch's error.
	bad := store.PipelineWatch{Name: "bad", Repo: filepath.Join(repo, "missing"), Interval: minInterval}
	_ = Normalize(&bad)
	_ = e.Store.PutPipelineWatch(bad)
	if err := w.Trigger(ctx, "bad"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := e.Store.GetPipelineWatch("bad"); got.LastError != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got, _ := e.Store.GetPipelineWatch("bad"); !strings.Contains(got.LastError, "ls-remote") {
		t.Errorf("bad watch error = %q", got.LastError)
	}
	cancel()
	w.Wait()
}
