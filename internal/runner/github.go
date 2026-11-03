package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// github takes jobs of one repository through just-in-time runner configs.
type github struct {
	api
}

// githubJob is a job as the GitHub API lists it.
type githubJob struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Labels     []string `json:"labels"`
	RunnerName string   `json:"runner_name"`
}

func (j githubJob) job() Job { return Job{ID: j.ID, Name: j.Name, Labels: j.Labels, RunID: j.RunID} }

// githubGeneric are labels every self-hosted Linux runner has, so a job asking for them
// alone doesn't pick an anvil runner.
var githubGeneric = map[string]bool{"self-hosted": true, "linux": true, "x64": true, "arm64": true}

func (g *github) Waiting(ctx context.Context, labels []string) ([]Job, error) {
	want := map[string]bool{}
	for _, l := range labels {
		want[strings.ToLower(l)] = true
	}
	// A run turns in_progress once its first job starts, so later jobs sit in such runs.
	var ids []int64
	seen := map[int64]bool{}
	for _, status := range []string{"queued", "in_progress"} {
		var runs struct {
			WorkflowRuns []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		if err := g.do(ctx, "GET", "/actions/runs?status="+status+"&per_page=50", nil, &runs); err != nil {
			return nil, err
		}
		for _, run := range runs.WorkflowRuns {
			if !seen[run.ID] {
				seen[run.ID] = true
				ids = append(ids, run.ID)
			}
		}
	}
	var out []Job
	for _, id := range ids {
		jobs, err := g.runJobs(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Status != "queued" || !githubMatches(j.Labels, want) {
				continue
			}
			if j.RunID == 0 {
				j.RunID = id
			}
			out = append(out, j.job())
		}
	}
	return out, nil
}

// runJobs lists the jobs of workflow run id.
func (g *github) runJobs(ctx context.Context, id int64) ([]githubJob, error) {
	var jobs struct {
		Jobs []githubJob `json:"jobs"`
	}
	err := g.do(ctx, "GET", fmt.Sprintf("/actions/runs/%d/jobs?per_page=100", id), nil, &jobs)
	return jobs.Jobs, err
}

// githubMatches reports whether every label of a job is generic or one of want, and at
// least one is in want.
func githubMatches(labels []string, want map[string]bool) bool {
	ours := false
	for _, l := range labels {
		l = strings.ToLower(l)
		switch {
		case want[l]:
			ours = true
		case !githubGeneric[l]:
			return false
		}
	}
	return ours
}

func (g *github) Register(ctx context.Context, name string, job Job) (Launch, error) {
	labels := []string{"self-hosted", "linux"}
	for _, l := range job.Labels {
		if !githubGeneric[strings.ToLower(l)] {
			labels = append(labels, l)
		}
	}
	var reply struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		JITConfig string `json:"encoded_jit_config"`
	}
	body := map[string]any{"name": name, "runner_group_id": 1, "labels": labels, "work_folder": "_work"}
	if err := g.do(ctx, "POST", "/actions/runners/generate-jitconfig", body, &reply); err != nil {
		return Launch{}, fmt.Errorf("github: making a just-in-time runner: %w", err)
	}
	if reply.JITConfig == "" {
		return Launch{}, fmt.Errorf("github: the just-in-time runner came back without a config")
	}
	return Launch{
		Script: `cd /opt/actions-runner
export RUNNER_ALLOW_RUNASROOT=1
cfg=$ANVIL_JIT_CONFIG
unset ANVIL_JIT_CONFIG
exec ./run.sh --jitconfig "$cfg"`,
		Env: []string{"ANVIL_JIT_CONFIG=" + reply.JITConfig},
		// A just-in-time runner that never took a job stays listed as offline.
		Cleanup: func(ctx context.Context) {
			if reply.Runner.ID != 0 {
				_ = g.do(ctx, "DELETE", fmt.Sprintf("/actions/runners/%d", reply.Runner.ID), nil, nil)
			}
		},
		// GitHub gives any queued job with matching labels to any idle runner.
		Watch: func(ctx context.Context) (RunnerState, error) { return g.watch(ctx, reply.Runner.ID, name, job) },
	}, nil
}

// watch reports on runner id, named name and made for job.
func (g *github) watch(ctx context.Context, id int64, name string, job Job) (RunnerState, error) {
	var st RunnerState
	var x githubJob
	if err := g.do(ctx, "GET", fmt.Sprintf("/actions/jobs/%d", job.ID), nil, &x); err != nil {
		return st, err
	}
	switch {
	case x.RunnerName == name:
		took := x.job()
		return RunnerState{Busy: true, Took: &took}, nil
	case x.Status == "queued":
		st.Waiting = true
	}
	var rn struct {
		Busy bool `json:"busy"`
	}
	err := g.do(ctx, "GET", fmt.Sprintf("/actions/runners/%d", id), nil, &rn)
	// A just-in-time runner goes away by itself once its job is done.
	var he *httpError
	if errors.As(err, &he) && he.code == http.StatusNotFound {
		rn.Busy, err = true, nil
	}
	if err != nil || !rn.Busy {
		return st, err
	}
	st.Busy = true
	st.Took, err = g.findJob(ctx, name, job.RunID)
	return st, err
}

// findJob looks for the job runner name took, in run first, then in the runs in progress.
func (g *github) findJob(ctx context.Context, name string, run int64) (*Job, error) {
	if run != 0 {
		if j, err := g.findIn(ctx, name, run); j != nil || err != nil {
			return j, err
		}
	}
	var runs struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := g.do(ctx, "GET", "/actions/runs?status=in_progress&per_page=50", nil, &runs); err != nil {
		return nil, err
	}
	for _, r := range runs.WorkflowRuns {
		if r.ID == run {
			continue
		}
		if j, err := g.findIn(ctx, name, r.ID); j != nil || err != nil {
			return j, err
		}
	}
	return nil, nil
}

// findIn is the job of run that runner name took, or nil.
func (g *github) findIn(ctx context.Context, name string, run int64) (*Job, error) {
	jobs, err := g.runJobs(ctx, run)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.RunnerName == name {
			if j.RunID == 0 {
				j.RunID = run
			}
			took := j.job()
			return &took, nil
		}
	}
	return nil, nil
}

func (g *github) Install(version string, deps []string) (string, string) {
	v := strings.TrimPrefix(version, "v")
	name := "actions-runner " + v
	if v == "" {
		name = "actions-runner latest"
	}
	return setupName(name, deps), toolsScript(deps) + `v=` + shellQuote(v) + `
if [ -z "$v" ]; then
	v=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)
fi
mkdir -p /opt/actions-runner
curl -fsSL "https://github.com/actions/runner/releases/download/v$v/actions-runner-linux-$arch-$v.tar.gz" | tar -xz -C /opt/actions-runner
/opt/actions-runner/bin/installdependencies.sh
`
}
