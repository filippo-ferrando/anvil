package daemon

import (
	"context"
	"log"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent"
)

// Server implements anvilv1.InstanceServiceServer against an
// *instance.Manager. It holds no state of its own beyond that — all
// business logic lives in internal/instance and the backends it dispatches
// to, per the plan's "daemon owns all logic, clients are thin" mandate.
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
	// of producing a standalone instance — see internal/intent.Manager's
	// doc comment for why that routing decision lives here rather than
	// inside instance.Manager.Launch itself.
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
	// Resolved before deleting, not after: once gone, an instance's own
	// Labels (where its intent membership is tagged) aren't retrievable
	// through the normal Info path any more.
	specs, err := s.Manager.Info(req.GetNames())
	if err != nil {
		return nil, err
	}

	if err := s.Manager.Delete(ctx, req.GetNames(), req.GetPurge()); err != nil {
		return nil, err
	}

	// `anvil delete` (as opposed to `anvil intent remove`/`delete`) doesn't
	// go through internal/intent at all, so a deleted member would
	// otherwise leave a stale entry behind in its intent's Members list
	// forever, pointing at an instance ID that no longer exists — best-
	// effort cleanup here, not fatal to Delete itself if it fails (the
	// instance is already gone regardless).
	for _, spec := range specs {
		intentName := spec.Labels["intent"]
		if intentName == "" {
			continue
		}
		if _, err := s.Intents.Remove(intentName, spec.ID); err != nil {
			log.Printf("daemon: removing deleted instance %s from intent %q: %v", spec.Name, intentName, err)
		}
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
