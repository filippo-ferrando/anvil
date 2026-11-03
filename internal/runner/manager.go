package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// ErrInvalid marks a runner refused for bad settings; ErrExists one whose name is taken.
var (
	ErrInvalid = errors.New("invalid runner")
	ErrExists  = errors.New("already exists")
)

// invalidError keeps err's message and matches ErrInvalid.
type invalidError struct{ err error }

func (e invalidError) Error() string        { return e.err.Error() }
func (e invalidError) Unwrap() error        { return e.err }
func (e invalidError) Is(target error) bool { return target == ErrInvalid }

// Store keeps runners and their jobs; *store.Store is one.
type Store interface {
	PutRunner(store.Runner) error
	GetRunner(name string) (store.Runner, error)
	ListRunners() ([]store.Runner, error)
	DeleteRunner(name string) error
	PutRunnerJob(store.RunnerJob) error
	ListRunnerJobs(runner string) ([]store.RunnerJob, error)
	DeleteRunnerJob(id string) error
}

// Runs runs commands in ephemeral clones; *ephemeral.Manager is one.
type Runs interface {
	Run(ctx context.Context, r ephemeral.RunRequest, emit func(ephemeral.Event)) error
}

// Defaults for a runner.
const (
	DefaultMaxJobs = 2
	DefaultCPUs    = 2
	DefaultMemory  = 2048
	// jobTimeout bounds one job's clone, the same as a GitHub-hosted job.
	jobTimeout = 6 * time.Hour
	// keepJobs is how many finished jobs each runner keeps in its history.
	keepJobs = 100
	// errTail is how much of a failed job's output its record keeps.
	errTail = 2 << 10
)

// Watchdog timings for runners whose forge reports which job they took; tests shorten them.
var (
	watchEvery = 30 * time.Second
	idleLimit  = 5 * time.Minute
	// watchLookups bounds the polls spent looking for the job a busy runner took.
	watchLookups = 10
)

// Manager polls every runner's forge and runs the jobs it finds.
type Manager struct {
	Store Store
	Runs  Runs
	// Poll is how often a runner asks its forge for jobs.
	Poll time.Duration
	// newForge makes a runner's forge client; tests replace it.
	newForge func(store.Runner) (Forge, error)

	mu    sync.Mutex
	loops map[string]*loop
	jobs  sync.WaitGroup
}

// loop is one runner's polling, and the jobs it started.
type loop struct {
	cancel   context.CancelFunc
	done     chan struct{} // closed when the polling goroutine returns
	jobs     sync.WaitGroup
	running  int           // clones now
	active   map[int64]int // forge job ids with a clone now, and how many
	recent   map[int64]time.Time
	lastPoll time.Time
	lastErr  string
}

// Info is a runner as listed: its record without the token, and how it is doing.
type Info struct {
	store.Runner
	Running  int
	LastPoll time.Time
	LastErr  string
}

func NewManager(s Store, runs Runs) *Manager {
	return &Manager{Store: s, Runs: runs, Poll: 10 * time.Second, newForge: newForge, loops: map[string]*loop{}}
}

