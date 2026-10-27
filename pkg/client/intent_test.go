package client

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestParseMemberSpec(t *testing.T) {
	m, err := ParseMemberSpec(anvilv1.Kind_KIND_CONTAINER, "cache:redis:7")
	if err != nil || m.GetRole() != "cache" || m.GetImage() != "redis:7" || m.GetKind() != anvilv1.Kind_KIND_CONTAINER {
		t.Errorf("got %v, %v", m, err)
	}
	if _, err := ParseMemberSpec(anvilv1.Kind_KIND_VM, "nocolon"); err == nil {
		t.Error("expected a spec without a colon to fail")
	}
}

func TestNewAddMembersRequestKeysOnlyForVMs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ct := &anvilv1.IntentMemberSpec{Role: "web", Kind: anvilv1.Kind_KIND_CONTAINER, Image: "nginx"}
	req, err := NewAddMembersRequest("shop", true, []*anvilv1.IntentMemberSpec{ct}, 0)
	if err != nil || len(req.GetSshPublicKeys()) != 0 || !req.GetCreate() || req.GetName() != "shop" {
		t.Errorf("containers only: got %v, %v", req, err)
	}
	vm := &anvilv1.IntentMemberSpec{Role: "db", Kind: anvilv1.Kind_KIND_VM, Image: "debian-13"}
	req, err = NewAddMembersRequest("shop", false, []*anvilv1.IntentMemberSpec{vm, ct}, 0)
	if err != nil || len(req.GetSshPublicKeys()) != 1 {
		t.Errorf("with a VM: got %v, %v", req, err)
	}
}
