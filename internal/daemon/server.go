package daemon

import (
	"context"
	"errors"
	"log"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent"
)

// Server implements anvilv1.InstanceServiceServer against an
// *instance.Manager.
type Server struct {
	anvilv1.UnimplementedInstanceServiceServer
	Manager *instance.Manager
	Intents *intent.Manager
}

func NewServer(mgr *instance.Manager, intents *intent.Manager) *Server {
	return &Server{Manager: mgr, Intents: intents}
}

func (s *Server) Launch(req *anvilv1.LaunchRequest, stream anvilv1.InstanceService_LaunchServer) error {
	params := launchParamsFromPB(req)
	send := func(ev instance.LaunchEvent) {
		switch {
		case ev.Err != nil:
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Error{Error: ev.Err.Error()}})
		case ev.Instance != nil:
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Instance{Instance: specToPB(ev.Instance)}})
		case ev.Status != "":
			_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Status{Status: ev.Status}})
		}
	}
	// A launch with an intent_name joins (or creates) that intent instead
	// of producing a standalone instance.
	if params.IntentName != "" {
		return s.Intents.Launch(stream.Context(), params, send)
	}
	return s.Manager.Launch(stream.Context(), params, send)
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
		return nil, wrapErr(err)
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
	// Resolved before deleting so each instance's intent-membership label
	// is still available afterward.
	specs, err := s.Manager.Info(req.GetNames())
	if err != nil {
		return nil, err
	}

	// Deleted one instance at a time so a failure on one instance doesn't
	// block deletion or intent cleanup of the others.
	var errs []error
	for _, spec := range specs {
		if err := s.Manager.Delete(ctx, []string{spec.Name}, req.GetPurge()); err != nil {
			errs = append(errs, err)
			continue
		}

		// Best-effort cleanup of the deleted instance's intent membership.
		intentName := spec.Labels["intent"]
		if intentName == "" {
			continue
		}
		if _, err := s.Intents.Remove(ctx, intentName, spec.ID); err != nil {
			log.Printf("daemon: removing deleted instance %s from intent %q: %v", spec.Name, intentName, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
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

func (s *Server) Stats(ctx context.Context, req *anvilv1.StatsRequest) (*anvilv1.StatsReply, error) {
	stats, err := s.Manager.Stats(ctx, req.GetName())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.StatsReply{Stats: statsToPB(stats)}, nil
}
