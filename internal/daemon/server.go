package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

// Server implements anvilv1.InstanceServiceServer against an
// *instance.Manager.
type Server struct {
	anvilv1.UnimplementedInstanceServiceServer
	Manager *instance.Manager
	Intents *intent.Manager
	Vault   *image.Vault // base image cache, for Delete's purge
}

func NewServer(mgr *instance.Manager, intents *intent.Manager, vault *image.Vault) *Server {
	return &Server{Manager: mgr, Intents: intents, Vault: vault}
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
	return s.launch(stream.Context(), req, stream)
}

// launch runs one Launch request, also used for each member of IntentService.AddMembers.
func (s *Server) launch(ctx context.Context, req *anvilv1.LaunchRequest, stream launchProgressSender) error {
	params, err := launchParamsFromPB(req)
	if err != nil {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
		return err
	}
	if params.Name == "" {
		if err := s.defaultLaunchName(&params); err != nil {
			sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
			return err
		}
	}
	// Checked before anything is named after it on disk.
	if err := instance.ValidateName(params.Name); err != nil {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
		return err
	}
	if err := checkCallerOwns(ctx, sharedPaths(params)...); err != nil {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
		return err
	}
	undoAdopt, err := adoptStagedData(ctx, &params)
	if err != nil {
		sendLaunchEvent(stream, instance.LaunchEvent{Err: err})
		return err
	}
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
	if params.IntentName != "" {
		err = s.Intents.Launch(ctx, params, send)
	} else {
		err = s.Manager.Launch(ctx, params, send)
	}
	if err != nil {
		undoAdopt()
	}
	if err != nil || !wait || launched == nil {
		return err
	}
	return s.waitReady(ctx, launched.Name, req.GetWaitTimeoutSeconds(), stream)
}

// defaultLaunchName names an unnamed launch: "<intent>-<role>" for an intent member,
// otherwise a free name derived from the image ref.
func (s *Server) defaultLaunchName(params *instance.LaunchParams) error {
	if params.IntentName != "" && params.Role != "" {
		params.Name = intent.MemberName(params.IntentName, params.Role)
		return nil
	}
	var ref string
	switch {
	case params.VM != nil:
		ref = params.VM.ImageRef
	case params.Container != nil:
		ref = params.Container.ImageRef
	}
	name, err := s.Manager.DefaultName(ref)
	params.Name = name
	return err
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
	if len(req.GetNames()) == 0 {
		return nil, instance.ErrNoNames
	}
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
		// Read before the disk is gone: the overlay names the base image it sits on.
		var base string
		if req.GetPurge() {
			base = baseImageOf(spec, s.Vault)
		}
		if err := s.Manager.Delete(ctx, []string{spec.Name}); err != nil {
			errs = append(errs, err)
			continue
		}
		if base != "" {
			if err := s.pruneBaseImage(base); err != nil {
				errs = append(errs, err)
			}
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

// pruneBaseImage removes path from the base image cache when no VM's disk sits on it.
// A base outside the cache (one that came with a migrated disk) left with the instance dir.
func (s *Server) pruneBaseImage(path string) error {
	cached, err := s.Vault.List()
	if err != nil {
		return err
	}
	list := func() ([]*instance.Spec, error) { return s.Manager.List(instance.KindVM) }
	for _, img := range cached {
		if !samePath(img.Path, path) {
			continue
		}
		bases, err := preloadBases(list, s.Vault)
		if err != nil {
			return err
		}
		err = s.Vault.DeleteUnused(img.Path, func() error {
			users, err := usersOfLocked(img.Path, list, bases, s.Vault)
			if err != nil {
				return err
			}
			if len(users) > 0 {
				return fmt.Errorf("%w: still used by %s", image.ErrInUse, strings.Join(users, ", "))
			}
			return nil
		})
		if errors.Is(err, image.ErrInUse) {
			log.Printf("daemon: keeping base image %s: %v", img.ID, err)
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) Logs(req *anvilv1.LogsRequest, stream anvilv1.InstanceService_LogsServer) error {
	return s.Manager.Logs(stream.Context(), req.GetName(), req.GetFollow(), int(req.GetTailLines()), func(chunk []byte) error {
		return stream.Send(&anvilv1.LogChunk{Data: chunk})
	})
}

func (s *Server) Mount(ctx context.Context, req *anvilv1.MountRequest) (*anvilv1.MountReply, error) {
	hostPath := req.GetHostPath()
	if err := checkCallerOwns(ctx, &hostPath); err != nil {
		return nil, err
	}
	if err := s.Manager.Mount(ctx, req.GetName(), hostPath, req.GetGuestPath(), req.GetReadOnly()); err != nil {
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
		return nil, err
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

func (s *Server) Update(ctx context.Context, req *anvilv1.UpdateRequest) (*anvilv1.UpdateReply, error) {
	var p instance.UpdateParams
	if req.Cpus != nil {
		n := int(req.GetCpus())
		p.CPUs = &n
	}
	if req.MemoryMib != nil {
		n := req.GetMemoryMib()
		p.MemoryMiB = &n
	}
	if req.DiskGib != nil {
		n := req.GetDiskGib()
		p.DiskGiB = &n
	}
	if req.Autostart != nil {
		b := req.GetAutostart()
		p.Autostart = &b
	}
	policy, err := restartPolicyFromPB(req.GetRestartPolicy())
	if err != nil {
		return nil, err
	}
	p.RestartPolicy = policy

	res, err := s.Manager.Update(ctx, req.GetName(), p)
	if err != nil {
		return nil, err
	}
	return &anvilv1.UpdateReply{Instance: specToPB(res.Spec), RestartPending: res.RestartPending, Notes: res.Notes}, nil
}

func (s *Server) Version(ctx context.Context, req *anvilv1.VersionRequest) (*anvilv1.VersionReply, error) {
	v, commit := version.Get()
	return &anvilv1.VersionReply{Version: v, Commit: commit}, nil
}
