package daemon

import (
	"context"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/intent"
	"github.com/anvil-project/anvil/internal/store"
)

// IntentServer implements anvilv1.IntentServiceServer against an
// *intent.Manager.
type IntentServer struct {
	anvilv1.UnimplementedIntentServiceServer
	Manager *intent.Manager
}

func NewIntentServer(mgr *intent.Manager) *IntentServer {
	return &IntentServer{Manager: mgr}
}

func intentMemberToPB(m store.IntentMember) *anvilv1.IntentMember {
	return &anvilv1.IntentMember{
		InstanceId: m.InstanceID,
		Role:       m.Role,
		Kind:       kindToPB(m.Kind),
	}
}

func intentToPB(it store.Intent) *anvilv1.Intent {
	pb := &anvilv1.Intent{Id: it.ID, Name: it.Name}
	for _, m := range it.Members {
		pb.Members = append(pb.Members, intentMemberToPB(m))
	}
	if it.Network != nil {
		pb.Network = &anvilv1.IntentNetwork{
			EngineNetworkName: it.Network.EngineNetworkName,
			BridgeInterface:   it.Network.BridgeInterface,
			Subnet:            it.Network.Subnet,
			Gateway:           it.Network.Gateway,
			DockerIpRange:     it.Network.DockerIPRange,
		}
	}
	return pb
}

func (s *IntentServer) List(ctx context.Context, req *anvilv1.IntentListRequest) (*anvilv1.IntentListReply, error) {
	intents, err := s.Manager.List()
	if err != nil {
		return nil, err
	}
	reply := &anvilv1.IntentListReply{}
	for _, it := range intents {
		reply.Intents = append(reply.Intents, intentToPB(it))
	}
	return reply, nil
}

func (s *IntentServer) Info(ctx context.Context, req *anvilv1.IntentInfoRequest) (*anvilv1.IntentInfoReply, error) {
	it, err := s.Manager.Info(req.GetName())
	if err != nil {
		return nil, wrapErr(err)
	}
	return &anvilv1.IntentInfoReply{Intent: intentToPB(it)}, nil
}

func (s *IntentServer) Remove(ctx context.Context, req *anvilv1.IntentRemoveRequest) (*anvilv1.IntentRemoveReply, error) {
	it, err := s.Manager.Remove(req.GetName(), req.GetMember())
	if err != nil {
		return nil, err
	}
	return &anvilv1.IntentRemoveReply{Intent: intentToPB(it)}, nil
}

func (s *IntentServer) Delete(ctx context.Context, req *anvilv1.IntentDeleteRequest) (*anvilv1.IntentDeleteReply, error) {
	it, err := s.Manager.Delete(ctx, req.GetName(), req.GetPurgeMembers())
	if err != nil {
		return nil, err
	}
	return &anvilv1.IntentDeleteReply{Intent: intentToPB(it)}, nil
}
