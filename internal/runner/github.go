package runner

import (
	"context"
	"fmt"
	"strings"
)

// github takes jobs of one repository through just-in-time runner configs.
type github struct {
	api
}

// githubGeneric are labels every self-hosted Linux runner has, so a job asking for them
// alone doesn't pick an anvil runner.
var githubGeneric = map[string]bool{"self-hosted": true, "linux": true, "x64": true, "arm64": true}

func (g *github) Waiting(ctx context.Context, labels []string) ([]Job, error) {
	want := map[string]bool{}
	for _, l := range labels {
		want[strings.ToLower(l)] = true
	}
	var runs struct {
		WorkflowRuns []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := g.do(ctx, "GET", "/actions/runs?status=queued&per_page=50", nil, &runs); err != nil {
		return nil, err
	}
	var out []Job
	for _, run := range runs.WorkflowRuns {
		var jobs struct {
			Jobs []struct {
				ID     int64    `json:"id"`
				Name   string   `json:"name"`
				Status string   `json:"status"`
				Labels []string `json:"labels"`
			} `json:"jobs"`
		}
		if err := g.do(ctx, "GET", fmt.Sprintf("/actions/runs/%d/jobs?per_page=100", run.ID), nil, &jobs); err != nil {
			return nil, err
		}
		for _, j := range jobs.Jobs {
			if j.Status != "queued" || !githubMatches(j.Labels, want) {
				continue
			}
			out = append(out, Job{ID: j.ID, Name: j.Name, Labels: j.Labels})
		}
	}
	return out, nil
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
	}, nil
}

func (g *github) Install(version string) (string, string) {
	v := strings.TrimPrefix(version, "v")
	name := "actions-runner " + v
	if v == "" {
		name = "actions-runner latest"
	}
	return name, toolsScript + `v=` + shellQuote(v) + `
if [ -z "$v" ]; then
	v=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n1)
fi
mkdir -p /opt/actions-runner
curl -fsSL "https://github.com/actions/runner/releases/download/v$v/actions-runner-linux-$arch-$v.tar.gz" | tar -xz -C /opt/actions-runner
/opt/actions-runner/bin/installdependencies.sh
`
}
