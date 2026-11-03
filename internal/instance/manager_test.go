package instance

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCloneVMSpecForForkDeepCopiesSlices(t *testing.T) {
	source := &VMSpec{
		SSHPublicKeys:   []string{"ssh-ed25519 AAAA"},
		Ports:           []PortMapping{{HostPort: 2222, GuestPort: 22}},
		Mounts:          []Mount{{HostPath: "/home/me", GuestPath: "/mnt/me", Tag: "mount0"}},
		DiskPath:        "/state/instances/src/disk.qcow2",
		SeedISOPath:     "/state/instances/src/seed.iso",
		SSHPort:         2222,
		NetworkMode:     "bridge",
		BridgeInterface: "anvil0",
		StaticIP:        "10.55.201.4/24",
		Gateway:         "10.55.201.1",
		ExtraHosts:      map[string]string{"db": "10.55.201.3"},
		SourceDiskPath:  "/staging/migrated.qcow2",
		MACAddress:      "52:54:00:aa:bb:cc",
	}

	clone := cloneVMSpecForFork(source, "02")

	// Mutating the clone's slices must not affect source's.
	clone.SSHPublicKeys[0] = "mutated"
	clone.Mounts[0].Tag = "mutated"
	if source.SSHPublicKeys[0] != "ssh-ed25519 AAAA" {
		t.Errorf("mutating clone.SSHPublicKeys leaked into source: %v", source.SSHPublicKeys)
	}
	// Host forwards stay with the source, so both can run at once.
	if clone.Ports != nil || len(source.Ports) != 1 {
		t.Errorf("expected fork Ports cleared and source kept, got %v / %v", clone.Ports, source.Ports)
	}
	if source.Mounts[0].Tag != "mount0" {
		t.Errorf("mutating clone.Mounts leaked into source: %v", source.Mounts)
	}

	// Instance-specific and intent/bridge fields must be reset for the fork.
	if clone.DiskPath != "" || clone.SeedISOPath != "" || clone.SSHPort != 0 {
		t.Errorf("expected disk/seed/SSH-port fields cleared, got %+v", clone)
	}
	if clone.NetworkMode != "slirp" {
		t.Errorf("expected fork to always start standalone (slirp), got %q", clone.NetworkMode)
	}
	if clone.BridgeInterface != "" || clone.StaticIP != "" || clone.Gateway != "" || clone.ExtraHosts != nil {
		t.Errorf("expected bridge/intent addressing cleared, got %+v", clone)
	}
	if clone.SourceDiskPath != "" {
		t.Errorf("expected SourceDiskPath cleared, got %q", clone.SourceDiskPath)
	}
	if clone.MACAddress != DefaultMAC("02") {
		t.Errorf("expected the fork to derive its own MAC, got %q", clone.MACAddress)
	}
}

func TestDeleteRemovesRecord(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "box", Kind: KindVM, State: StateRunning, VM: &VMSpec{}})
	m := NewManager(reg, map[Kind]Backend{KindVM: plainBackend{}})
	if err := m.Delete(context.Background(), []string{"box"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.GetByID("01"); err == nil {
		t.Error("expected the record to be removed")
	}
}

// TestStateActionsRejectEmptyNames guards against an empty list acting on every instance.
func TestStateActionsRejectEmptyNames(t *testing.T) {
	m := NewManager(&memRegistry{}, map[Kind]Backend{KindVM: &plainBackend{}})
	ctx := context.Background()
	if err := m.Start(ctx, nil); !errors.Is(err, ErrNoNames) {
		t.Errorf("Start: got %v, want ErrNoNames", err)
	}
	if err := m.Stop(ctx, nil, false, time.Second); !errors.Is(err, ErrNoNames) {
		t.Errorf("Stop: got %v, want ErrNoNames", err)
	}
	if err := m.Delete(ctx, nil); !errors.Is(err, ErrNoNames) {
		t.Errorf("Delete: got %v, want ErrNoNames", err)
	}
}

func TestForkDropsIntentLabels(t *testing.T) {
	reg := &memRegistry{}
	_ = reg.PutInstance(&Spec{ID: "01", Name: "src", Kind: KindVM, State: StateStopped, VM: &VMSpec{},
		Labels: map[string]string{IntentLabel: "i1", RoleLabel: "db", "team": "a"}})
	m := NewManager(reg, map[Kind]Backend{KindVM: &forkingBackend{}})
	if err := m.Fork(t.Context(), ForkParams{Source: "src", NewName: "copy"}, func(LaunchEvent) {}); err != nil {
		t.Fatal(err)
	}
	fork, _ := reg.GetByName("copy")
	if _, ok := fork.Labels[IntentLabel]; ok || fork.Labels[RoleLabel] != "" || fork.Labels["team"] != "a" {
		t.Errorf("expected intent/role labels dropped and others kept, got %v", fork.Labels)
	}
	if src, _ := reg.GetByName("src"); src.Labels[IntentLabel] != "i1" {
		t.Errorf("source labels changed: %v", src.Labels)
	}
}
