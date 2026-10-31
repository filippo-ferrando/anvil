package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// Store keeps runs and watches; *store.Store is one.
type Store interface {
	PutPipelineRun(store.PipelineRun) error
	GetPipelineRun(id string) (store.PipelineRun, error)
	ListPipelineRuns() ([]store.PipelineRun, error)
	DeletePipelineRun(id string) error
	PutPipelineWatch(store.PipelineWatch) error
	GetPipelineWatch(name string) (store.PipelineWatch, error)
	ListPipelineWatches() ([]store.PipelineWatch, error)
	DeletePipelineWatch(name string) error
}

// Runs runs commands in ephemeral clones; *ephemeral.Manager is one.
type Runs interface {
	Run(ctx context.Context, r ephemeral.RunRequest, emit func(ephemeral.Event)) error
}

// ApplyFunc applies an anvil.yaml for uid, whose host paths it checks.
type ApplyFunc func(ctx context.Context, uid uint32, manifest []byte, progress func(string)) error

// keepRuns is how many runs are kept, with their files.
const keepRuns = 50

// Event is something a run reports while it goes; exactly one field is set.
type Event struct {
	Progress *Progress // Job "" for the run itself
	Output   *Output
	Job      *store.PipelineJob // a job on one distro changed state
	Run      *store.PipelineRun // the run started or ended
}

type Progress struct{ Job, Distro, Message string }

type Output struct {
	Job, Distro string
	Stderr      bool
	Data        []byte
}

// Source is what a run runs: a tar of the repo and the pipeline file in it.
type Source struct {
	Tar    string // moved into the run's directory
	File   string // "" for DefaultFile
	Origin string // "manual", "rerun:<id>", "watch:<name>"
	Commit string
	Owner  uint32
}

// Executor runs pipelines and keeps their history.
type Executor struct {
	Store Store
	Runs  Runs
	Dir   string // one directory per run: source.tar, logs/, artifacts/
	Apply ApplyFunc

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewExecutor(s Store, runs Runs, dir string, apply ApplyFunc) *Executor {
	return &Executor{Store: s, Runs: runs, Dir: dir, Apply: apply, cancels: map[string]context.CancelFunc{}}
}

// RunDir is where run id's files are.
func (e *Executor) RunDir(id string) string { return filepath.Join(e.Dir, id) }

// Run runs src's pipeline to the end and returns its record. Cancelling ctx, or Cancel,
// stops it and deletes its clones. emit may be nil.
func (e *Executor) Run(ctx context.Context, src Source, emit func(Event)) (store.PipelineRun, error) {
	if emit == nil {
		emit = func(Event) {}
	}
	var emitMu sync.Mutex
	safe := func(ev Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emit(ev)
	}
	if src.File == "" {
		src.File = DefaultFile
	}
	if err := relPath(src.File); err != nil {
		return store.PipelineRun{}, fmt.Errorf("pipeline file: %w", err)
	}
	id := ulid.Make().String()
	dir := e.RunDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o750); err != nil {
		return store.PipelineRun{}, err
	}
	tarPath := filepath.Join(dir, "source.tar")
	if err := moveFile(src.Tar, tarPath); err != nil {
		_ = os.RemoveAll(dir)
		return store.PipelineRun{}, err
	}
	rec := store.PipelineRun{ID: id, Source: src.Origin, File: src.File, Commit: src.Commit, Owner: src.Owner, State: "running", StartedAt: time.Now()}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	e.cancels[id] = cancel
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, id)
		e.mu.Unlock()
	}()

	r := &run{e: e, rec: rec, dir: dir, tar: tarPath, emit: safe}
	err := r.execute(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.EndedAt = time.Now()
	switch {
	case ctx.Err() != nil:
		r.rec.State, r.rec.Error = "cancelled", "cancelled"
	case err != nil:
		r.rec.State, r.rec.Error = "failed", err.Error()
	case r.failed():
		r.rec.State = "failed"
	default:
		r.rec.State = "succeeded"
	}
	_ = e.Store.PutPipelineRun(r.rec)
	_ = os.RemoveAll(filepath.Join(dir, "src"))
	final := r.rec
	safe(Event{Run: &final})
	e.prune()
	return final, nil
}

// Cancel stops run id when it runs here.
func (e *Executor) Cancel(id string) error {
	e.mu.Lock()
	cancel := e.cancels[id]
	e.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("pipeline run %s isn't running: %w", id, instance.ErrNotFound)
	}
	cancel()
	return nil
}

