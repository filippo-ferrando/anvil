package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// forgejo talks to Forgejo 15 or later: its waiting-jobs API and ephemeral runners.
type forgejo struct {
	api
	url string
}

type forgejoJob struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Handle string   `json:"handle"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
}

func (f *forgejo) Waiting(ctx context.Context, labels []string) ([]Job, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	// A bare array, or {"jobs": [...]}, or null when nothing waits.
	var raw json.RawMessage
	if err := f.do(ctx, "GET", "/actions/runners/jobs?labels="+url.QueryEscape(strings.Join(labels, ",")), nil, &raw); err != nil {
		return nil, err
	}
	var list []forgejoJob
	if err := json.Unmarshal(raw, &list); err != nil {
		var env struct {
			Jobs []forgejoJob `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("forgejo: reading waiting jobs: %w", err)
		}
		list = env.Jobs
	}
	var out []Job
	for _, j := range list {
		if s := strings.ToLower(j.Status); s != "" && s != "waiting" && s != "blocked" {
			continue
		}
		out = append(out, Job{ID: j.ID, Name: j.Name, Labels: j.RunsOn, Handle: j.Handle})
	}
	return out, nil
}

func (f *forgejo) Register(ctx context.Context, name string, job Job) (Launch, error) {
	var reg struct {
		ID    int64  `json:"id"`
		UUID  string `json:"uuid"`
		Token string `json:"token"`
	}
	body := map[string]any{"name": name, "ephemeral": true, "labels": job.Labels}
	if err := f.do(ctx, "POST", "/actions/runners", body, &reg); err != nil {
		if he, ok := err.(*httpError); ok && he.code == 404 {
			return Launch{}, fmt.Errorf("forgejo: no ephemeral runners here, they need Forgejo 15 or later: %w", err)
		}
		return Launch{}, fmt.Errorf("forgejo: registering a runner: %w", err)
	}
	if reg.UUID == "" || reg.Token == "" {
		return Launch{}, fmt.Errorf("forgejo: the runner registration came back without a uuid or token")
	}
	// Job steps run on the clone itself: every label maps to the host executor.
	var args []string
	for _, l := range job.Labels {
		args = append(args, "--label "+shellQuote(l+":host"))
	}
	if job.Handle != "" {
		args = append(args, "--handle \"$ANVIL_JOB_HANDLE\"")
	}
	script := `umask 077
printf %s "$ANVIL_RUNNER_TOKEN" > /tmp/anvil-runner-token
unset ANVIL_RUNNER_TOKEN
exec forgejo-runner one-job --url "$ANVIL_FORGE_URL" --uuid "$ANVIL_RUNNER_UUID" --token-url file:///tmp/anvil-runner-token ` + strings.Join(args, " ") + ` --wait`
	return Launch{
		Script: script,
		Env:    []string{"ANVIL_FORGE_URL=" + f.url, "ANVIL_RUNNER_UUID=" + reg.UUID, "ANVIL_RUNNER_TOKEN=" + reg.Token, "ANVIL_JOB_HANDLE=" + job.Handle},
		// Forgejo drops an ephemeral runner once its job is done; this covers one that never got it.
		Cleanup: func(ctx context.Context) {
			if reg.ID != 0 {
				_ = f.do(ctx, "DELETE", fmt.Sprintf("/actions/runners/%d", reg.ID), nil, nil)
			}
		},
	}, nil
}

func (f *forgejo) Install(version string) (string, string) {
	v := strings.TrimPrefix(version, "v")
	name := "forgejo-runner " + v
	if v == "" {
		name = "forgejo-runner latest"
	}
	return name, toolsScript + `v=` + shellQuote(v) + `
if [ -z "$v" ]; then
	v=$(curl -fsSL https://code.forgejo.org/api/v1/repos/forgejo/runner/releases/latest | sed -n 's/.*"tag_name":"v\{0,1\}\([^"]*\)".*/\1/p')
fi
curl -fsSL -o /usr/local/bin/forgejo-runner "https://code.forgejo.org/forgejo/runner/releases/download/v$v/forgejo-runner-$v-linux-$goarch"
chmod 755 /usr/local/bin/forgejo-runner
forgejo-runner --version
`
}
