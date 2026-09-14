package intent

import (
	"context"
	"errors"
	"testing"

	"github.com/anvil-project/anvil/internal/instance"
	"github.com/anvil-project/anvil/internal/store"
)

// fakeStore is a minimal in-memory Store for testing Manager without bbolt.
type fakeStore struct {
	byID map[string]store.Intent
}

func newFakeStore() *fakeStore { return &fakeStore{byID: map[string]store.Intent{}} }

func (s *fakeStore) PutIntent(it store.Intent) error {
	s.byID[it.ID] = it
	return nil
}
func (s *fakeStore) GetIntentByID(id string) (store.Intent, error) {
	it, ok := s.byID[id]
	if !ok {
		return store.Intent{}, instance.ErrNotFound
	}
	return it, nil
}
func (s *fakeStore) GetIntentByName(name string) (store.Intent, error) {
	for _, it := range s.byID {
		if it.Name == name {
			return it, nil
		}
	}
	return store.Intent{}, instance.ErrNotFound
}
func (s *fakeStore) ListIntents() ([]store.Intent, error) {
	out := make([]store.Intent, 0, len(s.byID))
	for _, it := range s.byID {
		out = append(out, it)
	}
	return out, nil
}
func (s *fakeStore) DeleteIntentByID(id string) error {
	delete(s.byID, id)
	return nil
}

// fakeInstances is a minimal Instances fake; only GetByID/Delete are
// exercised by the tests in this file.
type fakeInstances struct {
	specs map[string]*instance.Spec
}

func (f *fakeInstances) Launch(ctx context.Context, params instance.LaunchParams, progress func(instance.LaunchEvent)) error {
	return errors.New("not implemented in fakeInstances")
}
func (f *fakeInstances) GetByID(id string) (*instance.Spec, error) {
	spec, ok := f.specs[id]
	if !ok {
		return nil, instance.ErrNotFound
	}
	return spec, nil
}
func (f *fakeInstances) Delete(ctx context.Context, names []string, purge bool) error {
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
