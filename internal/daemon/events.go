package daemon

import (
	"context"
	"fmt"
	"os/user"
	"sync"
	"time"

	"google.golang.org/grpc"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// ActorInterceptors tag every request's context with the caller's user name, which the
// event log records as the actor of what the request does.
func ActorInterceptors() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(interceptUnary),
		grpc.ChainStreamInterceptor(interceptStream),
	}
}

// interceptUnary and interceptStream tag each call with its caller and run wrapErr on the
// result, so every service gives the same NotFound code, not only those that call it.
func interceptUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	reply, err := h(withCallerActor(ctx), req)
	return reply, wrapErr(err)
}

func interceptStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
	return wrapErr(h(srv, actorStream{ServerStream: ss, ctx: withCallerActor(ss.Context())}))
}

type actorStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s actorStream) Context() context.Context { return s.ctx }

func withCallerActor(ctx context.Context) context.Context {
	uid, ok := callerUID(ctx)
	if !ok {
		return ctx // events fall back to instance.DaemonActor
	}
	return instance.WithActor(ctx, actorNames.name(uid))
}

// actorNameTTL bounds how long a looked-up user name is reused, so a renamed user shows up.
const actorNameTTL = 5 * time.Minute

// actorNames caches uid to user name, since every RPC needs one and a lookup may read
// /etc/passwd or ask NSS.
var actorNames = &uidNames{lookup: func(uid string) (string, error) {
	u, err := user.LookupId(uid)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}}

type uidNames struct {
	lookup func(uid string) (string, error)
	mu     sync.Mutex
	names  map[uint32]cachedName
}

type cachedName struct {
	name string
	at   time.Time
}

func (c *uidNames) name(uid uint32) string {
	c.mu.Lock()
	n, ok := c.names[uid]
	c.mu.Unlock()
	if ok && time.Since(n.at) < actorNameTTL {
		return n.name
	}
	name := fmt.Sprintf("uid %d", uid)
	if u, err := c.lookup(fmt.Sprint(uid)); err == nil {
		name = u
	}
	c.mu.Lock()
	if c.names == nil {
		c.names = make(map[uint32]cachedName)
	}
	c.names[uid] = cachedName{name, time.Now()}
	c.mu.Unlock()
	return name
}

// Default and minimum StreamStats intervals; one sample already takes ~200ms.
const (
	defaultStatsInterval = 2 * time.Second
	minStatsInterval     = 500 * time.Millisecond
)

func (s *Server) StreamStats(req *anvilv1.StreamStatsRequest, stream anvilv1.InstanceService_StreamStatsServer) error {
	interval := time.Duration(req.GetIntervalMs()) * time.Millisecond
	if interval == 0 {
		interval = defaultStatsInterval
	}
	interval = max(interval, minStatsInterval)
	err := s.Manager.StreamStats(stream.Context(), req.GetName(), interval, func(st instance.Stats) error {
		return stream.Send(&anvilv1.StatsReply{Stats: statsToPB(st)})
	})
	return wrapErr(err)
}

func (s *Server) Events(req *anvilv1.EventsRequest, stream anvilv1.InstanceService_EventsServer) error {
	// Subscribe before reading the log, so no event between the two is lost.
	var live <-chan instance.HistoryEvent
	if req.GetFollow() {
		ch, cancel := s.Manager.SubscribeEvents()
		defer cancel()
		live = ch
	}

	stored, err := s.Manager.ListEvents(req.GetName(), int(req.GetLimit()))
	if err != nil {
		return err
	}
	var last uint64
	for _, ev := range stored {
		if err := stream.Send(historyEventToPB(ev)); err != nil {
			return err
		}
		last = ev.Seq
	}
	if live == nil {
		return nil
	}

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-live:
			if ev.Seq != 0 && ev.Seq <= last {
				continue // already sent from the stored log
			}
			if req.GetName() != "" && ev.InstanceName != req.GetName() {
				continue
			}
			if err := stream.Send(historyEventToPB(ev)); err != nil {
				return err
			}
		}
	}
}

func historyEventToPB(ev instance.HistoryEvent) *anvilv1.InstanceEvent {
	return &anvilv1.InstanceEvent{
		Seq:          ev.Seq,
		TimeUnix:     ev.Time.Unix(),
		InstanceId:   ev.InstanceID,
		InstanceName: ev.InstanceName,
		Kind:         kindToPB(ev.Kind),
		Action:       ev.Action,
		Actor:        ev.Actor,
		Message:      ev.Message,
		Error:        ev.Err,
	}
}