// MarkInterrupted records runs an earlier anvild left running as failed.
func (e *Executor) MarkInterrupted() {
	runs, err := e.Store.ListPipelineRuns()
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.State == "running" {
			r.State, r.Error, r.EndedAt = "failed", "anvild restarted during the run", time.Now()
			for i := range r.Jobs {
				if r.Jobs[i].State == "running" || r.Jobs[i].State == "pending" {
					r.Jobs[i].State = "failed"
				}
			}
			_ = e.Store.PutPipelineRun(r)
			_ = os.RemoveAll(filepath.Join(e.RunDir(r.ID), "src"))
		}
	}
}

// prune drops the oldest finished runs past keepRuns, with their files.
func (e *Executor) prune() {
	runs, err := e.Store.ListPipelineRuns()
	if err != nil {
		return
	}
	kept := 0
	for _, r := range runs {
		if r.State == "running" {
			continue
		}
		if kept++; kept > keepRuns {
			_ = e.Store.DeletePipelineRun(r.ID)
			_ = os.RemoveAll(e.RunDir(r.ID))
		}
	}
}

// run is one pipeline run in progress.
type run struct {
	e    *Executor
	dir  string
	tar  string
	emit func(Event)

	mu  sync.Mutex
	rec store.PipelineRun
}

func (r *run) save() {
	r.mu.Lock()
	rec := r.rec
	rec.Jobs = append([]store.PipelineJob(nil), r.rec.Jobs...)
	r.mu.Unlock()
	_ = r.e.Store.PutPipelineRun(rec)
}

func (r *run) progress(job, distro, msg string) {
	r.emit(Event{Progress: &Progress{Job: job, Distro: distro, Message: msg}})
}

func (r *run) failed() bool {
	for _, j := range r.rec.Jobs {
		if j.State != "succeeded" {
			return true
		}
	}
	return strings.HasPrefix(r.rec.Applied, "failed")
}

// setJob updates job on distro, saves the record and reports it.
func (r *run) setJob(job, distro string, change func(*store.PipelineJob)) {
	r.mu.Lock()
	var cp store.PipelineJob
	for i := range r.rec.Jobs {
		if j := &r.rec.Jobs[i]; j.Name == job && j.Distro == distro {
			change(j)
			cp = *j
		}
	}
	r.mu.Unlock()
	r.save()
	r.emit(Event{Job: &cp})
}

