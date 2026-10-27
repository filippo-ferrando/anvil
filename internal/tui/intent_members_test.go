package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestIntentMembersRequest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	form := newIntentMembersForm(true, "")
	if _, err := newIntentMembersRequest(form, true, ""); err == nil {
		t.Error("expected a missing name to fail")
	}
	setField(&form, intentNameField, "shop")
	if _, err := newIntentMembersRequest(form, true, ""); err == nil {
		t.Error("expected no members to fail")
	}
	setField(&form, vmMembersField, "db:debian-13, app:ubuntu-24.04")
	setField(&form, containerMembersField, "cache:redis:7")
	setField(&form, hypervisorField, "firecracker")
	req, err := newIntentMembersRequest(form, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.GetName() != "shop" || !req.GetCreate() || len(req.GetMembers()) != 3 ||
		req.GetMembers()[2].GetImage() != "redis:7" || req.GetHypervisor() != anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER ||
		len(req.GetSshPublicKeys()) == 0 {
		t.Errorf("got %v", req)
	}
	setField(&form, containerMembersField, "redis")
	if _, err := newIntentMembersRequest(form, true, ""); err == nil || !strings.Contains(err.Error(), containerMembersField) {
		t.Errorf("expected a bad container member to fail, got %v", err)
	}

	add := newIntentMembersForm(false, "shop")
	if hasField(add, intentNameField) {
		t.Error("expected no name field when adding to an existing intent")
	}
	setField(&add, containerMembersField, "web:nginx")
	if req, err := newIntentMembersRequest(add, false, "shop"); err != nil || req.GetName() != "shop" || req.GetCreate() {
		t.Errorf("add: got %v, %v", req, err)
	}
}

func TestIntentMembersKeysAndProgress(t *testing.T) {
	m := model{screen: screenIntents, intents: newIntentsModel()}
	m.intents.list.SetItems([]list.Item{intentItem{intent: &anvilv1.Intent{Name: "shop"}}})
	next, _ := m.Update(key("a"))
	m = next.(model)
	if !m.intents.membersPrompting || m.intents.membersCreate || m.intents.membersOf != "shop" {
		t.Fatal("expected a to open the add-members form for the selected intent")
	}
	m.intents.membersPrompting = false
	next, _ = m.Update(key("n"))
	if in := next.(model).intents; !in.membersPrompting || !in.membersCreate {
		t.Fatal("expected n to open the create form")
	}

	m.intents.members.start()
	for _, ev := range []*anvilv1.IntentAddMembersProgress{
		{Event: &anvilv1.IntentAddMembersProgress_Member{Member: &anvilv1.IntentMemberProgress{
			Role: "db", Launch: &anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Status{Status: "booting"}}}}},
		{Event: &anvilv1.IntentAddMembersProgress_Member{Member: &anvilv1.IntentMemberProgress{
			Role: "db", Launch: &anvilv1.LaunchProgress{Event: &anvilv1.LaunchProgress_Instance{Instance: &anvilv1.Instance{Name: "shop-db"}}}}}},
	} {
		next, _ = m.Update(intentMembersStreamMsg{parseIntentMembersProgress(ev)})
		m = next.(model)
	}
	next, cmd := m.Update(intentMembersStreamMsg{failed[struct{}](errors.New("image bad is broken"))})
	m = next.(model)
	view := m.intents.View()
	for _, want := range []string{"[db] booting", "[db] launched shop-db", "image bad is broken"} {
		if !strings.Contains(view, want) {
			t.Errorf("expected %q in:\n%s", want, view)
		}
	}
	if m.intents.members.running || cmd == nil {
		t.Error("expected a failure to end the transcript and reload the intents")
	}
}
