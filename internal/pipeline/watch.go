package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// Watch defaults and bounds.
const (
	DefaultInterval = time.Minute
	minInterval     = 10 * time.Second
)

// Watcher polls watched branches and runs their pipeline on each new commit.
type Watcher struct {
	Exec *Executor
	// Tick is how often watches are checked for being due.
	Tick time.Duration
	// TempDir holds clones while a run's source is packed.
	TempDir string

	mu      sync.Mutex
	running map[string]bool // watch name -> a check or run going on
	wg      sync.WaitGroup
}

func NewWatcher(e *Executor, tempDir string) *Watcher {
	return &Watcher{Exec: e, Tick: 5 * time.Second, TempDir: tempDir, running: map[string]bool{}}
}

var (
	watchName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)
	// scpLike is git's user@host:path form.
	scpLike = regexp.MustCompile(`^[a-zA-Z0-9._-]+@[a-zA-Z0-9.-]+:[^-]`)
	branch  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)
)

// IsRemote reports whether repo is a URL git fetches over the network.
func IsRemote(repo string) bool {
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(repo, p) {
			return true
		}
	}
	return scpLike.MatchString(repo)
}

// Normalize checks w and fills its defaults. A local repo path must be checked by the
// caller against w's owner, as anvild reads it.
func Normalize(w *store.PipelineWatch) error {
	if !watchName.MatchString(w.Name) {
		return fmt.Errorf("watch name %q: use letters, digits, '.', '_' and '-'", w.Name)
	}
	if w.Repo == "" || strings.HasPrefix(w.Repo, "-") || strings.Contains(w.Repo, "::") {
		return fmt.Errorf("watch %s: repo %q, want an https://, ssh://, git:// or user@host:path URL, or a local path", w.Name, w.Repo)
	}
	if !IsRemote(w.Repo) && !strings.HasPrefix(w.Repo, "/") {
		return fmt.Errorf("watch %s: repo %q: a local repo needs an absolute path", w.Name, w.Repo)
	}
	if w.Branch == "" {
		w.Branch = "main"
	}
	if !branch.MatchString(w.Branch) || strings.Contains(w.Branch, "..") {
		return fmt.Errorf("watch %s: bad branch %q", w.Name, w.Branch)
	}
	if w.File == "" {
		w.File = DefaultFile
	}
	if err := relPath(w.File); err != nil {
		return fmt.Errorf("watch %s: file: %w", w.Name, err)
	}
	if w.Interval == 0 {
		w.Interval = DefaultInterval
	}
	if w.Interval < minInterval {
		return fmt.Errorf("watch %s: an interval under %s would hammer the remote", w.Name, minInterval)
	}
	return nil
}

// Start checks watches every Tick until ctx ends.
func (w *Watcher) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(w.Tick)
		defer t.Stop()
		for {
			w.checkDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Wait waits for checks and runs in progress.
func (w *Watcher) Wait() { w.wg.Wait() }

func (w *Watcher) checkDue(ctx context.Context) {
	list, err := w.Exec.Store.ListPipelineWatches()
	if err != nil {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, pw := range list {
		if pw.Paused || time.Since(pw.LastPoll) < pw.Interval {
			continue
		}
		w.check(ctx, pw.Name, false)
	}
}

// Trigger runs a watch's pipeline on its branch now, even without a new commit.
func (w *Watcher) Trigger(ctx context.Context, name string) error {
	if _, err := w.Exec.Store.GetPipelineWatch(name); err != nil {
		return err
	}
	if !w.check(ctx, name, true) {
		return fmt.Errorf("watch %s is already checking or running", name)
	}
	return nil
}

// check looks at the branch's head in the background and runs on a new commit, or always
// with force. It returns false when the watch is busy.
func (w *Watcher) check(ctx context.Context, name string, force bool) bool {
	w.mu.Lock()
	if w.running[name] {
		w.mu.Unlock()
		return false
	}
	w.running[name] = true
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.running, name)
			w.mu.Unlock()
		}()
		if err := w.checkOne(ctx, name, force); err != nil && ctx.Err() == nil {
			log.Printf("pipeline watch %s: %v", name, err)
		}
	}()
	return true
}

func (w *Watcher) checkOne(ctx context.Context, name string, force bool) error {
	pw, err := w.Exec.Store.GetPipelineWatch(name)
	if err != nil {
		return err
	}
	update := func(change func(*store.PipelineWatch)) {
		// Re-read: the user may have paused or changed it meanwhile.
		cur, err := w.Exec.Store.GetPipelineWatch(name)
		if err != nil {
			return
		}
		change(&cur)
		_ = w.Exec.Store.PutPipelineWatch(cur)
	}
	commit, err := headCommit(ctx, pw.Repo, pw.Branch)
	update(func(c *store.PipelineWatch) {
		c.LastPoll, c.LastError = time.Now(), ""
		if err != nil {
			c.LastError = err.Error()
		}
	})
	if err != nil || (commit == pw.LastCommit && !force) {
		return err
	}
	tarPath, err := w.packCommit(ctx, pw.Repo, pw.Branch, commit)
	if err != nil {
		update(func(c *store.PipelineWatch) { c.LastError, c.LastCommit = err.Error(), commit })
		return err
	}
	// The commit counts as seen before the run, so a failing pipeline isn't run again.
	update(func(c *store.PipelineWatch) { c.LastCommit = commit })
	rec, err := w.Exec.Run(ctx, Source{Tar: tarPath, File: pw.File, Origin: "watch:" + name, Commit: commit, Owner: pw.Owner}, nil)
	_ = os.Remove(tarPath)
	update(func(c *store.PipelineWatch) {
		c.LastRun = rec.ID
		if err != nil {
			c.LastError = err.Error()
		}
	})
	return err
}

// gitCmd keeps git from prompting, and from protocols that run commands.
func gitCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PROTOCOL_FROM_USER=0", "GIT_ALLOW_PROTOCOL=https:http:ssh:git:file",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	return cmd
}

// headCommit is the commit branch points at in repo.
func headCommit(ctx context.Context, repo, branch string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := gitCmd(ctx, "ls-remote", "--", repo, "refs/heads/"+branch).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sha, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if len(sha) < 40 {
		return "", fmt.Errorf("no branch %s in %s", branch, repo)
	}
	return sha, nil
}

// packCommit clones branch at commit and packs what git tracks into a tar for a run.
func (w *Watcher) packCommit(ctx context.Context, repo, branch, commit string) (string, error) {
	if err := os.MkdirAll(w.TempDir, 0o750); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(w.TempDir, "clone-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if out, err := gitCmd(cctx, "clone", "--quiet", "--depth", "1", "--branch", branch, "--", repo, dir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	got, err := gitCmd(cctx, "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	if strings.TrimSpace(string(got)) != commit {
		return "", errors.New("the branch moved while cloning; the next check runs the new commit")
	}
	f, err := os.CreateTemp(w.TempDir, "source-*.tar")
	if err != nil {
		return "", err
	}
	err = client.WriteUploadTar(dir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
