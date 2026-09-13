package daemon

import (
	"context"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
)

// Server implements anvilv1.InstanceServiceServer against an
// *instance.Manager. It holds no state of its own beyond that — all
// business logic lives in internal/instance and the backends it dispatches
// to, per the plan's "daemon owns all logic, clients are thin" mandate.
type Server struct {
	anvilv1.UnimplementedInstanceServiceServer
	Manager *instance.Manager
}

func NewServer(mgr *instance.Manager) *Server {
	return &Server{Manager: mgr}
}

func (s *Server) Launch(req *anvilv1.LaunchRequest, stream anvilv1.InstanceService_LaunchServer) error {
	params := launchParamsFromPB(req)
	return s.Manager.Launch(stream.Context(), params, func(ev instance.LaunchEvent) {
		switch {
		case ev.Err != nil:
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Error{Error: ev.Err.Error()}})
		case ev.Instance != nil:
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Instance{Instance: specToPB(ev.Instance)}})
		case ev.Status != "":
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Status{Status: ev.Status}})
		}
	})
}

func (s *Server) List(ctx context.Context, req *anvilv1.ListRequest) (*anvilv1.ListReply, error) {
	specs, err := s.Manager.List(kindFromPB(req.GetKindFilter()))
	if err != nil {
		return nil, err
	}
	return &anvilv1.ListReply{Instances: specsToPB(specs)}, nil
}

func (s *Server) Info(ctx context.Context, req *anvilv1.InfoRequest) (*anvilv1.InfoReply, error) {
	specs, err := s.Manager.Info(req.GetNames())
	if err != nil {
		return nil, err
	}
	return &anvilv1.InfoReply{Instances: specsToPB(specs)}, nil
}

func (s *Server) Start(ctx context.Context, req *anvilv1.StartRequest) (*anvilv1.StartReply, error) {
	if err := s.Manager.Start(ctx, req.GetNames()); err != nil {
		return nil, err
	}
	return &anvilv1.StartReply{}, nil
}

func (s *Server) Stop(ctx context.Context, req *anvilv1.StopRequest) (*anvilv1.StopReply, error) {
	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := s.Manager.Stop(ctx, req.GetNames(), req.GetForce(), timeout); err != nil {
		return nil, err
	}
	return &anvilv1.StopReply{}, nil
}

func (s *Server) Delete(ctx context.Context, req *anvilv1.DeleteRequest) (*anvilv1.DeleteReply, error) {
	if err := s.Manager.Delete(ctx, req.GetNames(), req.GetPurge()); err != nil {
		return nil, err
	}
	return &anvilv1.DeleteReply{}, nil
}

func (s *Server) Purge(ctx context.Context, req *anvilv1.PurgeRequest) (*anvilv1.PurgeReply, error) {
	if err := s.Manager.Purge(req.GetNames()); err != nil {
		return nil, err
	}
	return &anvilv1.PurgeReply{}, nil
}

func (s *Server) Logs(req *anvilv1.LogsRequest, stream anvilv1.InstanceService_LogsServer) error {
	return s.Manager.Logs(stream.Context(), req.GetName(), req.GetFollow(), int(req.GetTailLines()), func(chunk []byte) error {
		return stream.Send(&anvilv1.LogChunk{Data: chunk})
	})
}

func (s *Server) Mount(ctx context.Context, req *anvilv1.MountRequest) (*anvilv1.MountReply, error) {
	if err := s.Manager.Mount(ctx, req.GetName(), req.GetHostPath(), req.GetGuestPath(), req.GetReadOnly()); err != nil {
		return nil, err
	}
	return &anvilv1.MountReply{}, nil
}

func (s *Server) Umount(ctx context.Context, req *anvilv1.UmountRequest) (*anvilv1.UmountReply, error) {
	if err := s.Manager.Umount(ctx, req.GetName(), req.GetGuestPath()); err != nil {
		return nil, err
	}
	return &anvilv1.UmountReply{}, nil
}
