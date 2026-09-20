package instance

import "testing"

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
	}

	clone := cloneVMSpecForFork(source)

	// Mutating the clone's slices must not affect source's.
	clone.SSHPublicKeys[0] = "mutated"
	clone.Ports[0].HostPort = 9999
	clone.Mounts[0].Tag = "mutated"
	if source.SSHPublicKeys[0] != "ssh-ed25519 AAAA" {
		t.Errorf("mutating clone.SSHPublicKeys leaked into source: %v", source.SSHPublicKeys)
	}
	if source.Ports[0].HostPort != 2222 {
		t.Errorf("mutating clone.Ports leaked into source: %v", source.Ports)
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
}
