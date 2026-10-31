package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/apply"
	"git.fferrando.cc/rdfilippo/anvil/internal/pipeline"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
	"git.fferrando.cc/rdfilippo/anvil/pkg/client"
)

// PipelineServer serves PipelineService. Watches run under Base, not under a call.
type PipelineServer struct {
	anvilv1.UnimplementedPipelineServiceServer
	Exec      *pipeline.Executor
	Watcher   *pipeline.Watcher
	UploadDir string
	Base      context.Context
}

func NewPipelineServer(base context.Context, e *pipeline.Executor, w *pipeline.Watcher, uploadDir string) *PipelineServer {
	return &PipelineServer{Exec: e, Watcher: w, UploadDir: uploadDir, Base: base}
}

// PipelineApply applies a pipeline's anvil.yaml for uid, with its host paths checked
// against uid the same way `anvil apply` checks them against its caller.
func PipelineApply(a *apply.Applier) pipeline.ApplyFunc {
	return func(ctx context.Context, uid uint32, data []byte, progress func(string)) error {
		m, err := apply.Parse(data, nil)
		if err != nil {
			return err
		}
		if err := checkUIDOwns(uid, m.VolumePaths()...); err != nil {
			return err
		}
		return a.Apply(ctx, m, apply.Options{}, nil, progress)
	}
}

func pipelineRunToPB(r store.PipelineRun) *anvilv1.PipelineRun {
	out := &anvilv1.PipelineRun{
		Id: r.ID, Source: r.Source, File: r.File, Commit: r.Commit, State: r.State, Error: r.Error, Applied: r.Applied,
		StartedAtUnix: r.StartedAt.Unix(),
	}
	if !r.EndedAt.IsZero() {
		out.EndedAtUnix = r.EndedAt.Unix()
	}
	for _, j := range r.Jobs {
		out.Jobs = append(out.Jobs, pipelineJobToPB(j))
	}
	return out
}

func pipelineJobToPB(j store.PipelineJob) *anvilv1.PipelineJob {
	return &anvilv1.PipelineJob{Name: j.Name, Distro: j.Distro, State: j.State, ExitCode: int32(j.ExitCode), Error: j.Error, DurationMs: j.Duration.Milliseconds()}
}

func pipelineEventToPB(ev pipeline.Event) *anvilv1.PipelineEvent {
	switch {
	case ev.Run != nil:
		return &anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Run{Run: pipelineRunToPB(*ev.Run)}}
	case ev.Progress != nil:
		p := ev.Progress
		return &anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Progress{Progress: &anvilv1.PipelineProgress{Job: p.Job, Distro: p.Distro, Message: p.Message}}}
	case ev.Output != nil:
		o := ev.Output
		st := anvilv1.OutputStream_OUTPUT_STREAM_STDOUT
		if o.Stderr {
			st = anvilv1.OutputStream_OUTPUT_STREAM_STDERR
		}
		return &anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Output{Output: &anvilv1.PipelineOutput{Job: o.Job, Distro: o.Distro, Stream: st, Data: o.Data}}}
	default:
		return &anvilv1.PipelineEvent{Event: &anvilv1.PipelineEvent_Job{Job: pipelineJobToPB(*ev.Job)}}
	}
}

// run runs src and streams it to send until it ends.
func (s *PipelineServer) run(ctx context.Context, src pipeline.Source, send func(*anvilv1.PipelineEvent) error) error {
	var sendErr error
	_, err := s.Exec.Run(ctx, src, func(ev pipeline.Event) {
		if sendErr == nil {
			sendErr = send(pipelineEventToPB(ev))
		}
	})
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return sendErr
}

func (s *PipelineServer) Run(stream anvilv1.PipelineService_RunServer) error {
	ctx := stream.Context()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	req := first.GetRequest()
	if req == nil {
		return status.Error(codes.InvalidArgument, "pipeline: a run starts with its request")
	}
	uid, ok := callerUID(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "pipeline: can't identify the caller")
	}
	if err := os.MkdirAll(s.UploadDir, 0o750); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.UploadDir, "pipeline-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	for {
		in, err := stream.Recv()
		if err == nil && in.GetUploadDone() {
			break
		}
		if err == nil && in.GetUploadChunk() == nil {
			err = status.Error(codes.InvalidArgument, "pipeline: the upload was interrupted")
		}
		if err == nil {
			_, err = f.Write(in.GetUploadChunk())
		}
		if err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return s.run(ctx, pipeline.Source{Tar: f.Name(), File: req.GetFile(), Origin: "manual", Owner: uid}, stream.Send)
}

