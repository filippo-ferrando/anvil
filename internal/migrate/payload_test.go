package migrate

import (
	"context"
	"os"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/migrate/payload"
)

type fileExporter struct{}

func (fileExporter) ExportDisk(_ context.Context, _ *instance.Spec, dest string) error {
	return os.WriteFile(dest, []byte("flattened disk"), 0o640)
}

// A standalone Firecracker guest's address is configured inside its disk, so it travels
// with the payload; an intent member's goes in Payload.StaticIP instead.
func TestBuildVMPayloadFirecracker(t *testing.T) {
	probe, err := os.CreateTemp(config.MigrateStagingDir(), "probe-*")
	if err != nil {
		t.Skipf("migration stages under %s, not writable here: %v", config.MigrateStagingDir(), err)
	}
	probe.Close()
	_ = os.Remove(probe.Name())
	remote := t.TempDir()
	localSSH(t, func(cmd string) string { return strings.ReplaceAll(cmd, remoteStagingDir, remote) })
	m := &Manager{Exporter: fileExporter{}}

	spec := vmSpec()
	spec.ID = "01JFC"
	spec.VM.Hypervisor = instance.VMHypervisorFirecracker
	spec.VM.StaticIP = "10.254.254.9/24"
	var pl payload.Payload
	if err := m.buildVMPayload(context.Background(), spec, target{}, remoteCaps{sha256sum: true}, &pl, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if pl.VM.Hypervisor != "firecracker" || pl.VM.StaticIP != "10.254.254.9/24" {
		t.Errorf("standalone: hypervisor %q, address %q", pl.VM.Hypervisor, pl.VM.StaticIP)
	}

	spec.VM.NetworkMode = "bridge"
	pl = payload.Payload{}
	if err := m.buildVMPayload(context.Background(), spec, target{}, remoteCaps{sha256sum: true}, &pl, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if pl.VM.Hypervisor != "firecracker" || pl.VM.StaticIP != "" {
		t.Errorf("intent member: hypervisor %q, address %q", pl.VM.Hypervisor, pl.VM.StaticIP)
	}
}
