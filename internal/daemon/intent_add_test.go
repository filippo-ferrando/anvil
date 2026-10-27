package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/intent"
	"git.fferrando.cc/rdfilippo/anvil/internal/store"
)

// failingBackend is readyBackend, but creating an instance from image "bad" fails.
type failingBackend struct{ readyBackend }

func (failingBackend) Create(_ context.Context, spec *instance.Spec, _ func(string)) error {
	if spec.VM != nil && spec.VM.ImageRef == "bad" {
		return errors.New("image bad is broken")
	}
	return nil
}

func newIntentTestServer(t *testing.T) *IntentServer {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "anvil.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := failingBackend{}
	mgr := instance.NewManager(db, map[instance.Kind]instance.Backend{instance.KindVM: b, instance.KindContainer: b})
	intents := intent.NewManager(db, mgr, nil)
	return NewIntentServer(intents, mgr, NewServer(mgr, intents, nil))
}

func addMembers(t *testing.T, s *IntentServer, req *anvilv1.IntentAddMembersRequest) []*anvilv1.IntentAddMembersProgress {
	t.Helper()
	stream := &progressStream[*anvilv1.IntentAddMembersProgress]{ctx: context.Background()}
	if err := s.AddMembers(req, stream); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	if len(stream.sent) == 0 {
		t.Fatal("expected at least one event")
	}
	return stream.sent
}

func vmMember(role, image string) *anvilv1.IntentMemberSpec {
	return &anvilv1.IntentMemberSpec{Role: role, Kind: anvilv1.Kind_KIND_VM, Image: image}
}

func TestAddMembersCreatesIntent(t *testing.T) {
	s := newIntentTestServer(t)
	events := addMembers(t, s, &anvilv1.IntentAddMembersRequest{
		Name: "shop", Create: true, SshPublicKeys: []string{"ssh-ed25519 AAAA test"},
		Members: []*anvilv1.IntentMemberSpec{
			vmMember("db", "debian-13"),
			{Role: "web", Kind: anvilv1.Kind_KIND_CONTAINER, Image: "nginx"},
		},
	})
	last := events[len(events)-1]
	if got := last.GetIntent(); got == nil || len(got.GetMembers()) != 2 {
		t.Fatalf("expected the final event to be the intent with 2 members, got %v", last)
	}
	launched := map[string]string{}
	for _, ev := range events {
		if inst := ev.GetMember().GetLaunch().GetInstance(); inst != nil {
			launched[ev.GetMember().GetRole()] = inst.GetName()
		}
	}
	if launched["db"] != "shop-db" || launched["web"] != "shop-web" {
		t.Errorf("expected each member's instance event, got %v", launched)
	}
	specs, err := s.Launcher.Manager.Info([]string{"shop-db"})
	if err != nil || len(specs[0].VM.SSHPublicKeys) != 1 {
		t.Errorf("expected the VM member to get the SSH key: %v %+v", err, specs)
	}

	// create refuses an existing intent; add grows it.
	events = addMembers(t, s, &anvilv1.IntentAddMembersRequest{Name: "shop", Create: true, Members: []*anvilv1.IntentMemberSpec{vmMember("cache", "x")}})
	if e := events[len(events)-1].GetError(); !strings.Contains(e, "already exists") {
		t.Errorf("expected create of an existing intent to fail, got %q", e)
	}
	events = addMembers(t, s, &anvilv1.IntentAddMembersRequest{Name: "shop", Members: []*anvilv1.IntentMemberSpec{vmMember("cache", "x")}})
	if got := events[len(events)-1].GetIntent(); len(got.GetMembers()) != 3 {
		t.Errorf("expected add to grow the intent to 3 members, got %v", events[len(events)-1])
	}
}

func TestAddMembersRefusesMissingIntentAndBadMembers(t *testing.T) {
	s := newIntentTestServer(t)
	cases := map[string]*anvilv1.IntentAddMembersRequest{
		"no such intent":  {Name: "nope", Members: []*anvilv1.IntentMemberSpec{vmMember("db", "x")}},
		"at least one":    {Name: "nope", Create: true},
		"has no image":    {Name: "nope", Create: true, Members: []*anvilv1.IntentMemberSpec{vmMember("db", "")}},
		"given twice":     {Name: "nope", Create: true, Members: []*anvilv1.IntentMemberSpec{vmMember("db", "x"), vmMember("db", "y")}},
		"vm or container": {Name: "nope", Create: true, Members: []*anvilv1.IntentMemberSpec{{Role: "db", Image: "x"}}},
	}
	for want, req := range cases {
		events := addMembers(t, s, req)
		if e := events[len(events)-1].GetError(); !strings.Contains(e, want) {
			t.Errorf("expected %q, got %q", want, e)
		}
	}
	if _, err := s.Manager.Info("nope"); err == nil {
		t.Error("expected no intent to be created by a refused request")
	}
}

func TestAddMembersStopsAtFirstFailureAndKeepsEarlierMembers(t *testing.T) {
	s := newIntentTestServer(t)
	events := addMembers(t, s, &anvilv1.IntentAddMembersRequest{Name: "shop", Create: true, Members: []*anvilv1.IntentMemberSpec{
		vmMember("db", "x"), vmMember("web", "bad"), vmMember("cache", "x"),
	}})
	errs := 0
	for _, ev := range events {
		if ev.GetMember().GetLaunch().GetError() != "" {
			t.Error("expected member events to carry no error")
		}
		if ev.GetError() != "" {
			errs++
		}
		if ev.GetMember().GetRole() == "cache" {
			t.Error("expected no launch after the failed member")
		}
	}
	if e := events[len(events)-1].GetError(); errs != 1 || !strings.Contains(e, "broken") {
		t.Fatalf("expected one final error about the failed member, got %d, %q", errs, e)
	}
	it, err := s.Manager.Info("shop")
	if err != nil || len(it.Members) != 1 || it.Members[0].Role != "db" {
		t.Errorf("expected the earlier member to be kept, got %+v, %v", it, err)
	}
}
