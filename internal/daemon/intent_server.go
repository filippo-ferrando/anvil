package daemon

import (
	"context"
	"time"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/intent"
	"github.com/anvil-project/anvil/internal/intent/apply"
	"github.com/anvil-project/anvil/internal/store"
)

// IntentServer implements anvilv1.IntentServiceServer against an
// *intent.Manager.
type IntentServer struct {
	anvilv1.UnimplementedIntentServiceServer
	Manager *intent.Manager
	Applier *apply.Applier
}

func NewIntentServer(mgr *intent.Manager, instances *instance.Manager) *IntentServer {
	return &IntentServer{Manager: mgr, Applier: &apply.Applier{Instances: instances, Intents: mgr}}
}

func intentMemberToPB(it store.Intent, m store.IntentMember) *anvilv1.IntentMember {
	pb := &anvilv1.IntentMember{
		InstanceId: m.InstanceID,
		Role:       m.Role,
		Kind:       kindToPB(m.Kind),
		Ip:         m.IP,
	}
	if it.Network != nil {
		pb.DnsName = intent.MemberDNSName(it.Name, m.Role)
	}
	return pb
}

func intentToPB(it store.Intent) *anvilv1.Intent {
	pb := &anvilv1.Intent{Id: it.ID, Name: it.Name}
	for _, m := range it.Members {
		pb.Members = append(pb.Members, intentMemberToPB(it, m))
	}
	if it.Network != nil {
		pb.Network = &anvilv1.IntentNetwork{
			EngineNetworkName: it.Network.EngineNetworkName,
			BridgeInterface:   it.Network.BridgeInterface,
			Subnet:            it.Network.Subnet,
			Gateway:           it.Network.Gateway,
			DockerIpRange:     it.Network.DockerIPRange,
			DnsDomain:         intent.Domain(it.Name),
			DnsServer:         it.Network.Gateway,
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
	it, err := s.Manager.Remove(ctx, req.GetName(), req.GetMember())
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

func (s *IntentServer) Apply(req *anvilv1.IntentApplyRequest, stream anvilv1.IntentService_ApplyServer) error {
	sendErr := func(err error) error {
		_ = stream.Send(&anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Error{Error: err.Error()}})
		return nil // reported in-band, like a failed launch
	}
	m, err := apply.Parse(req.GetManifest())
	if err != nil {
		return sendErr(err)
	}
	opts := apply.Options{
		DryRun:      req.GetDryRun(),
		Prune:       req.GetPrune(),
		Recreate:    req.GetRecreate(),
		SSHKeys:     req.GetSshPublicKeys(),
		WaitTimeout: time.Duration(req.GetWaitTimeoutSeconds()) * time.Second,
	}
	onPlan := func(steps []apply.Step) {
		plan := &anvilv1.IntentApplyPlan{IntentName: m.Intent}
		for _, st := range steps {
			plan.Steps = append(plan.Steps, &anvilv1.IntentApplyStep{
				Role: st.Role, InstanceName: st.Name, Action: string(st.Action), Changes: st.Changes,
			})
		}
		_ = stream.Send(&anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Plan{Plan: plan}})
	}
	progress := func(status string) {
		_ = stream.Send(&anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Status{Status: status}})
	}
	if err := s.Applier.Apply(stream.Context(), m, opts, onPlan, progress); err != nil {
		return sendErr(err)
	}
	if opts.DryRun {
		return nil
	}
	it, err := s.Manager.Info(m.Intent)
	if err != nil {
		return sendErr(err)
	}
	return stream.Send(&anvilv1.IntentApplyProgress{Event: &anvilv1.IntentApplyProgress_Intent{Intent: intentToPB(it)}})
}
