package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/pipeline"
	"git.fferrando.cc/rdfilippo/anvil/internal/runner"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// echoRuns passes every distro, writing the command as output and collecting one file.
type echoRuns struct{}

func (echoRuns) Run(_ context.Context, r ephemeral.RunRequest, emit func(ephemeral.Event)) error {
	for _, d := range r.Distros {
		emit(ephemeral.Event{Output: &ephemeral.Output{Distro: d, Data: []byte(r.Argv[2] + "\n")}})
		emit(ephemeral.Event{Exit: &ephemeral.Exit{Distro: d}})
	}
	return nil
}

// ciClient serves the runner and pipeline services on a unix socket, with the caller's uid.
func ciClient(t *testing.T) (*client.Client, *pipeline.Executor) {
	t.Helper()
	db := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp("", "anvil-ci")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	exec := pipeline.NewExecutor(db, echoRuns{}, filepath.Join(dir, "runs"), nil)
	srv := grpc.NewServer()
	anvilv1.RegisterPipelineServiceServer(srv, NewPipelineServer(ctx, exec, pipeline.NewWatcher(exec, filepath.Join(dir, "clones")), filepath.Join(dir, "up")))
	anvilv1.RegisterRunnerServiceServer(srv, NewRunnerServer(ctx, runner.NewManager(db, echoRuns{})))
	go func() { _ = srv.Serve(CallerListener(lis)) }()
	t.Cleanup(srv.Stop)
	c, err := client.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, exec
}

