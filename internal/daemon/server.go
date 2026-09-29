package daemon

import (
	"context"
	"errors"
	"fmt"
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

// launchProgressSender is the subset of *anvilv1.InstanceService_LaunchServer
// and *anvilv1.InstanceService_ForkServer that sendLaunchEvent needs; both stream a LaunchProgress.
type launchProgressSender interface {
	Send(*anvilv1.LaunchProgress) error
}

func sendLaunchEvent(stream launchProgressSender, ev instance.LaunchEvent) {
	switch {
	case ev.Err != nil:
		_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Error{Error: ev.Err.Error()}})
	case ev.Instance != nil:
		_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Instance{Instance: specToPB(ev.Instance)}})
	case ev.Status != "":
		_ = stream.Send(&anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Status{Status: ev.Status}})
	}
}

// defaultWaitTimeout bounds launch --wait and WaitReady when the request sets no timeout.
const defaultWaitTimeout = 15 * time.Minute

func (s *Server) Launch(req *anvilv1.LaunchRequest, stream anvilv1.InstanceService_LaunchServer) error {
	params := launchParamsFromPB(req)
	ctx := stream.Context()
	wait := req.GetWait() && !params.NoStart

	// With wait, the final Instance event is held back until the guest is ready.
	var launched *instance.Spec
	send := func(ev instance.LaunchEvent) {
		if wait && ev.Instance != nil {
			launched = ev.Instance
			return
		}
		sendLaunchEvent(stream, ev)
	}
	// A launch with an intent_name joins (or creates) that intent instead
	// of producing a standalone instance.
	var err error
	if params.IntentName != "" {
		err = s.Intents.Launch(ctx, params, send)
	} else {
		err = s.Manager.Launch(ctx, params, send)
	}
	if err != nil || !wait || launched == nil {
		return err
	}
	return s.waitReady(ctx, launched.Name, req.GetWaitTimeoutSeconds(), stream)
}

func (s *Server) WaitReady(req *anvilv1.WaitReadyRequest, stream anvilv1.InstanceService_WaitReadyServer) error {
	return s.waitReady(stream.Context(), req.GetName(), req.GetTimeoutSeconds(), stream)
}

// waitReady streams wait progress for name, then its fresh record or the error.
func (s *Server) waitReady(ctx context.Context, name string, timeoutSeconds int32, stream launchProgressSender) error {
	timeout := time.Duration(timeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	progress := func(status string) { sendLaunchEvent(stream, instance.LaunchEvent{Status: status}) }
	if err := s.Manager.WaitReady(ctx, name, progress); err != nil {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
		return nil // reported in-band, like a failed launch
	}
	specs, err := s.Manager.Info([]string{name})
	if err != nil || len(specs) == 0 {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: fmt.Errorf("instance: reading %s after waiting: %v", name, err)})
		return nil
	}
	sendLaunchEvent(stream, instance.LaunchEvent{Instance: specs[0]})
	return nil
}

func (s *Server) Fork(req *anvilv1.ForkRequest, stream anvilv1.InstanceService_ForkServer) error {
	send := func(ev instance.LaunchEvent) { sendLaunchEvent(stream, ev) }
	return s.Manager.Fork(stream.Context(), instance.ForkParams{
		Source:  req.GetName(),
		NewName: req.GetNewName(),
		Start:   req.GetStart(),
	}, send)
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

func (s *Server) AddPort(ctx context.Context, req *anvilv1.AddPortRequest) (*anvilv1.AddPortReply, error) {
	p := req.GetPort()
	port := instance.PortMapping{HostPort: int(p.GetHostPort()), GuestPort: int(p.GetGuestPort()), Protocol: p.GetProtocol()}
	if err := s.Manager.AddPort(ctx, req.GetName(), port); err != nil {
		return nil, err
	}
	return &anvilv1.AddPortReply{}, nil
}

func (s *Server) RemovePort(ctx context.Context, req *anvilv1.RemovePortRequest) (*anvilv1.RemovePortReply, error) {
	if err := s.Manager.RemovePort(ctx, req.GetName(), int(req.GetHostPort()), req.GetProtocol()); err != nil {
		return nil, err
	}
	return &anvilv1.RemovePortReply{}, nil
}

func (s *Server) Stats(ctx context.Context, req *anvilv1.StatsRequest) (*anvilv1.StatsReply, error) {
	stats, err := s.Manager.Stats(ctx, req.GetName())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.StatsReply{Stats: statsToPB(stats)}, nil
}

func (s *Server) Watch(req *anvilv1.WatchRequest, stream anvilv1.InstanceService_WatchServer) error {
	// Subscribe before listing, so no change between the two is lost.
	events, cancel := s.Manager.Subscribe()
	defer cancel()

	if req.GetIncludeExisting() {
		specs, err := s.Manager.List("")
		if err != nil {
			return err
		}
		for _, spec := range specs {
			if err := stream.Send(&anvilv1.WatchEvent{Type: anvilv1.WatchEventType_WATCH_EVENT_TYPE_UPDATED, Instance: specToPB(spec)}); err != nil {
				return err
			}
		}
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-events:
			if err := stream.Send(watchEventToPB(ev)); err != nil {
				return err
			}
		}
	}
}

func watchEventToPB(ev instance.Event) *anvilv1.WatchEvent {
	t := anvilv1.WatchEventType_WATCH_EVENT_TYPE_UPDATED
	if ev.Type == instance.EventDeleted {
		t = anvilv1.WatchEventType_WATCH_EVENT_TYPE_DELETED
	}
	return &anvilv1.WatchEvent{Type: t, Instance: specToPB(ev.Spec)}
}
