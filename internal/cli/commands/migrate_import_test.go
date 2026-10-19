package commands

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/migrate/payload"
)

func TestLaunchRequestFromPayloadHypervisor(t *testing.T) {
	req, err := launchRequestFromPayload(payload.Payload{Name: "fc", Kind: "vm", VM: &payload.VM{
		ImageRef: "debian-13", Hypervisor: "firecracker", StaticIP: "10.254.254.9/24",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Vm.GetHypervisor() != anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER || req.Vm.GetStaticIp() != "10.254.254.9/24" {
		t.Fatalf("vm = %+v", req.Vm)
	}

	// From a source older than hypervisors: qemu, the daemon's default.
	req, err = launchRequestFromPayload(payload.Payload{Name: "old", Kind: "vm", VM: &payload.VM{ImageRef: "debian-13"}})
	if err != nil || req.Vm.GetHypervisor() != anvilv1.VmHypervisor_VM_HYPERVISOR_UNSPECIFIED {
		t.Fatalf("old payload: %v, %v", req.GetVm().GetHypervisor(), err)
	}

	if _, err := launchRequestFromPayload(payload.Payload{Name: "x", Kind: "vm", VM: &payload.VM{Hypervisor: "xen"}}); err == nil {
		t.Fatal("expected an unknown hypervisor to be refused")
	}
}