func (r *run) execute(ctx context.Context) error {
	srcDir := filepath.Join(r.dir, "src")
	f, err := os.Open(r.tar)
	if err != nil {
		return err
	}
	err = guestexec.Extract(f, srcDir)
	f.Close()
	if err != nil {
		return fmt.Errorf("unpacking the source: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(r.rec.File)))
	if err != nil {
		return fmt.Errorf("reading the pipeline file: %w", err)
	}
	p, err := Parse(data)
	if err != nil {
		return err
	}
	for _, name := range p.Order() {
		for _, d := range p.Jobs[name].Distros {
			r.rec.Jobs = append(r.rec.Jobs, store.PipelineJob{Name: name, Distro: d, State: "pending"})
		}
	}
	r.save()
	run := r.rec
	r.emit(Event{Run: &run})

	// Start every job whose needs passed, until nothing more can start.
	state := map[string]string{} // job -> pending, running, succeeded, failed, skipped
	done := make(chan string)
	running := 0
	for {
		for _, name := range p.Order() {
			if state[name] != "" {
				continue
			}
			ready, skip := true, false
			for _, n := range p.Jobs[name].Needs {
				switch state[n] {
				case "succeeded":
				case "failed", "skipped":
					skip = true
				default:
					ready = false
				}
			}
			switch {
			case skip:
				state[name] = "skipped"
				for _, d := range p.Jobs[name].Distros {
					r.setJob(name, d, func(j *store.PipelineJob) { j.State, j.Error = "skipped", "a job it needs failed" })
				}
			case ready && ctx.Err() == nil:
				state[name] = "running"
				running++
				go func() {
					ok := r.runJob(ctx, srcDir, name, p.Jobs[name])
					if ok {
						done <- name + "\x00succeeded"
					} else {
						done <- name + "\x00failed"
					}
				}()
			}
		}
		if running == 0 {
			break
		}
		name, st, _ := strings.Cut(<-done, "\x00")
		state[name] = st
		running--
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if p.Apply != "" && !r.failed() {
		r.applyManifest(ctx, srcDir, p.Apply)
	}
	return nil
}

// runJob runs job on all its distros at once and reports whether every one passed.
func (r *run) runJob(ctx context.Context, srcDir, name string, j *Job) bool {
	fail := func(err error) bool {
		for _, d := range j.Distros {
			r.setJob(name, d, func(pj *store.PipelineJob) { pj.State, pj.Error, pj.ExitCode = "failed", err.Error(), -1 })
		}
		return false
	}
	var setup *ephemeral.SetupScript
	if j.Setup != "" {
		s, err := readSetup(srcDir, j.Setup, j.KeyFiles)
		if err != nil {
			return fail(err)
		}
		setup = s
	}
	var env []string
	for k, v := range j.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	logs := map[string]*os.File{}
	for _, d := range j.Distros {
		f, err := os.Create(filepath.Join(r.dir, "logs", logName(name, d)))
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		logs[d] = f
		r.setJob(name, d, func(pj *store.PipelineJob) { pj.State = "running" })
	}
	collector := client.NewCollector(j.Collect, func(_ int, distro string) string {
		return filepath.Join(r.dir, "artifacts", name, distro)
	})
	defer collector.Close()

	ctx, cancel := context.WithTimeout(ctx, j.timeout)
	defer cancel()
	ok := true
	err := r.e.Runs.Run(ctx, ephemeral.RunRequest{
		Distros: j.Distros, Argv: []string{"sh", "-c", j.Run}, Env: env, CPUs: j.CPUs, MemoryMiB: j.Memory,
		Upload: r.tar, Collect: j.Collect, Setup: setup,
	}, func(ev ephemeral.Event) {
		switch {
		case ev.Progress != nil:
			r.progress(name, ev.Progress.Distro, ev.Progress.Message)
		case ev.Output != nil:
			if f := logs[ev.Output.Distro]; f != nil {
				_, _ = f.Write(ev.Output.Data)
			}
			r.emit(Event{Output: &Output{Job: name, Distro: ev.Output.Distro, Stderr: ev.Output.Stderr, Data: ev.Output.Data}})
		case ev.Collect != nil:
			c := ev.Collect
			res := collector.Handle(&anvilv1.RunCollect{CloneId: c.CloneID, Distro: c.Distro, Index: int32(c.Index), Data: c.Data, Done: c.Done, Warning: c.Warning})
			if res != nil && res.Err != nil {
				r.progress(name, c.Distro, "collect: "+res.Err.Error())
			}
		case ev.Exit != nil:
			x := ev.Exit
			passed := x.Code == 0 && x.Err == ""
			ok = ok && passed
			r.setJob(name, x.Distro, func(pj *store.PipelineJob) {
				pj.State, pj.ExitCode, pj.Duration, pj.Error = "succeeded", x.Code, x.Duration, x.Err
				if !passed {
					pj.State = "failed"
				}
				if ctx.Err() == context.DeadlineExceeded {
					pj.Error = "timed out after " + j.timeout.String()
				}
			})
		}
	})
	if err != nil {
		return fail(err)
	}
	return ok
}

// applyManifest applies the pipeline's anvil.yaml as the run's owner.
func (r *run) applyManifest(ctx context.Context, srcDir, file string) {
	set := func(s string) {
		r.mu.Lock()
		r.rec.Applied = s
		r.mu.Unlock()
		r.save()
	}
	r.progress("", "", "applying "+file)
	data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(file)))
	if err == nil && r.e.Apply == nil {
		err = errors.New("apply isn't available here")
	}
	if err == nil {
		err = r.e.Apply(ctx, r.rec.Owner, data, func(s string) { r.progress("", "", s) })
	}
	if err != nil {
		set("failed: " + err.Error())
		r.progress("", "", "apply failed: "+err.Error())
		return
	}
	set("applied " + file)
}

// readSetup reads a job's setup script and key files from the source.
func readSetup(srcDir, script string, keyFiles []string) (*ephemeral.SetupScript, error) {
	data, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(script)))
	if err != nil {
		return nil, fmt.Errorf("reading the setup script: %w", err)
	}
	s := &ephemeral.SetupScript{Name: script, Script: data}
	for _, f := range keyFiles {
		b, err := os.ReadFile(filepath.Join(srcDir, filepath.FromSlash(f)))
		if err != nil {
			return nil, fmt.Errorf("reading key file: %w", err)
		}
		s.Files = append(s.Files, ephemeral.SetupFile{Name: f, Data: b})
	}
	return s, nil
}

// logName is the log file of job on distro.
func logName(job, distro string) string {
	return job + "@" + strings.ReplaceAll(distro, "/", "_") + ".log"
}

// LogPath is where a run keeps the output of job on distro.
func (e *Executor) LogPath(id, job, distro string) string {
	return filepath.Join(e.RunDir(id), "logs", logName(job, distro))
}

// moveFile renames src to dst, or copies it across filesystems.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}
