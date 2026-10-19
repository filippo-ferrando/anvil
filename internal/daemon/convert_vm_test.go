package daemon

import (
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

func TestVMSpecFromPBHypervisor(t *testing.T) {
	fc := vmSpecFromPB(&anvilv1.VMSpec{Hypervisor: anvilv1.VmHypervisor_VM_HYPERVISOR_FIRECRACKER, StaticIp: "10.254.254.9/24"})
	if fc.Hypervisor != instance.VMHypervisorFirecracker || fc.StaticIP != "10.254.254.9/24" {
		t.Fatalf("firecracker spec = hypervisor %q ip %q", fc.Hypervisor, fc.StaticIP)
	}
	// Only a migrated Firecracker VM may bring its own address.
	q := vmSpecFromPB(&anvilv1.VMSpec{StaticIp: "10.0.0.5/24"})
	if q.Hypervisor != "" || q.StaticIP != "" {
		t.Fatalf("qemu spec = hypervisor %q ip %q", q.Hypervisor, q.StaticIP)
	}
	if pb := vmSpecToPB(&instance.VMSpec{}); pb.GetHypervisor() != anvilv1.VmHypervisor_VM_HYPERVISOR_QEMU {
		t.Fatalf("an unset hypervisor reads back as %v, want qemu", pb.GetHypervisor())
	}
	// An enum value from a newer client stays unknown, so Launch refuses it.
	if h := vmSpecFromPB(&anvilv1.VMSpec{Hypervisor: anvilv1.VmHypervisor(7)}).Hypervisor; instance.CheckHypervisor(h) == nil {
		t.Fatalf("unknown enum became %q, which CheckHypervisor accepts", h)
	}
}
