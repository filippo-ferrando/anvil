package daemon

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/runner"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// RunnerServer serves RunnerService. Runner loops run under Base, not under the call
// that added them.
type RunnerServer struct {
	anvilv1.UnimplementedRunnerServiceServer
	Manager *runner.Manager
	Base    context.Context
}

func NewRunnerServer(base context.Context, m *runner.Manager) *RunnerServer {
	return &RunnerServer{Manager: m, Base: base}
}

func runnerToPB(in runner.Info) *anvilv1.Runner {
	r := in.Runner
	out := &anvilv1.Runner{
		Name: r.Name, Forge: r.Forge, Url: r.URL, Scope: r.Scope, Labels: r.Labels, Cpus: int32(r.CPUs), MemoryMib: r.MemoryMiB,
		MaxJobs: int32(r.MaxJobs), Version: r.Version, Paused: r.Paused, Running: int32(in.Running), LastError: in.LastErr,
		CreatedAtUnix: r.CreatedAt.Unix(),
	}
	if !in.LastPoll.IsZero() {
		out.LastPollUnix = in.LastPoll.Unix()
	}
	return out
}

func runnerJobToPB(j store.RunnerJob) *anvilv1.RunnerJob {
	out := &anvilv1.RunnerJob{
		Id: j.ID, Runner: j.Runner, JobId: j.JobID, Name: j.Name, Labels: j.Labels, Distro: j.Distro, CloneId: j.CloneID,
		State: j.State, ExitCode: int32(j.ExitCode), Error: j.Error, StartedAtUnix: j.StartedAt.Unix(),
	}
	if !j.EndedAt.IsZero() {
		out.EndedAtUnix = j.EndedAt.Unix()
	}
	return out
}

func (s *RunnerServer) Add(_ context.Context, req *anvilv1.RunnerAddRequest) (*anvilv1.RunnerAddReply, error) {
	r := req.GetRunner()
	if r == nil {
		return nil, status.Error(codes.InvalidArgument, "runner: add needs a runner")
	}
	info, err := s.Manager.Add(s.Base, store.Runner{
		Name: r.GetName(), Forge: r.GetForge(), URL: r.GetUrl(), Scope: r.GetScope(), Token: req.GetToken(), Labels: r.GetLabels(),
		CPUs: int(r.GetCpus()), MemoryMiB: r.GetMemoryMib(), MaxJobs: int(r.GetMaxJobs()), Version: r.GetVersion(), Paused: r.GetPaused(),
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &anvilv1.RunnerAddReply{Runner: runnerToPB(info)}, nil
}

func (s *RunnerServer) List(context.Context, *anvilv1.RunnerListRequest) (*anvilv1.RunnerListReply, error) {
	list, err := s.Manager.List()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.RunnerListReply{}
	for _, in := range list {
		reply.Runners = append(reply.Runners, runnerToPB(in))
	}
	return reply, nil
}

func (s *RunnerServer) Remove(_ context.Context, req *anvilv1.RunnerRemoveRequest) (*anvilv1.RunnerRemoveReply, error) {
	return &anvilv1.RunnerRemoveReply{}, wrapErr(s.Manager.Remove(req.GetName()))
}

func (s *RunnerServer) SetPaused(_ context.Context, req *anvilv1.RunnerSetPausedRequest) (*anvilv1.RunnerSetPausedReply, error) {
	info, err := s.Manager.SetPaused(req.GetName(), req.GetPaused())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.RunnerSetPausedReply{Runner: runnerToPB(info)}, nil
}

func (s *RunnerServer) Jobs(_ context.Context, req *anvilv1.RunnerJobsRequest) (*anvilv1.RunnerJobsReply, error) {
	jobs, err := s.Manager.Jobs(req.GetRunner())
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.RunnerJobsReply{}
	for _, j := range jobs {
		reply.Jobs = append(reply.Jobs, runnerJobToPB(j))
	}
	return reply, nil
}
