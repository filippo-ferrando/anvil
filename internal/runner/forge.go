// Package runner takes jobs from Forgejo/Gitea or GitHub Actions and runs each one in its
// own ephemeral clone, with the forge's own runner inside it in one-job mode.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// Job is a forge job waiting for a runner.
type Job struct {
	ID     int64
	Name   string
	Labels []string
	Handle string // forgejo: picks this exact job for one-job
}

// Launch is what a clone runs for one job: a script, its secrets in the environment
// (never on the command line), and how to clean up after it.
type Launch struct {
	Script  string
	Env     []string
	Cleanup func(ctx context.Context)
}

// Forge is one forge's side of a runner.
type Forge interface {
	// Waiting lists the jobs waiting for a runner with one of labels.
	Waiting(ctx context.Context, labels []string) ([]Job, error)
	// Register makes single-use runner credentials for job, named name.
	Register(ctx context.Context, name string, job Job) (Launch, error)
	// Install is the setup script that puts the forge's runner in a warm snapshot.
	Install(version string) (name, script string)
}

// newForge returns the client for r's forge.
func newForge(r store.Runner) (Forge, error) {
	hc := &http.Client{Timeout: 30 * time.Second}
	switch r.Forge {
	case "forgejo", "gitea":
		return &forgejo{api: api{base: strings.TrimRight(r.URL, "/") + "/api/v1/" + strings.Trim(r.Scope, "/"), auth: "token " + r.Token, hc: hc}, url: strings.TrimRight(r.URL, "/")}, nil
	case "github":
		if !strings.HasPrefix(r.Scope, "repos/") {
			return nil, fmt.Errorf("a github runner takes a repos/<owner>/<repo> scope")
		}
		return &github{api: api{base: githubAPI(r.URL) + "/" + strings.Trim(r.Scope, "/"), auth: "Bearer " + r.Token, hc: hc,
			headers: map[string]string{"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}}}, nil
	}
	return nil, fmt.Errorf("unknown forge %q, want forgejo or github", r.Forge)
}

// githubAPI is the REST base for github.com or a GitHub Enterprise Server URL.
func githubAPI(url string) string {
	url = strings.TrimRight(url, "/")
	if url == "" || url == "https://github.com" || url == "https://api.github.com" {
		return "https://api.github.com"
	}
	return url + "/api/v3"
}

// api is a JSON client for one forge scope.
type api struct {
	base    string
	auth    string
	headers map[string]string
	hc      *http.Client
}

// do sends body as JSON (when not nil) to base+path and decodes the reply into out.
func (a api) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = a.base + path
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", a.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return &httpError{method: method, url: url, code: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

type httpError struct {
	method, url string
	code        int
	body        string
}

func (e *httpError) Error() string {
	body := e.body
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("%s %s: %d %s", e.method, e.url, e.code, body)
}

// shellQuote quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// toolsScript installs what jobs on the clone itself need: curl, git, and node for
// JavaScript actions like actions/checkout.
const toolsScript = `set -eu
if command -v apt-get >/dev/null 2>&1; then
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q && apt-get install -yq curl ca-certificates git nodejs tar
elif command -v dnf >/dev/null 2>&1; then
	dnf install -yq curl git nodejs tar
elif command -v apk >/dev/null 2>&1; then
	apk add --no-cache curl git nodejs bash tar
fi
case "$(uname -m)" in x86_64) arch=x64; goarch=amd64 ;; aarch64) arch=arm64; goarch=arm64 ;; *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; esac
`
