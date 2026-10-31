package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var (
	bucketRunners         = []byte("runners")          // name -> json(Runner)
	bucketRunnerJobs      = []byte("runner_jobs")      // id -> json(RunnerJob)
	bucketPipelineRuns    = []byte("pipeline_runs")    // id -> json(PipelineRun)
	bucketPipelineWatches = []byte("pipeline_watches") // name -> json(PipelineWatch)
)

// Runner takes jobs from a forge and runs each in its own ephemeral clone.
type Runner struct {
	Name      string
	Forge     string            // "forgejo" or "github"
	URL       string            // the forge's base URL, e.g. https://code.example.com
	Scope     string            // "repos/<owner>/<repo>", "orgs/<org>" or "admin" (forgejo only)
	Token     string            // API token; never sent back to clients
	Labels    map[string]string // job label -> distro or "@<warm key>"
	CPUs      int
	MemoryMiB int64
	MaxJobs   int    // jobs at once
	Version   string // runner release to install in clones, "" for the latest
	Paused    bool
	CreatedAt time.Time
}

// RunnerJob is one forge job a runner ran, or is running.
type RunnerJob struct {
	ID        string // "<runner>/<forge job id>/<attempt time>"
	Runner    string
	JobID     int64
	Name      string
	Labels    []string
	Distro    string
	CloneID   string
	State     string // "running", "succeeded", "failed"
	ExitCode  int
	Error     string
	StartedAt time.Time
	EndedAt   time.Time
}

// PipelineRun is one run of an anvil-ci.yaml, by hand or by a watch.
type PipelineRun struct {
	ID        string // a ULID, sorted by time
	Source    string // "manual", "rerun:<id>" or "watch:<name>"
	File      string // the pipeline file, relative to the source
	Commit    string // with a watch: the commit it ran
	Owner     uint32 // uid of who started it, or who added its watch; apply checks host paths against it
	State     string // "running", "succeeded", "failed", "cancelled"
	Error     string
	Jobs      []PipelineJob
	Applied   string // with apply on success: the file applied, or why it failed
	StartedAt time.Time
	EndedAt   time.Time
}

// PipelineJob is one job of a run on one distro.
type PipelineJob struct {
	Name     string
	Distro   string
	State    string // "pending", "running", "succeeded", "failed", "skipped"
	ExitCode int
	Error    string
	Duration time.Duration
}

// PipelineWatch runs a repo's pipeline on every new commit of a branch.
type PipelineWatch struct {
	Name       string
	Repo       string // anything `git clone` takes
	Branch     string
	File       string // the pipeline file in the repo, "anvil-ci.yaml" by default
	Interval   time.Duration
	Owner      uint32 // uid of who added it
	Paused     bool
	LastCommit string
	LastRun    string
	LastError  string
	LastPoll   time.Time
	CreatedAt  time.Time
}

func putJSON(s *Store, bucket []byte, key string, v any) error {
	if key == "" {
		return fmt.Errorf("store: empty key in %s", bucket)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: encoding %s %s: %w", bucket, key, err)
	}
	return s.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bucket).Put([]byte(key), data) })
}

func getJSON(s *Store, bucket []byte, key string, v any) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket(bucket).Get([]byte(key))
		if data == nil {
			return fmt.Errorf("store: no %s %q: %w", bucket, key, instance.ErrNotFound)
		}
		return json.Unmarshal(data, v)
	})
}

func listJSON[T any](s *Store, bucket []byte) ([]T, error) {
	var out []T
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, data []byte) error {
			var v T
			if err := json.Unmarshal(data, &v); err != nil {
				return err
			}
			out = append(out, v)
			return nil
		})
	})
	return out, err
}

func deleteJSON(s *Store, bucket []byte, key string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		if b.Get([]byte(key)) == nil {
			return fmt.Errorf("store: no %s %q: %w", bucket, key, instance.ErrNotFound)
		}
		return b.Delete([]byte(key))
	})
}

func (s *Store) PutRunner(r Runner) error { return putJSON(s, bucketRunners, r.Name, r) }
func (s *Store) GetRunner(name string) (Runner, error) {
	var r Runner
	return r, getJSON(s, bucketRunners, name, &r)
}
func (s *Store) ListRunners() ([]Runner, error) { return listJSON[Runner](s, bucketRunners) }
func (s *Store) DeleteRunner(name string) error { return deleteJSON(s, bucketRunners, name) }

func (s *Store) PutRunnerJob(j RunnerJob) error { return putJSON(s, bucketRunnerJobs, j.ID, j) }

// ListRunnerJobs returns the jobs of runner (all runners for ""), newest first.
func (s *Store) ListRunnerJobs(runner string) ([]RunnerJob, error) {
	all, err := listJSON[RunnerJob](s, bucketRunnerJobs)
	var out []RunnerJob
	for _, j := range all {
		if runner == "" || j.Runner == runner {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, err
}

func (s *Store) DeleteRunnerJob(id string) error { return deleteJSON(s, bucketRunnerJobs, id) }

func (s *Store) PutPipelineRun(r PipelineRun) error { return putJSON(s, bucketPipelineRuns, r.ID, r) }
func (s *Store) GetPipelineRun(id string) (PipelineRun, error) {
	var r PipelineRun
	return r, getJSON(s, bucketPipelineRuns, id, &r)
}

// ListPipelineRuns returns every run, newest first.
func (s *Store) ListPipelineRuns() ([]PipelineRun, error) {
	out, err := listJSON[PipelineRun](s, bucketPipelineRuns)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, err
}
func (s *Store) DeletePipelineRun(id string) error { return deleteJSON(s, bucketPipelineRuns, id) }

func (s *Store) PutPipelineWatch(w PipelineWatch) error {
	return putJSON(s, bucketPipelineWatches, w.Name, w)
}
func (s *Store) GetPipelineWatch(name string) (PipelineWatch, error) {
	var w PipelineWatch
	return w, getJSON(s, bucketPipelineWatches, name, &w)
}
func (s *Store) ListPipelineWatches() ([]PipelineWatch, error) {
	return listJSON[PipelineWatch](s, bucketPipelineWatches)
}
func (s *Store) DeletePipelineWatch(name string) error {
	return deleteJSON(s, bucketPipelineWatches, name)
}
