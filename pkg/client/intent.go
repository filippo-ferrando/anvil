package client

import (
	"fmt"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// ParseMemberSpec parses one "<role>:<image>" intent member, split on the first colon.
func ParseMemberSpec(kind anvilv1.Kind, spec string) (*anvilv1.IntentMemberSpec, error) {
	role, image, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf(`%q must be "<role>:<image>"`, spec)
	}
	return &anvilv1.IntentMemberSpec{Role: role, Kind: kind, Image: image}, nil
}

// NewAddMembersRequest builds an IntentService.AddMembers request. When a member is a
// VM, anvil's own SSH key is authorized on the VMs, as a plain launch does.
func NewAddMembersRequest(name string, create bool, members []*anvilv1.IntentMemberSpec, hv anvilv1.VmHypervisor) (*anvilv1.IntentAddMembersRequest, error) {
	req := &anvilv1.IntentAddMembersRequest{Name: name, Create: create, Members: members, Hypervisor: hv}
	for _, m := range members {
		if m.GetKind() == anvilv1.Kind_KIND_VM {
			keys, err := ResolveSSHKeys(nil)
			if err != nil {
				return nil, err
			}
			req.SshPublicKeys = keys
			break
		}
	}
	return req, nil
}
