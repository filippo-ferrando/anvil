package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// fakeForgejo serves the waiting-jobs and runner registration API of one repo.
type fakeForgejo struct {
	mu       sync.Mutex
	jobs     string // the JSON body of the jobs list
	regs     []map[string]any
	deleted  []string
	gotLabel string
	auth     string
}

func (f *fakeForgejo) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/acme/app/actions/runners/jobs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gotLabel, f.auth = r.URL.Query().Get("labels"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(f.jobs))
	})
	mux.HandleFunc("POST /api/v1/repos/acme/app/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.regs = append(f.regs, body)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":31,"uuid":"uuid-1","token":"secret-token"}`))
	})
	mux.HandleFunc("DELETE /api/v1/repos/acme/app/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
	})
	return mux
}

func TestForgejoClient(t *testing.T) {
	ff := &fakeForgejo{jobs: `[{"id":7,"name":"test","handle":"h-7","runs_on":["debian"],"status":"waiting"},{"id":8,"runs_on":["debian"],"status":"running"}]`}
	srv := httptest.NewServer(ff.handler(t))
	defer srv.Close()
	f, err := newForge(store.Runner{Forge: "forgejo", URL: srv.URL + "/", Scope: "repos/acme/app", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := f.Waiting(context.Background(), []string{"debian", "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 7 || jobs[0].Handle != "h-7" || !slices.Equal(jobs[0].Labels, []string{"debian"}) {
		t.Errorf("jobs = %+v", jobs)
	}
	if ff.gotLabel != "debian,ubuntu" || ff.auth != "token tok" {
		t.Errorf("query labels %q, auth %q", ff.gotLabel, ff.auth)
	}
	for _, body := range []string{`{"jobs":[{"id":9,"runs_on":["x"]}]}`, `null`} {
		ff.jobs = body
		if jobs, err := f.Waiting(context.Background(), []string{"x"}); err != nil || (body != "null" && (len(jobs) != 1 || jobs[0].ID != 9)) {
			t.Errorf("jobs from %s = %+v, %v", body, jobs, err)
		}
	}

	launch, err := f.Register(context.Background(), "anvil-r-7", Job{ID: 7, Labels: []string{"debian"}, Handle: "h-7"})
	if err != nil {
		t.Fatal(err)
	}
	if reg := ff.regs[0]; reg["ephemeral"] != true || reg["name"] != "anvil-r-7" {
		t.Errorf("registration = %v", reg)
	}
	for _, want := range []string{"forgejo-runner one-job", "--label 'debian:host'", `--handle "$ANVIL_JOB_HANDLE"`, "--wait", "--token-url file:///tmp/anvil-runner-token"} {
		if !strings.Contains(launch.Script, want) {
			t.Errorf("script lacks %q:\n%s", want, launch.Script)
		}
	}
	if strings.Contains(launch.Script, "secret-token") || !slices.Contains(launch.Env, "ANVIL_RUNNER_TOKEN=secret-token") || !slices.Contains(launch.Env, "ANVIL_RUNNER_UUID=uuid-1") {
		t.Errorf("the token must go in the environment only: %q %q", launch.Script, launch.Env)
	}
	launch.Cleanup(context.Background())
	if !slices.Equal(ff.deleted, []string{"31"}) {
		t.Errorf("deleted = %v", ff.deleted)
	}
	if name, script := f.Install(""); name != "forgejo-runner latest" || !strings.Contains(script, "releases/latest") {
		t.Errorf("install = %q", name)
	}
	if name, script := f.Install("v12.6.0"); name != "forgejo-runner 12.6.0" || !strings.Contains(script, "v='12.6.0'") {
		t.Errorf("install pinned = %q\n%s", name, script)
	}
}

func TestGitHubClient(t *testing.T) {
	var jit map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/acme/app/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") != "queued" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "bad request", 400)
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"id":100}]}`))
	})
	mux.HandleFunc("GET /api/v3/repos/acme/app/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jobs":[
			{"id":1,"name":"ours","status":"queued","labels":["self-hosted","Linux","anvil-ubuntu"]},
			{"id":2,"name":"hosted","status":"queued","labels":["ubuntu-latest"]},
			{"id":3,"name":"generic","status":"queued","labels":["self-hosted","linux"]},
			{"id":4,"name":"started","status":"in_progress","labels":["anvil-ubuntu"]}]}`))
	})
	mux.HandleFunc("POST /api/v3/repos/acme/app/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&jit)
		_, _ = w.Write([]byte(`{"runner":{"id":55},"encoded_jit_config":"JITCFG"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	g, err := newForge(store.Runner{Forge: "github", URL: srv.URL, Scope: "repos/acme/app", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := g.Waiting(context.Background(), []string{"anvil-ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	launch, err := g.Register(context.Background(), "anvil-r-1", jobs[0])
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := jit["labels"].([]any)
	if jit["name"] != "anvil-r-1" || len(labels) != 3 || labels[2] != "anvil-ubuntu" {
		t.Errorf("jit request = %v", jit)
	}
	if !slices.Contains(launch.Env, "ANVIL_JIT_CONFIG=JITCFG") || strings.Contains(launch.Script, "JITCFG") || !strings.Contains(launch.Script, "run.sh --jitconfig") {
		t.Errorf("launch = %+v", launch)
	}
	if githubAPI("https://github.com/") != "https://api.github.com" || githubAPI("https://ghe.example") != "https://ghe.example/api/v3" {
		t.Error("githubAPI")
	}
	if _, err := newForge(store.Runner{Forge: "github", Scope: "orgs/acme"}); err == nil {
		t.Error("a github org scope was accepted")
	}
}

func TestNormalize(t *testing.T) {
	ok := store.Runner{Name: "ci", Forge: "gitea", URL: "https://code.example", Scope: "orgs/acme", Token: "t", Labels: map[string]string{"debian": "debian-13"}}
	if err := normalize(&ok); err != nil || ok.Forge != "forgejo" || ok.MaxJobs != DefaultMaxJobs || ok.CPUs != DefaultCPUs || ok.MemoryMiB != DefaultMemory {
		t.Errorf("normalize = %+v, %v", ok, err)
	}
	gh := store.Runner{Name: "gh", Forge: "github", Scope: "repos/a/b", Token: "t", Labels: map[string]string{"x": "alpine-3.22"}}
	if err := normalize(&gh); err != nil || gh.URL != "https://github.com" {
		t.Errorf("github defaults = %+v, %v", gh, err)
	}
	for _, mut := range []func(*store.Runner){
		func(r *store.Runner) { r.Name = "bad name" },
		func(r *store.Runner) { r.Forge = "gitlab" },
		func(r *store.Runner) { r.URL = "code.example" },
		func(r *store.Runner) { r.Token = "" },
		func(r *store.Runner) { r.Scope = "acme" },
		func(r *store.Runner) { r.Labels = nil },
		func(r *store.Runner) { r.Labels = map[string]string{"a:b": "x"} },
		func(r *store.Runner) { r.Forge, r.Scope = "github", "admin" },
	} {
		r := ok
		r.Labels = map[string]string{"debian": "debian-13"}
		mut(&r)
		if err := normalize(&r); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestPickDistro(t *testing.T) {
	m := map[string]string{"debian": "debian-13", "big": "@abc"}
	for _, tc := range []struct {
		labels []string
		want   string
	}{
		{[]string{"debian"}, "debian-13"},
		{[]string{"self-hosted", "big"}, "@abc"},
		{[]string{"debian", "gpu"}, ""},
		{[]string{"self-hosted"}, ""},
	} {
		if got := pickDistro(m, tc.labels); got != tc.want {
			t.Errorf("pickDistro(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

// fakeForge hands out jobs and counts registrations.
type fakeForge struct {
	mu      sync.Mutex
	jobs    []Job
	regs    atomic.Int32
	cleaned atomic.Int32
	regErr  error
}

func (f *fakeForge) Waiting(context.Context, []string) ([]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Job(nil), f.jobs...), nil
}

func (f *fakeForge) Register(_ context.Context, name string, job Job) (Launch, error) {
	if f.regErr != nil {
		return Launch{}, f.regErr
	}
	f.regs.Add(1)
	return Launch{Script: "run " + name, Env: []string{"TOKEN=x"}, Cleanup: func(context.Context) { f.cleaned.Add(1) }}, nil
}

func (f *fakeForge) Install(string) (string, string) { return "fake-runner", "install it" }

// fakeRuns records requests and exits with code, after release is closed.
type fakeRuns struct {
	mu      sync.Mutex
	reqs    []ephemeral.RunRequest
	code    int
	release chan struct{}
}

func (f *fakeRuns) Run(ctx context.Context, r ephemeral.RunRequest, emit func(ephemeral.Event)) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	emit(ephemeral.Event{Progress: &ephemeral.Progress{CloneID: "eph-1", Distro: r.Distros[0], Message: "restored"}})
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	emit(ephemeral.Event{Output: &ephemeral.Output{Data: []byte("runner output\n")}})
	emit(ephemeral.Event{Exit: &ephemeral.Exit{Distro: r.Distros[0], Code: f.code}})
	return nil
}

func newTestManager(t *testing.T, forge Forge, runs Runs) *Manager {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	m := NewManager(s, runs)
	m.Poll = 20 * time.Millisecond
	m.newForge = func(store.Runner) (Forge, error) { return forge, nil }
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagerRunsJobs(t *testing.T) {
	forge := &fakeForge{jobs: []Job{{ID: 1, Name: "a", Labels: []string{"debian"}}, {ID: 2, Name: "b", Labels: []string{"debian"}},
		{ID: 3, Name: "c", Labels: []string{"debian"}}, {ID: 4, Name: "other", Labels: []string{"gpu"}}}}
	runs := &fakeRuns{release: make(chan struct{}), code: 0}
	m := newTestManager(t, forge, runs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	info, err := m.Add(ctx, store.Runner{Name: "ci", Forge: "forgejo", URL: "https://x", Scope: "repos/a/b", Token: "secret",
		Labels: map[string]string{"debian": "debian-13"}, MaxJobs: 2})
	if err != nil || info.Token != "" {
		t.Fatalf("Add = %+v, %v", info, err)
	}
	// Two at once, the gpu job never.
	waitFor(t, "two jobs", func() bool { list, _ := m.List(); return list[0].Running == 2 })
	time.Sleep(60 * time.Millisecond)
	if n := forge.regs.Load(); n != 2 {
		t.Errorf("%d registrations with max 2 jobs", n)
	}
	runs.mu.Lock()
	r := runs.reqs[0]
	runs.mu.Unlock()
	if !slices.Equal(r.Distros, []string{"debian-13"}) || r.Setup == nil || r.Setup.Name != "fake-runner" || r.CPUs != DefaultCPUs || !slices.Contains(r.Env, "TOKEN=x") {
		t.Errorf("run request = %+v", r)
	}
	// Jobs done: the forge stops listing them, and they aren't started again.
	forge.mu.Lock()
	forge.jobs = forge.jobs[2:]
	forge.mu.Unlock()
	close(runs.release)
	waitFor(t, "all jobs", func() bool {
		jobs, _ := m.Jobs("ci")
		done := 0
		for _, j := range jobs {
			if j.State == "succeeded" {
				done++
			}
		}
		return done == 3
	})
	jobs, _ := m.Jobs("ci")
	if jobs[0].CloneID != "eph-1" || jobs[0].Distro != "debian-13" {
		t.Errorf("job record = %+v", jobs[0])
	}
	if forge.cleaned.Load() != 3 {
		t.Errorf("%d cleanups for 3 jobs", forge.cleaned.Load())
	}

	// Paused: nothing new starts.
	if _, err := m.SetPaused("ci", true); err != nil {
		t.Fatal(err)
	}
	forge.mu.Lock()
	forge.jobs = []Job{{ID: 10, Labels: []string{"debian"}}}
	forge.mu.Unlock()
	time.Sleep(80 * time.Millisecond)
	if forge.regs.Load() != 3 {
		t.Errorf("a paused runner registered: %d", forge.regs.Load())
	}
	if err := m.Remove("ci"); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(); len(list) != 0 {
		t.Errorf("after Remove: %+v", list)
	}
	if jobs, _ := m.Jobs("ci"); len(jobs) != 0 {
		t.Errorf("history after Remove: %d", len(jobs))
	}
	m.Wait()
}

func TestManagerFailures(t *testing.T) {
	forge := &fakeForge{jobs: []Job{{ID: 1, Labels: []string{"big"}}}}
	runs := &fakeRuns{release: make(chan struct{}), code: 3}
	close(runs.release)
	m := newTestManager(t, forge, runs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := m.Add(ctx, store.Runner{Name: "ci", Forge: "github", Scope: "repos/a/b", Token: "t", Labels: map[string]string{"big": "@k1"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a failed job", func() bool { jobs, _ := m.Jobs("ci"); return len(jobs) > 0 && jobs[0].State == "failed" })
	jobs, _ := m.Jobs("ci")
	if jobs[0].ExitCode != 3 || !strings.Contains(jobs[0].Error, "runner output") {
		t.Errorf("failed job = %+v", jobs[0])
	}
	runs.mu.Lock()
	if runs.reqs[0].Setup != nil {
		t.Error("a warm snapshot by key got the install setup")
	}
	runs.mu.Unlock()
	if _, err := m.Add(ctx, store.Runner{Name: "ci", Forge: "github", Scope: "repos/a/b", Token: "t", Labels: map[string]string{"big": "@k1"}}); err == nil {
		t.Error("a second runner named ci was accepted")
	}
	cancel()
	m.Wait()

	// A registration error fails the job without a clone.
	forge2 := &fakeForge{jobs: []Job{{ID: 5, Labels: []string{"x"}}}, regErr: errors.New("403 forbidden")}
	m2 := newTestManager(t, forge2, &fakeRuns{release: make(chan struct{})})
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if _, err := m2.Add(ctx2, store.Runner{Name: "r2", Forge: "forgejo", URL: "https://x", Scope: "admin", Token: "t", Labels: map[string]string{"x": "alpine-3.22"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a registration failure", func() bool { jobs, _ := m2.Jobs("r2"); return len(jobs) > 0 && jobs[0].State == "failed" })
	if jobs, _ := m2.Jobs("r2"); !strings.Contains(jobs[0].Error, "403") {
		t.Errorf("job = %+v", jobs[0])
	}
	cancel2()
	m2.Wait()

	// Jobs an earlier anvild left running are failed at start.
	_ = m2.Store.PutRunnerJob(store.RunnerJob{ID: "r2/9/1", Runner: "r2", State: "running", StartedAt: time.Now()})
	m2.MarkInterrupted()
	if jobs, _ := m2.Jobs("r2"); jobs[0].State != "failed" || !strings.Contains(jobs[0].Error, "restarted") {
		t.Errorf("interrupted job = %+v", jobs[0])
	}
}
