package ipam

import (
	"strings"
	"testing"
)

func TestAllocateSubnetIsDeterministic(t *testing.T) {
	subnet1, gateway1, ipRange1 := AllocateSubnet("myapp", 0)
	subnet2, gateway2, ipRange2 := AllocateSubnet("myapp", 0)
	if subnet1 != subnet2 || gateway1 != gateway2 || ipRange1 != ipRange2 {
		t.Errorf("expected the same seed/attempt to always produce the same allocation, got (%s,%s,%s) vs (%s,%s,%s)",
			subnet1, gateway1, ipRange1, subnet2, gateway2, ipRange2)
	}
}

func TestAllocateSubnetDiffersByAttempt(t *testing.T) {
	subnet0, _, _ := AllocateSubnet("myapp", 0)
	subnet1, _, _ := AllocateSubnet("myapp", 1)
	if subnet0 == subnet1 {
		t.Errorf("expected different attempts to (almost always) produce different subnets, both were %s", subnet0)
	}
}

func TestAllocateSubnetShape(t *testing.T) {
	subnet, gateway, ipRange := AllocateSubnet("myapp", 0)
	if !strings.HasPrefix(subnet, "10.") || !strings.HasSuffix(subnet, ".0/24") {
		t.Errorf("expected a 10.x.y.0/24 subnet, got %s", subnet)
	}
	if !strings.HasPrefix(gateway, "10.") || !strings.HasSuffix(gateway, ".1") {
		t.Errorf("expected a 10.x.y.1 gateway, got %s", gateway)
	}
	if !strings.HasSuffix(ipRange, ".128/25") {
		t.Errorf("expected a .128/25 docker IP range, got %s", ipRange)
	}
	// The gateway and IP range must actually fall inside the subnet's own
	// x.y octets, not just happen to look right independently.
	base := strings.TrimSuffix(subnet, "0/24")
	if !strings.HasPrefix(gateway, base) {
		t.Errorf("gateway %s doesn't share subnet %s's network portion", gateway, subnet)
	}
	if !strings.HasPrefix(ipRange, base) {
		t.Errorf("ip range %s doesn't share subnet %s's network portion", ipRange, subnet)
	}
}

func TestVMAddress(t *testing.T) {
	subnet, _, _ := AllocateSubnet("myapp", 0)
	base := strings.TrimSuffix(subnet, "0/24")

	first, err := VMAddress(subnet, 0)
	if err != nil {
		t.Fatalf("VMAddress(0): %v", err)
	}
	if first != base+"2/24" {
		t.Errorf("VMAddress(0) = %s, want %s", first, base+"2/24")
	}

	second, err := VMAddress(subnet, 1)
	if err != nil {
		t.Fatalf("VMAddress(1): %v", err)
	}
	if second != base+"3/24" {
		t.Errorf("VMAddress(1) = %s, want %s", second, base+"3/24")
	}
}

func TestVMAddressRejectsOutOfRangeIndex(t *testing.T) {
	subnet, _, _ := AllocateSubnet("myapp", 0)
	if _, err := VMAddress(subnet, 126); err == nil {
		t.Error("expected an error for an index that would overflow into Docker's own IPAM range")
	}
	if _, err := VMAddress(subnet, -1); err == nil {
		t.Error("expected an error for a negative index")
	}
}

func TestVMAddressRejectsInvalidSubnet(t *testing.T) {
	if _, err := VMAddress("not-a-subnet", 0); err == nil {
		t.Error("expected an error for an invalid subnet string")
	}
}
