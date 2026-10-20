package intent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// fakeStore is a minimal in-memory Store for testing Manager without bbolt.
type fakeStore struct {
	mu   sync.Mutex
	byID map[string]store.Intent
}

func newFakeStore() *fakeStore { return &fakeStore{byID: map[string]store.Intent{}} }

func (s *fakeStore) PutIntent(it store.Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[it.ID] = it
	return nil
}
func (s *fakeStore) GetIntentByID(id string) (store.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.byID[id]
	if !ok {
		return store.Intent{}, instance.ErrNotFound
	}
	return it, nil
}
func (s *fakeStore) GetIntentByName(name string) (store.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.byID {
		if it.Name == name {
			return it, nil
		}
	}
	return store.Intent{}, instance.ErrNotFound
}
func (s *fakeStore) ListIntents() ([]store.Intent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Intent, 0, len(s.byID))
	for _, it := range s.byID {
		out = append(out, it)
	}
	return out, nil
}
func (s *fakeStore) DeleteIntentByID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
	return nil
}

// fakeInstances is a minimal Instances fake; Launch fails unless launch is set.
type fakeInstances struct {
	specs  map[string]*instance.Spec
	launch func(params instance.LaunchParams, progress func(instance.LaunchEvent)) error
}

func (f *fakeInstances) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	if f.launch != nil {
		return f.launch(params, progress)
	}
	return errors.New("not implemented in fakeInstances")
}
func (f *fakeInstances) GetByID(id string) (*instance.Spec, error) {
	spec, ok := f.specs[id]
	if !ok {
		return nil, instance.ErrNotFound
	}
	return spec, nil
}
func (f *fakeInstances) List(instance.Kind) ([]*instance.Spec, error) {
	var out []*instance.Spec
	for _, s := range f.specs {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeInstances) Delete(ctx context.Context, names []string) error {
	return nil
}

// fakeNetworker is an in-memory Networker recording every create/remove call.
type fakeNetworker struct {
	networks map[string]bool
	removed  []string
	failNext bool
}

func newFakeNetworker(existing ...string) *fakeNetworker {
	n := &fakeNetworker{networks: map[string]bool{}}
	for _, name := range existing {
		n.networks[name] = true
	}
	return n
}

func (n *fakeNetworker) CreateNetwork(ctx context.Context, name, bridgeInterface, subnet, gateway, dockerIPRange string) error {
	n.networks[name] = true
	return nil
}
func (n *fakeNetworker) RemoveNetwork(ctx context.Context, name string) error {
	if n.failNext {
		n.failNext = false
		return errors.New("simulated removal failure")
	}
	delete(n.networks, name)
	n.removed = append(n.removed, name)
	return nil
}
func (n *fakeNetworker) ContainerAddress(ctx context.Context, networkName, containerID string) (string, error) {
	return "", errors.New("not implemented in fakeNetworker")
}
func (n *fakeNetworker) ListNetworks(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(n.networks))
	for name := range n.networks {
		names = append(names, name)
	}
	return names, nil
}

func TestRemoveTearsDownNetworkWhenLastMemberLeaves(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker("anvil-intent1")
	it := store.Intent{
		ID:   "intent1",
		Name: "myapp",
		Members: []store.IntentMember{
			{InstanceID: "inst1", Role: "web"},
		},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1"},
	}
	if err := s.PutIntent(it); err != nil {
		t.Fatal(err)
	}

	m := NewManager(s, &fakeInstances{}, net)
	updated, err := m.Remove(t.Context(), "myapp", "web")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(updated.Members) != 0 {
		t.Errorf("expected no members left, got %+v", updated.Members)
	}
	if updated.Network != nil {
		t.Errorf("expected Network to be cleared after successful teardown, got %+v", updated.Network)
	}
	if len(net.removed) != 1 || net.removed[0] != "anvil-intent1" {
		t.Errorf("expected the network to actually be removed, got removed=%v", net.removed)
	}

	// And the store record itself reflects this, not just the returned copy.
	stored, err := s.GetIntentByName("myapp")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Network != nil {
		t.Errorf("expected the persisted record's Network to be cleared too, got %+v", stored.Network)
	}
}