func (s *PipelineServer) Rerun(req *anvilv1.PipelineRerunRequest, stream anvilv1.PipelineService_RerunServer) error {
	old, err := s.resolveRun(req.GetId())
	if err != nil {
		return err
	}
	uid, ok := callerUID(stream.Context())
	if !ok {
		return status.Error(codes.PermissionDenied, "pipeline: can't identify the caller")
	}
	// A copy, since a run takes its source tar over.
	if err := os.MkdirAll(s.UploadDir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.UploadDir, "rerun-*.tar")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	src, err := os.Open(filepath.Join(s.Exec.RunDir(old.ID), "source.tar"))
	if err == nil {
		_, err = io.Copy(tmp, src)
		src.Close()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "pipeline: the source of run %s is gone: %v", old.ID, err)
	}
	return s.run(stream.Context(), pipeline.Source{Tar: tmp.Name(), File: old.File, Origin: "rerun:" + old.ID, Commit: old.Commit, Owner: uid}, stream.Send)
}

// resolveRun finds the one run whose id starts with prefix.
func (s *PipelineServer) resolveRun(prefix string) (store.PipelineRun, error) {
	runs, err := s.Exec.Store.ListPipelineRuns()
	if err != nil {
		return store.PipelineRun{}, err
	}
	var found []store.PipelineRun
	for _, r := range runs {
		if prefix != "" && strings.HasPrefix(r.ID, strings.ToUpper(prefix)) {
			found = append(found, r)
		}
	}
	switch {
	case len(found) == 0:
		return store.PipelineRun{}, status.Errorf(codes.NotFound, "pipeline: no run %q", prefix)
	case len(found) > 1:
		return store.PipelineRun{}, status.Errorf(codes.InvalidArgument, "pipeline: %q matches %d runs, give more of the id", prefix, len(found))
	}
	return found[0], nil
}

func (s *PipelineServer) Cancel(_ context.Context, req *anvilv1.PipelineCancelRequest) (*anvilv1.PipelineCancelReply, error) {
	r, err := s.resolveRun(req.GetId())
	if err != nil {
		return nil, err
	}
	return &anvilv1.PipelineCancelReply{}, wrapErr(s.Exec.Cancel(r.ID))
}

func (s *PipelineServer) ListRuns(_ context.Context, req *anvilv1.PipelineListRunsRequest) (*anvilv1.PipelineListRunsReply, error) {
	runs, err := s.Exec.Store.ListPipelineRuns()
	if err != nil {
		return nil, err
	}
	if n := int(req.GetLimit()); n > 0 && len(runs) > n {
		runs = runs[:n]
	}
	reply := &anvilv1.PipelineListRunsReply{}
	for _, r := range runs {
		reply.Runs = append(reply.Runs, pipelineRunToPB(r))
	}
	return reply, nil
}

func (s *PipelineServer) GetRun(_ context.Context, req *anvilv1.PipelineGetRunRequest) (*anvilv1.PipelineRun, error) {
	r, err := s.resolveRun(req.GetId())
	if err != nil {
		return nil, err
	}
	return pipelineRunToPB(r), nil
}

// pipelineChunk is how much of a log or artifact tar one message carries.
const pipelineChunk = 256 << 10

func (s *PipelineServer) Logs(req *anvilv1.PipelineLogsRequest, stream anvilv1.PipelineService_LogsServer) error {
	r, err := s.resolveRun(req.GetId())
	if err != nil {
		return err
	}
	var logs []store.PipelineJob
	for _, j := range r.Jobs {
		if (req.GetJob() == "" || j.Name == req.GetJob()) && (req.GetDistro() == "" || j.Distro == req.GetDistro()) {
			logs = append(logs, j)
		}
	}
	if len(logs) == 0 {
		return status.Errorf(codes.NotFound, "pipeline: run %s has no job %s %s", r.ID, req.GetJob(), req.GetDistro())
	}
	buf := make([]byte, pipelineChunk)
	for _, j := range logs {
		if len(logs) > 1 {
			if err := stream.Send(&anvilv1.PipelineChunk{Data: fmt.Appendf(nil, "==> %s on %s (%s) <==\n", j.Name, j.Distro, j.State)}); err != nil {
				return err
			}
		}
		f, err := os.Open(s.Exec.LogPath(r.ID, j.Name, j.Distro))
		if os.IsNotExist(err) {
			continue // never started
		}
		if err != nil {
			return err
		}
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if err := stream.Send(&anvilv1.PipelineChunk{Data: append([]byte(nil), buf[:n]...)}); err != nil {
					f.Close()
					return err
				}
			}
			if rerr != nil {
				break
			}
		}
		f.Close()
	}
	return nil
}