// Start starts every stored runner's loop; they stop with ctx.
func (m *Manager) Start(ctx context.Context) error {
	list, err := m.Store.ListRunners()
	if err != nil {
		return err
	}
	for _, r := range list {
		m.start(ctx, r)
	}
	return nil
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// normalize checks r and fills its defaults.
func normalize(r *store.Runner) error {
	if !validName.MatchString(r.Name) {
		return fmt.Errorf("runner name %q: use letters, digits, '.', '_' and '-'", r.Name)
	}
	if r.Forge == "gitea" {
		r.Forge = "forgejo"
	}
	if r.Forge == "github" && r.URL == "" {
		r.URL = "https://github.com"
	}
	if r.URL == "" || !strings.HasPrefix(r.URL, "http") {
		return fmt.Errorf("runner %s: the forge URL must start with http:// or https://", r.Name)
	}
	if r.Token == "" {
		return fmt.Errorf("runner %s: an API token is needed", r.Name)
	}
	switch parts := strings.Split(strings.Trim(r.Scope, "/"), "/"); {
	case len(parts) == 3 && parts[0] == "repos" && parts[1] != "" && parts[2] != "":
	case r.Forge == "forgejo" && len(parts) == 2 && parts[0] == "orgs" && parts[1] != "":
	case r.Forge == "forgejo" && r.Scope == "admin":
	default:
		return fmt.Errorf("runner %s: scope %q, want repos/<owner>/<repo>%s", r.Name, r.Scope,
			map[bool]string{true: ", orgs/<org> or admin", false: ""}[r.Forge == "forgejo"])
	}
	if len(r.Labels) == 0 {
		return fmt.Errorf("runner %s: map at least one job label to a distro", r.Name)
	}
	for l, d := range r.Labels {
		if l == "" || d == "" || strings.ContainsAny(l, ", :") {
			return fmt.Errorf("runner %s: label %q -> %q, want <label>=<distro>", r.Name, l, d)
		}
	}
	deps, err := normalizeDeps(r.Deps)
	if err != nil {
		return fmt.Errorf("runner %s: %w", r.Name, err)
	}
	r.Deps = deps
	if r.MaxJobs <= 0 {
		r.MaxJobs = DefaultMaxJobs
	}
	if r.CPUs <= 0 {
		r.CPUs = DefaultCPUs
	}
	if r.MemoryMiB <= 0 {
		r.MemoryMiB = DefaultMemory
	}
	if _, err := newForge(*r); err != nil {
		return fmt.Errorf("runner %s: %w", r.Name, err)
	}
	return nil
}

// Add saves a new runner and starts it.
func (m *Manager) Add(ctx context.Context, r store.Runner) (Info, error) {
	if err := normalize(&r); err != nil {
		return Info{}, invalidError{err}
	}
	if _, err := m.Store.GetRunner(r.Name); err == nil {
		return Info{}, fmt.Errorf("a runner named %s %w", r.Name, ErrExists)
	}
	r.CreatedAt = time.Now()
	if err := m.Store.PutRunner(r); err != nil {
		return Info{}, err
	}
	m.start(ctx, r)
	return m.info(r), nil
}

// Remove stops a runner, cancelling its running jobs, and forgets it with its history.
func (m *Manager) Remove(name string) error {
	if _, err := m.Store.GetRunner(name); err != nil {
		return err
	}
	// Running jobs record how they ended, so the history goes only after they stop.
	m.stop(name)
	if err := m.Store.DeleteRunner(name); err != nil {
		return err
	}
	jobs, _ := m.Store.ListRunnerJobs(name)
	for _, j := range jobs {
		_ = m.Store.DeleteRunnerJob(j.ID)
	}
	return nil
}

// SetPaused stops or resumes taking new jobs; running jobs go on.
func (m *Manager) SetPaused(name string, paused bool) (Info, error) {
	r, err := m.Store.GetRunner(name)
	if err != nil {
		return Info{}, err
	}
	r.Paused = paused
	if err := m.Store.PutRunner(r); err != nil {
		return Info{}, err
	}
	return m.info(r), nil
}

// List returns every runner, by name.
func (m *Manager) List() ([]Info, error) {
	list, err := m.Store.ListRunners()
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	out := make([]Info, 0, len(list))
	for _, r := range list {
		out = append(out, m.info(r))
	}
	return out, nil
}

// Jobs returns runner's jobs (every runner's for ""), newest first.
func (m *Manager) Jobs(runner string) ([]store.RunnerJob, error) {
	return m.Store.ListRunnerJobs(runner)
}

func (m *Manager) info(r store.Runner) Info {
	r.Token = ""
	in := Info{Runner: r}
	m.mu.Lock()
	if l := m.loops[r.Name]; l != nil {
		in.Running, in.LastPoll, in.LastErr = l.running, l.lastPoll, l.lastErr
	}
	m.mu.Unlock()
	return in
}

func (m *Manager) start(ctx context.Context, r store.Runner) {
	ctx, cancel := context.WithCancel(ctx)
	l := &loop{cancel: cancel, done: make(chan struct{}), active: map[int64]int{}, recent: map[int64]time.Time{}}
	m.mu.Lock()
	if old := m.loops[r.Name]; old != nil {
		old.cancel()
	}
	m.loops[r.Name] = l
	m.mu.Unlock()
	go m.run(ctx, r.Name, l)
}

// stop cancels a runner's loop and waits for it and its jobs to end.
func (m *Manager) stop(name string) {
	m.mu.Lock()
	l := m.loops[name]
	delete(m.loops, name)
	m.mu.Unlock()
	if l != nil {
		l.cancel()
		<-l.done
		l.jobs.Wait()
	}
}

// Wait waits for the jobs started so far; for tests and shutdown.
func (m *Manager) Wait() { m.jobs.Wait() }

func (m *Manager) run(ctx context.Context, name string, l *loop) {
	defer close(l.done)
	t := time.NewTicker(m.Poll)
	defer t.Stop()
	for {
		m.pollOnce(ctx, name, l)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// recentFor keeps a job just handled from being started again while the forge still
// lists it as waiting.
const recentFor = 2 * time.Minute

// pollOnce asks the forge for waiting jobs and starts clones for those it can take.
func (m *Manager) pollOnce(ctx context.Context, name string, l *loop) {
	r, err := m.Store.GetRunner(name)
	if err != nil || r.Paused {
		return
	}
	setErr := func(err error) {
		m.mu.Lock()
		l.lastPoll, l.lastErr = time.Now(), ""
		if err != nil {
			l.lastErr = err.Error()
		}
		m.mu.Unlock()
	}
	forge, err := m.newForge(r)
	if err != nil {
		setErr(err)
		return
	}
	labels := make([]string, 0, len(r.Labels))
	for l := range r.Labels {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	jobs, err := forge.Waiting(ctx, labels)
	setErr(err)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("runner %s: %v", name, err)
		}
		return
	}
	for _, job := range jobs {
		distro := pickDistro(r.Labels, job.Labels)
		if distro == "" {
			continue
		}
		m.mu.Lock()
		now := time.Now()
		for id, at := range l.recent {
			if now.Sub(at) > recentFor {
				delete(l.recent, id)
			}
		}
		_, recent := l.recent[job.ID]
		take := l.active[job.ID] == 0 && !recent && l.running < r.MaxJobs
		if take {
			l.active[job.ID]++
			l.running++
		}
		m.mu.Unlock()
		if !take {
			continue
		}
		m.jobs.Add(1)
		l.jobs.Add(1)
		go func() {
			defer m.jobs.Done()
			defer l.jobs.Done()
			id := m.runJob(ctx, r, forge, job, distro, l)
			m.mu.Lock()
			l.running--
			l.unmark(id)
			l.recent[id] = time.Now()
			m.mu.Unlock()
		}()
	}
}

// unmark drops one clone of job id from l.active; m.mu is held.
func (l *loop) unmark(id int64) {
	if l.active[id]--; l.active[id] <= 0 {
		delete(l.active, id)
	}
}

// pickDistro is the distro for a job whose labels are all mapped, by its first label in
// order; "" when one of them isn't the runner's.
func pickDistro(mapped map[string]string, labels []string) string {
	distro := ""
	for _, l := range labels {
		d, ok := mapped[l]
		if !ok {
			if githubGeneric[strings.ToLower(l)] {
				continue
			}
			return ""
		}
		if distro == "" {
			distro = d
		}
	}
	return distro
}

// runJob registers single-use runner credentials and runs the forge's runner for job in
// a clone of distro, recording how it went. It returns the id of the job the runner took.
func (m *Manager) runJob(ctx context.Context, r store.Runner, forge Forge, job Job, distro string, l *loop) int64 {
	start := time.Now()
	rec := store.RunnerJob{
		ID: fmt.Sprintf("%s/%d/%d", r.Name, job.ID, start.UnixNano()), Runner: r.Name, JobID: job.ID, Name: job.Name,
		Labels: job.Labels, Distro: distro, State: "running", StartedAt: start,
	}
	// recMu guards rec and skipped, which the watchdog also writes.
	var recMu sync.Mutex
	skipped := ""
	_ = m.Store.PutRunnerJob(rec)
	finish := func(code int, err error) {
		recMu.Lock()
		defer recMu.Unlock()
		rec.EndedAt, rec.ExitCode, rec.State = time.Now(), code, "succeeded"
		if err != nil || code != 0 {
			rec.State = "failed"
		}
		if err != nil {
			rec.Error = err.Error()
		}
		if skipped != "" {
			rec.State, rec.Error = "skipped", skipped
		}
		if ctx.Err() != nil {
			rec.State, rec.Error = "failed", "the runner was stopped"
		}
		_ = m.Store.PutRunnerJob(rec)
		m.prune(r.Name)
	}
	name := fmt.Sprintf("anvil-%s-%d", r.Name, job.ID)
	launch, err := forge.Register(ctx, name, job)
	if err != nil {
		finish(-1, err)
		return job.ID
	}
	defer func() {
		if launch.Cleanup != nil {
			launch.Cleanup(context.WithoutCancel(ctx))
		}
	}()
	req := ephemeral.RunRequest{
		Distros: []string{distro}, Argv: []string{"sh", "-c", launch.Script}, Env: launch.Env,
		CPUs: r.CPUs, MemoryMiB: r.MemoryMiB,
	}
	// The runner goes in through a setup snapshot built once, on the distro or on the
	// picked warm snapshot.
	installName, script := forge.Install(r.Version, r.Deps)
	req.Setup = &ephemeral.SetupScript{Name: installName, Script: []byte("#!/bin/sh\n" + script)}
	runCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	current := job.ID
	started := make(chan struct{})
	var startOnce sync.Once
	watchCtx, stopWatch := context.WithCancel(runCtx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		if launch.Watch == nil {
			return
		}
		watchRunner(watchCtx, launch.Watch, started, func(took Job) {
			recMu.Lock()
			defer recMu.Unlock()
			if took.ID == rec.JobID {
				return
			}
			// The runner took another job: it is the one to record, and job goes back to waiting.
			m.mu.Lock()
			l.unmark(current)
			l.active[took.ID]++
			m.mu.Unlock()
			current = took.ID
			rec.JobID, rec.Name = took.ID, took.Name
			if len(took.Labels) > 0 {
				rec.Labels = took.Labels
			}
			_ = m.Store.PutRunnerJob(rec)
		}, func(why string) {
			recMu.Lock()
			skipped = why
			recMu.Unlock()
			cancel()
		})
	}()

	var out tail
	code, runErr := -1, error(nil)
	err = m.Runs.Run(runCtx, req, func(ev ephemeral.Event) {
		switch {
		case ev.Progress != nil:
			recMu.Lock()
			if ev.Progress.CloneID != "" && rec.CloneID == "" {
				rec.CloneID = ev.Progress.CloneID
				_ = m.Store.PutRunnerJob(rec)
			}
			recMu.Unlock()
			if ev.Progress.Started {
				startOnce.Do(func() { close(started) })
			}
		case ev.Output != nil:
			out.write(ev.Output.Data)
		case ev.Exit != nil:
			code = ev.Exit.Code
			if ev.Exit.Err != "" {
				runErr = errors.New(ev.Exit.Err)
			}
		}
	})
	stopWatch()
	<-watched
	if err == nil {
		err = runErr
	}
	if err == nil && code != 0 {
		err = fmt.Errorf("the runner exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	finish(code, err)
	recMu.Lock()
	defer recMu.Unlock()
	return current
}

// watchRunner polls a runner once its clone runs the command. It reports the job the
// runner took, or calls skip when no job comes to it.
func watchRunner(ctx context.Context, watch func(context.Context) (RunnerState, error), started <-chan struct{}, took func(Job), skip func(string)) {
	select {
	case <-started:
	case <-ctx.Done():
		return
	}
	since := time.Now()
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	misses, lookups := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := watch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("runner watchdog: %v", err)
			}
			continue
		}
		if st.Busy {
			// Busy: only the job it took is left to find; the job timeout still applies.
			if st.Took != nil {
				took(*st.Took)
				return
			}
			if lookups++; lookups >= watchLookups {
				return
			}
			continue
		}
		// Two polls in a row, so a job being handed over isn't cut off.
		misses++
		if st.Waiting {
			misses = 0
		}
		switch {
		case misses >= 2:
			skip("the job was cancelled or taken by another runner, and no other job came to this runner")
			return
		case time.Since(since) >= idleLimit:
			skip(fmt.Sprintf("no job came to this runner within %s", idleLimit))
			return
		}
	}
}

// prune drops runner's oldest finished jobs past keepJobs.
func (m *Manager) prune(runner string) {
	jobs, err := m.Store.ListRunnerJobs(runner)
	if err != nil {
		return
	}
	kept := 0
	for _, j := range jobs {
		if j.State == "running" {
			continue
		}
		if kept++; kept > keepJobs {
			_ = m.Store.DeleteRunnerJob(j.ID)
		}
	}
}

// MarkInterrupted records jobs left running by an earlier anvild as failed: their clones
// went with it.
func (m *Manager) MarkInterrupted() {
	jobs, err := m.Store.ListRunnerJobs("")
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.State == "running" {
			j.State, j.Error, j.EndedAt = "failed", "anvild restarted during the job", time.Now()
			_ = m.Store.PutRunnerJob(j)
		}
	}
}

// tail keeps the last errTail bytes written to it.
type tail struct{ b []byte }

func (t *tail) write(p []byte) {
	t.b = append(t.b, p...)
	if n := len(t.b); n > errTail {
		t.b = append([]byte(nil), t.b[n-errTail:]...)
	}
}

func (t *tail) String() string { return string(t.b) }