func TestPipelineServer(t *testing.T) {
	c, exec := ciClient(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "anvil-ci.yaml"), []byte("jobs:\n  build: {distros: [u1, u2], run: make build}\n"), 0o644)

	stream, err := c.Pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_Request{Request: &anvilv1.PipelineRunRequest{}}})
	if err := client.SendUpload(src, func(b []byte) error {
		return stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadChunk{UploadChunk: b}})
	}); err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&anvilv1.PipelineRunInput{Input: &anvilv1.PipelineRunInput_UploadDone{UploadDone: true}})
	var last *anvilv1.PipelineRun
	var out bytes.Buffer
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.GetRun() != nil {
			last = ev.GetRun()
		}
		out.Write(ev.GetOutput().GetData())
	}
	if last == nil || last.GetState() != "succeeded" || len(last.GetJobs()) != 2 || out.String() != "make build\nmake build\n" {
		t.Fatalf("run = %+v, output %q", last, out.String())
	}
	stored, _ := exec.Store.GetPipelineRun(last.GetId())
	if stored.Owner != uint32(os.Getuid()) || stored.Source != "manual" {
		t.Errorf("stored run = %+v", stored)
	}

	// By a unique start of the id, and its logs.
	got, err := c.Pipeline.GetRun(ctx, &anvilv1.PipelineGetRunRequest{Id: strings.ToLower(last.GetId()[:20])})
	if err != nil || got.GetId() != last.GetId() {
		t.Errorf("GetRun = %v, %v", got, err)
	}
	logs, _ := c.Pipeline.Logs(ctx, &anvilv1.PipelineLogsRequest{Id: last.GetId()})
	var text bytes.Buffer
	for {
		ch, err := logs.Recv()
		if err != nil {
			break
		}
		text.Write(ch.GetData())
	}
	if !strings.Contains(text.String(), "==> build on u2 (succeeded) <==\nmake build\n") {
		t.Errorf("logs = %q", text.String())
	}
	if _, err := c.Pipeline.GetRun(ctx, &anvilv1.PipelineGetRunRequest{Id: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown run = %v", err)
	}
	art, _ := c.Pipeline.Artifacts(ctx, &anvilv1.PipelineArtifactsRequest{Id: last.GetId()})
	if _, err := art.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("artifacts of a run without any = %v", err)
	}

	// Rerun copies the source; the old run keeps it.
	rerun, err := c.Pipeline.Rerun(ctx, &anvilv1.PipelineRerunRequest{Id: last.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	var re *anvilv1.PipelineRun
	for {
		ev, err := rerun.Recv()
		if err != nil {
			break
		}
		if ev.GetRun() != nil {
			re = ev.GetRun()
		}
	}
	if re == nil || re.GetState() != "succeeded" || re.GetSource() != "rerun:"+last.GetId() {
		t.Errorf("rerun = %+v", re)
	}
	if list, _ := c.Pipeline.ListRuns(ctx, &anvilv1.PipelineListRunsRequest{Limit: 1}); len(list.GetRuns()) != 1 || list.GetRuns()[0].GetId() != re.GetId() {
		t.Errorf("ListRuns = %+v", list)
	}

	// Watches: a local repo the caller owns, a remote one, and bad ones.
	repo := t.TempDir()
	if _, err := c.Pipeline.AddWatch(ctx, &anvilv1.PipelineWatchAddRequest{Watch: &anvilv1.PipelineWatch{Name: "local", Repo: repo}}); err != nil {
		t.Errorf("local watch: %v", err)
	}
	w, err := c.Pipeline.AddWatch(ctx, &anvilv1.PipelineWatchAddRequest{Watch: &anvilv1.PipelineWatch{Name: "app", Repo: "https://code.example/app.git", Paused: true}})
	if err != nil || w.GetBranch() != "main" || w.GetIntervalSeconds() != 60 {
		t.Errorf("AddWatch = %+v, %v", w, err)
	}
	if _, err := c.Pipeline.AddWatch(ctx, &anvilv1.PipelineWatchAddRequest{Watch: &anvilv1.PipelineWatch{Name: "app", Repo: "https://x/y"}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate watch = %v", err)
	}
	if _, err := c.Pipeline.AddWatch(ctx, &anvilv1.PipelineWatchAddRequest{Watch: &anvilv1.PipelineWatch{Name: "x", Repo: "ext::sh"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ext:: watch = %v", err)
	}
	if w, err := c.Pipeline.SetWatchPaused(ctx, &anvilv1.PipelineWatchPauseRequest{Name: "app", Paused: false}); err != nil || w.GetPaused() {
		t.Errorf("resume = %+v, %v", w, err)
	}
	if list, _ := c.Pipeline.ListWatches(ctx, &anvilv1.PipelineWatchListRequest{}); len(list.GetWatches()) != 2 || list.GetWatches()[0].GetName() != "app" {
		t.Errorf("ListWatches = %+v", list)
	}
	if _, err := c.Pipeline.TriggerWatch(ctx, &anvilv1.PipelineWatchTriggerRequest{Name: "nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("trigger unknown = %v", err)
	}
	if _, err := c.Pipeline.RemoveWatch(ctx, &anvilv1.PipelineWatchRemoveRequest{Name: "app"}); err != nil {
		t.Error(err)
	}
}

func TestPipelineApplyChecksOwner(t *testing.T) {
	apply := PipelineApply(nil) // the check comes before the applier is used
	manifest := []byte("intent: web\nmembers:\n  db:\n    kind: container\n    image: postgres\n    volumes: [\"/:/data\"]\n")
	err := apply(context.Background(), 12345, manifest, func(string) {})
	if os.Getuid() != 0 && (err == nil || status.Code(err) != codes.PermissionDenied) {
		t.Errorf("a volume on / for uid 12345 = %v", err)
	}
	if err := apply(context.Background(), 12345, []byte("not: [yaml"), nil); err == nil {
		t.Error("a broken manifest was applied")
	}
}

func TestRunnerServer(t *testing.T) {
	c, _ := ciClient(t)
	ctx := context.Background()
	add := &anvilv1.RunnerAddRequest{Token: "secret", Runner: &anvilv1.Runner{
		Name: "ci", Forge: "forgejo", Url: "https://127.0.0.1:1", Scope: "repos/a/b", Labels: map[string]string{"debian": "debian-13"}, Paused: true}}
	reply, err := c.Runner.Add(ctx, add)
	if err != nil || reply.GetRunner().GetMaxJobs() != 2 || !reply.GetRunner().GetPaused() {
		t.Fatalf("Add = %+v, %v", reply, err)
	}
	if _, err := c.Runner.Add(ctx, add); status.Code(err) != codes.InvalidArgument {
		t.Errorf("duplicate = %v", err)
	}
	add.Runner.Name, add.Token = "x", ""
	if _, err := c.Runner.Add(ctx, add); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no token = %v", err)
	}
	list, err := c.Runner.List(ctx, &anvilv1.RunnerListRequest{})
	if err != nil || len(list.GetRunners()) != 1 || list.GetRunners()[0].GetLabels()["debian"] != "debian-13" {
		t.Errorf("List = %+v, %v", list, err)
	}
	if strings.Contains(list.String(), "secret") {
		t.Error("the token came back")
	}
	if r, err := c.Runner.SetPaused(ctx, &anvilv1.RunnerSetPausedRequest{Name: "ci", Paused: false}); err != nil || r.GetRunner().GetPaused() {
		t.Errorf("resume = %+v, %v", r, err)
	}
	time.Sleep(50 * time.Millisecond) // a poll against nothing records its error, not a crash
	if jobs, err := c.Runner.Jobs(ctx, &anvilv1.RunnerJobsRequest{}); err != nil || len(jobs.GetJobs()) != 0 {
		t.Errorf("Jobs = %+v, %v", jobs, err)
	}
	if _, err := c.Runner.Remove(ctx, &anvilv1.RunnerRemoveRequest{Name: "ci"}); err != nil {
		t.Error(err)
	}
	if _, err := c.Runner.Remove(ctx, &anvilv1.RunnerRemoveRequest{Name: "ci"}); status.Code(err) != codes.NotFound {
		t.Errorf("remove twice = %v", err)
	}
}
