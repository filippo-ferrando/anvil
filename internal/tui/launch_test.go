package tui

import "testing"

func TestLaunchFieldsGuestToggles(t *testing.T) {
	vm := newSimpleForm("", launchFields("vm", nil))
	if !vm.Bool(guestAgentField) {
		t.Error("expected the guest agent toggle to default to on")
	}
	if vm.Bool(waitField) {
		t.Error("expected the wait toggle to default to off")
	}

	for _, f := range launchFields("container", nil) {
		if f.Label == guestAgentField || f.Label == waitField {
			t.Errorf("expected no %q field for a container", f.Label)
		}
	}
}
