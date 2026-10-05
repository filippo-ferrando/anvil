package daemon

import (
	"context"
	"sort"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/discovery"
	"github.com/anvil-project/anvil/internal/migrate"
	"github.com/anvil-project/anvil/internal/store"
)

// HostServer implements anvilv1.HostServiceServer against *store.Store,
// using *migrate.Manager for connectivity tests.
type HostServer struct {
	anvilv1.UnimplementedHostServiceServer
	Store   *store.Store
	Migrate *migrate.Manager
}

func NewHostServer(s *store.Store, m *migrate.Manager) *HostServer {
	return &HostServer{Store: s, Migrate: m}
}

func hostToPB(h store.Host) *anvilv1.Host {
	return &anvilv1.Host{Alias: h.Alias, Target: h.Target, Identity: h.Identity, StrictHostKey: h.StrictHostKey}
}

func (s *HostServer) Add(ctx context.Context, req *anvilv1.HostAddRequest) (*anvilv1.HostAddReply, error) {
	h := req.GetHost()
	if err := s.Store.PutHost(store.Host{
		Alias: h.GetAlias(), Target: h.GetTarget(), Identity: h.GetIdentity(), StrictHostKey: h.GetStrictHostKey(),
	}); err != nil {
		return nil, err
	}
	return &anvilv1.HostAddReply{}, nil
}

func (s *HostServer) List(ctx context.Context, req *anvilv1.HostListRequest) (*anvilv1.HostListReply, error) {
	hosts, err := s.Store.ListHosts()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.HostListReply{}
	for _, h := range hosts {
		reply.Hosts = append(reply.Hosts, hostToPB(h))
	}
	return reply, nil
}

func (s *HostServer) Remove(ctx context.Context, req *anvilv1.HostRemoveRequest) (*anvilv1.HostRemoveReply, error) {
	if err := s.Store.DeleteHost(req.GetAlias()); err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.HostRemoveReply{}, nil
}

func (s *HostServer) Test(ctx context.Context, req *anvilv1.HostTestRequest) (*anvilv1.HostTestReply, error) {
	detail, err := s.Migrate.CheckHost(ctx, req.GetAlias())
	if err != nil {
		return &anvilv1.HostTestReply{Ok: false, Message: err.Error()}, nil
	}
	return &anvilv1.HostTestReply{Ok: true, Message: detail}, nil
}

const (
	// defaultDiscoverTimeout is how long Discover waits for mDNS answers when
	// the caller doesn't ask for a different window.
	defaultDiscoverTimeout = 3 * time.Second
	// maxDiscoverTimeout caps it, so one client can't hold the call open.
	maxDiscoverTimeout = 30 * time.Second
)

func (s *HostServer) Discover(ctx context.Context, req *anvilv1.HostDiscoverRequest) (*anvilv1.HostDiscoverReply, error) {
	timeout := defaultDiscoverTimeout
	if secs := req.GetTimeoutSeconds(); secs > 0 {
		timeout = min(time.Duration(secs)*time.Second, maxDiscoverTimeout)
	}
	hosts, err := discovery.Browse(ctx, timeout)
	if err != nil {
		return nil, err
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Name < hosts[j].Name })

	reply := &anvilv1.HostDiscoverReply{}
	for _, h := range hosts {
		reply.Hosts = append(reply.Hosts, &anvilv1.DiscoveredHost{
			Name: h.Name, Address: h.Addr.String(), SshPort: int32(h.Port),
		})
	}
	return reply, nil
}