func (s *PipelineServer) Artifacts(req *anvilv1.PipelineArtifactsRequest, stream anvilv1.PipelineService_ArtifactsServer) error {
	r, err := s.resolveRun(req.GetId())
	if err != nil {
		return err
	}
	dir := filepath.Join(s.Exec.RunDir(r.ID), "artifacts")
	if _, err := os.Stat(dir); err != nil {
		return status.Errorf(codes.NotFound, "pipeline: run %s collected no files", r.ID)
	}
	return client.SendUpload(dir, func(chunk []byte) error { return stream.Send(&anvilv1.PipelineChunk{Data: chunk}) })
}

func watchToPB(w store.PipelineWatch) *anvilv1.PipelineWatch {
	out := &anvilv1.PipelineWatch{
		Name: w.Name, Repo: w.Repo, Branch: w.Branch, File: w.File, IntervalSeconds: int64(w.Interval / time.Second), Paused: w.Paused,
		LastCommit: w.LastCommit, LastRun: w.LastRun, LastError: w.LastError,
	}
	if !w.LastPoll.IsZero() {
		out.LastPollUnix = w.LastPoll.Unix()
	}
	return out
}

func (s *PipelineServer) AddWatch(ctx context.Context, req *anvilv1.PipelineWatchAddRequest) (*anvilv1.PipelineWatch, error) {
	in := req.GetWatch()
	uid, ok := callerUID(ctx)
	if !ok {
		return nil, status.Error(codes.PermissionDenied, "pipeline: can't identify the caller")
	}
	w := store.PipelineWatch{
		Name: in.GetName(), Repo: in.GetRepo(), Branch: in.GetBranch(), File: in.GetFile(),
		Interval: time.Duration(in.GetIntervalSeconds()) * time.Second, Paused: in.GetPaused(), Owner: uid, CreatedAt: time.Now(),
	}
	if err := pipeline.Normalize(&w); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// anvild reads a local repo with its own rights, so only one the caller owns.
	if !pipeline.IsRemote(w.Repo) {
		if err := checkUIDOwns(uid, &w.Repo); err != nil {
			return nil, err
		}
	}
	if _, err := s.Exec.Store.GetPipelineWatch(w.Name); err == nil {
		return nil, status.Errorf(codes.AlreadyExists, "pipeline: a watch named %s already exists", w.Name)
	}
	if err := s.Exec.Store.PutPipelineWatch(w); err != nil {
		return nil, err
	}
	return watchToPB(w), nil
}

func (s *PipelineServer) ListWatches(context.Context, *anvilv1.PipelineWatchListRequest) (*anvilv1.PipelineWatchListReply, error) {
	list, err := s.Exec.Store.ListPipelineWatches()
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	reply := &anvilv1.PipelineWatchListReply{}
	for _, w := range list {
		reply.Watches = append(reply.Watches, watchToPB(w))
	}
	return reply, nil
}

func (s *PipelineServer) RemoveWatch(_ context.Context, req *anvilv1.PipelineWatchRemoveRequest) (*anvilv1.PipelineWatchRemoveReply, error) {
	return &anvilv1.PipelineWatchRemoveReply{}, wrapErr(s.Exec.Store.DeletePipelineWatch(req.GetName()))
}

func (s *PipelineServer) SetWatchPaused(_ context.Context, req *anvilv1.PipelineWatchPauseRequest) (*anvilv1.PipelineWatch, error) {
	w, err := s.Exec.Store.GetPipelineWatch(req.GetName())
	if err != nil {
		return nil, wrapErr(err)
	}
	w.Paused = req.GetPaused()
	if err := s.Exec.Store.PutPipelineWatch(w); err != nil {
		return nil, err
	}
	return watchToPB(w), nil
}

func (s *PipelineServer) TriggerWatch(_ context.Context, req *anvilv1.PipelineWatchTriggerRequest) (*anvilv1.PipelineWatchTriggerReply, error) {
	if err := s.Watcher.Trigger(s.Base, req.GetName()); err != nil {
		if _, gerr := s.Exec.Store.GetPipelineWatch(req.GetName()); gerr != nil {
			return nil, wrapErr(gerr)
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &anvilv1.PipelineWatchTriggerReply{}, nil
}
