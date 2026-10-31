package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent/apply"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// IntentServer implements anvilv1.IntentServiceServer against an
// *intent.Manager.
type IntentServer struct {
	anvilv1.UnimplementedIntentServiceServer
	Manager  *intent.Manager
	Applier  *apply.Applier
	Launcher *Server // launches AddMembers' members the way InstanceService.Launch does
}

func NewIntentServer(mgr *intent.Manager, instances *instance.Manager, launcher *Server) *IntentServer {
	return &IntentServer{Manager: mgr, Applier: &apply.Applier{Instances: instances, Intents: mgr}, Launcher: launcher}
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
		return nil, err
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
	m, err := apply.Parse(req.GetManifest(), req.GetVars())
	if err != nil {
		return sendErr(err)
	}
	if err := checkCallerOwns(stream.Context(), m.VolumePaths()...); err != nil {
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

// memberSender wraps one member's launch events as AddMembers progress. An error
// is kept back in err, AddMembers reports it once as the final event.
type memberSender struct {
	role   string
	stream anvilv1.IntentService_AddMembersServer
	mu     *sync.Mutex // shared by the members launching at once: one Send at a time
	err    string
}

func (m *memberSender) Send(ev *anvilv1.LaunchProgress) error {
	if e := ev.GetError(); e != "" {
		m.err = e
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stream.Send(&anvilv1.IntentAddMembersProgress{Event: &anvilv1.IntentAddMembersProgress_Member{
		Member: &anvilv1.IntentMemberProgress{Role: m.role, Launch: ev},
	}})
}

// parallelMembers bounds how many members one AddMembers launches at once.
const parallelMembers = 4

// checkMembers rejects an empty or malformed member list before anything is launched.
func checkMembers(members []*anvilv1.IntentMemberSpec) error {
	if len(members) == 0 {
		return fmt.Errorf("intent: at least one member is needed")
	}
	roles := make(map[string]bool, len(members))
	for _, m := range members {
		switch {
		case m.GetRole() == "":
			return fmt.Errorf("intent: a member has no role")
		case m.GetImage() == "":
			return fmt.Errorf("intent: member %q has no image", m.GetRole())
		case m.GetKind() != anvilv1.Kind_KIND_VM && m.GetKind() != anvilv1.Kind_KIND_CONTAINER:
			return fmt.Errorf("intent: member %q needs kind vm or container", m.GetRole())
		case roles[m.GetRole()]:
			return fmt.Errorf("intent: role %q is given twice", m.GetRole())
		}
		roles[m.GetRole()] = true
	}
	return nil
}

// memberLaunchRequest is the Launch request for one member; the daemon names it "<intent>-<role>".
func memberLaunchRequest(req *anvilv1.IntentAddMembersRequest, m *anvilv1.IntentMemberSpec) *anvilv1.LaunchRequest {
	launch := &anvilv1.LaunchRequest{Kind: m.GetKind(), IntentName: req.GetName(), Role: m.GetRole()}
	if m.GetKind() == anvilv1.Kind_KIND_VM {
		launch.Vm = &anvilv1.VMSpec{ImageRef: m.GetImage(), SshPublicKeys: req.GetSshPublicKeys(), Hypervisor: req.GetHypervisor()}
	} else {
		launch.Container = &anvilv1.ContainerSpec{ImageRef: m.GetImage()}
	}
	return launch
}

func (s *IntentServer) AddMembers(req *anvilv1.IntentAddMembersRequest, stream anvilv1.IntentService_AddMembersServer) error {
	sendErr := func(err error) error {
		_ = stream.Send(&anvilv1.IntentAddMembersProgress{Event: &anvilv1.IntentAddMembersProgress_Error{Error: err.Error()}})
		return nil // reported in-band, like a failed launch
	}
	name := req.GetName()
	if name == "" {
		return sendErr(fmt.Errorf("intent: a name is needed"))
	}
	if err := checkMembers(req.GetMembers()); err != nil {
		return sendErr(err)
	}
	_, err := s.Manager.Info(name)
	switch {
	case err == nil && req.GetCreate():
		return sendErr(fmt.Errorf("intent %q already exists, use `anvil intent add` to grow it", name))
	case errors.Is(err, instance.ErrNotFound) && !req.GetCreate():
		return sendErr(fmt.Errorf("no such intent %q, use `anvil intent create` first", name))
	case err != nil && !errors.Is(err, instance.ErrNotFound):
		return sendErr(fmt.Errorf("intent: looking up %q: %w", name, err))
	}

	// Members launch side by side; the first one's error in member order is reported.
	members := req.GetMembers()
	errs := make([]error, len(members))
	var sendMu sync.Mutex
	sem := make(chan struct{}, parallelMembers)
	var wg sync.WaitGroup
	for i, m := range members {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			send := &memberSender{role: m.GetRole(), stream: stream, mu: &sendMu}
			err := s.Launcher.launch(stream.Context(), memberLaunchRequest(req, m), send)
			if send.err != "" {
				err = errors.New(send.err)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return sendErr(err)
		}
	}
	it, err := s.Manager.Info(name)
	if err != nil {
		return sendErr(err)
	}
	return stream.Send(&anvilv1.IntentAddMembersProgress{Event: &anvilv1.IntentAddMembersProgress_Intent{Intent: intentToPB(it)}})
}