func TestRemoveKeepsNetworkReferenceWhenTeardownFails(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker("anvil-intent1")
	net.failNext = true
	it := store.Intent{
		ID:      "intent1",
		Name:    "myapp",
		Members: []store.IntentMember{{InstanceID: "inst1", Role: "web"}},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1"},
	}
	_ = s.PutIntent(it)

	m := NewManager(s, &fakeInstances{}, net)
	updated, err := m.Remove(t.Context(), "myapp", "web")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if updated.Network == nil {
		t.Error("expected Network to stay set when RemoveNetwork fails, so a later reconcile can still find it")
	}
	if len(net.networks) != 1 {
		t.Errorf("expected the network to still exist in the engine after a failed removal, got %v", net.networks)
	}
}

func TestRemoveKeepsNetworkWhileMembersRemain(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker("anvil-intent1")
	it := store.Intent{
		ID:   "intent1",
		Name: "myapp",
		Members: []store.IntentMember{
			{InstanceID: "inst1", Role: "web"},
			{InstanceID: "inst2", Role: "db"},
		},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1"},
	}
	_ = s.PutIntent(it)

	m := NewManager(s, &fakeInstances{}, net)
	updated, err := m.Remove(t.Context(), "myapp", "web")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(updated.Members) != 1 {
		t.Errorf("expected one member left, got %+v", updated.Members)
	}
	if updated.Network == nil {
		t.Error("expected the network to stay up while a member remains")
	}
	if len(net.removed) != 0 {
		t.Errorf("expected no network removal while members remain, got %v", net.removed)
	}
}

func TestReconcileNetworksRemovesOrphans(t *testing.T) {
	s := newFakeStore()
	_ = s.PutIntent(store.Intent{
		ID:      "liveintent",
		Name:    "live",
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-liveintent"},
	})
	net := newFakeNetworker("anvil-liveintent", "anvil-orphaned1", "anvil-orphaned2", "bridge", "host")

	m := NewManager(s, &fakeInstances{}, net)
	if err := m.ReconcileNetworks(t.Context()); err != nil {
		t.Fatalf("ReconcileNetworks: %v", err)
	}

	if net.networks["anvil-orphaned1"] || net.networks["anvil-orphaned2"] {
		t.Errorf("expected both orphaned anvil-* networks to be removed, got %v", net.networks)
	}
	if !net.networks["anvil-liveintent"] {
		t.Error("expected the live intent's own network to survive reconciliation")
	}
	if !net.networks["bridge"] || !net.networks["host"] {
		t.Error("expected non-anvil networks to be left alone entirely")
	}
}

func TestReconcileNetworksNoopWithoutNetworker(t *testing.T) {
	m := NewManager(newFakeStore(), &fakeInstances{}, nil)
	if err := m.ReconcileNetworks(t.Context()); err != nil {
		t.Errorf("expected a nil Networker to be a no-op, got: %v", err)
	}
}

// TestLaunchRemovesNetworkWhenFirstMemberFails covers the window where a new
// intent's network exists but nothing in the store points at it yet.
func TestLaunchRemovesNetworkWhenFirstMemberFails(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker()
	m := NewManager(s, &fakeInstances{}, net)

	err := m.Launch(t.Context(), instance.LaunchParams{
		Name:       "web",
		IntentName: "myapp",
		Container:  &instance.ContainerSpec{ImageRef: "nginx"},
	}, func(instance.LaunchEvent) {})
	if err == nil {
		t.Fatal("expected the launch to fail")
	}

	if len(net.networks) != 0 {
		t.Errorf("a failed first launch left networks behind: %v", net.networks)
	}
	if len(net.removed) != 1 {
		t.Errorf("expected exactly one network removal, got %v", net.removed)
	}
	if its, _ := s.ListIntents(); len(its) != 0 {
		t.Errorf("expected no intent to be persisted, got %+v", its)
	}
}

// TestLaunchKeepsNetworkOfAnExistingIntent makes sure the cleanup above only
// ever touches an intent this call created.
func TestLaunchKeepsNetworkOfAnExistingIntent(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker("anvil-intent1")
	if err := s.PutIntent(store.Intent{
		ID: "intent1", Name: "myapp",
		Members: []store.IntentMember{{InstanceID: "inst1", Role: "db"}},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1", Subnet: "10.0.0.0/24", Gateway: "10.0.0.1"},
	}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(s, &fakeInstances{}, net)

	if err := m.Launch(t.Context(), instance.LaunchParams{
		Name: "web", IntentName: "myapp", Container: &instance.ContainerSpec{ImageRef: "nginx"},
	}, func(instance.LaunchEvent) {}); err == nil {
		t.Fatal("expected the launch to fail")
	}
	if !net.networks["anvil-intent1"] {
		t.Error("an existing intent's network must survive a failed launch")
	}
}

// TestLaunchDoesNotBlockOtherIntents checks a slow member launch only holds its own intent.
func TestLaunchDoesNotBlockOtherIntents(t *testing.T) {
	unblock := make(chan struct{})
	slowStarted := make(chan struct{})
	insts := &fakeInstances{launch: func(p instance.LaunchParams, progress func(instance.LaunchEvent)) error {
		if p.IntentName == "slow" {
			close(slowStarted)
			<-unblock
		}
		progress(instance.LaunchEvent{Instance: &instance.Spec{ID: p.Name, Name: p.Name, Kind: instance.KindContainer}})
		return nil
	}}
	m := NewManager(newFakeStore(), insts, nil)
	launch := func(name, intentName string) error {
		return m.Launch(t.Context(), instance.LaunchParams{
			Name: name, IntentName: intentName, Container: &instance.ContainerSpec{ImageRef: "nginx"},
		}, func(instance.LaunchEvent) {})
	}

	slowDone := make(chan error, 1)
	go func() { slowDone <- launch("a", "slow") }()
	<-slowStarted
	if err := launch("b", "fast"); err != nil {
		t.Fatalf("launch into another intent: %v", err)
	}
	close(unblock)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow launch: %v", err)
	}
	if its, _ := m.List(); len(its) != 2 {
		t.Errorf("expected 2 intents, got %+v", its)
	}
}

func TestRemoveKeepsNetworkWhileInstanceStillAttached(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker("anvil-intent1")
	_ = s.PutIntent(store.Intent{
		ID: "intent1", Name: "myapp",
		Members: []store.IntentMember{{InstanceID: "inst1", Role: "web"}},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1", BridgeInterface: "anvilintent1"},
	})
	// Ungrouped, not deleted: the VM still sits on the bridge.
	insts := &fakeInstances{specs: map[string]*instance.Spec{
		"inst1": {ID: "inst1", Name: "web", VM: &instance.VMSpec{BridgeInterface: "anvilintent1"}},
	}}
	m := NewManager(s, insts, net)
	updated, err := m.Remove(t.Context(), "myapp", "web")
	if err != nil {
		t.Fatal(err)
	}
	if !net.networks["anvil-intent1"] || updated.Network == nil {
		t.Error("expected the network to stay while an instance is attached to it")
	}
}

func TestLaunchRecreatesVanishedNetwork(t *testing.T) {
	s := newFakeStore()
	net := newFakeNetworker() // pruned behind anvil's back
	_ = s.PutIntent(store.Intent{
		ID: "intent1", Name: "myapp",
		Members: []store.IntentMember{{InstanceID: "inst1", Role: "db"}},
		Network: &store.IntentNetwork{EngineNetworkName: "anvil-intent1", Subnet: "10.0.0.0/24", Gateway: "10.0.0.1"},
	})
	m := NewManager(s, &fakeInstances{}, net)
	_ = m.Launch(t.Context(), instance.LaunchParams{
		Name: "web", IntentName: "myapp", Container: &instance.ContainerSpec{ImageRef: "nginx"},
	}, func(instance.LaunchEvent) {})
	if !net.networks["anvil-intent1"] {
		t.Error("expected the missing network to be created again")
	}
}

func TestLaunchRefusesDuplicateRole(t *testing.T) {
	s := newFakeStore()
	_ = s.PutIntent(store.Intent{ID: "intent1", Name: "myapp", Members: []store.IntentMember{{InstanceID: "inst1", Role: "db"}}})
	launched := false
	insts := &fakeInstances{launch: func(instance.LaunchParams, func(instance.LaunchEvent)) error {
		launched = true
		return nil
	}}
	m := NewManager(s, insts, nil)
	err := m.Launch(t.Context(), instance.LaunchParams{
		Name: "db2", Role: "db", IntentName: "myapp", Container: &instance.ContainerSpec{ImageRef: "postgres"},
	}, func(instance.LaunchEvent) {})
	if err == nil || launched {
		t.Errorf("expected a second %q member to be refused before launching, got %v", "db", err)
	}
}
