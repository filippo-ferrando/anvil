//go:build linux

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

func TestAnvilGuestUserData(t *testing.T) {
	v := &instance.VMSpec{Hypervisor: instance.VMHypervisorFirecracker, AnvilGuest: true, NoGuestAgent: true}
	out, err := seedUserData(v, "")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseCloudConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]map[string]any{}
	for _, f := range doc["write_files"].([]any) {
		m := f.(map[string]any)
		files[m["path"].(string)] = m
	}
	if unit := files["/etc/systemd/system/anvil-guest.service"]; unit == nil || !strings.Contains(unit["content"].(string), "--port 1025") {
		t.Errorf("systemd unit = %v", unit)
	}
	if rc := files[anvilGuestOpenRCPath]; rc == nil || !strings.HasPrefix(rc["content"].(string), "#!/sbin/openrc-run") {
		t.Errorf("OpenRC script = %v", rc)
	}
	if _, ok := files["/etc/init.d/anvil-guest"]; ok {
		t.Error("the OpenRC script went straight to /etc/init.d, where systemd's SysV compatibility trips on it")
	}
	runcmd := doc["runcmd"].([]any)
	if len(runcmd) == 0 || runcmd[len(runcmd)-1] != anvilGuestInstallCmd {
		t.Errorf("runcmd = %v, want the anvil-guest install last", runcmd)
	}
	if out, err := exec.Command("sh", "-n", "-c", anvilGuestInstallCmd).CombinedOutput(); err != nil {
		t.Errorf("install command doesn't parse: %v: %s", err, out)
	}

	v.AnvilGuest = false
	if out, _ := seedUserData(v, ""); strings.Contains(out, "anvil-guest") {
		t.Errorf("anvil-guest without AnvilGuest:\n%s", out)
	}
	qemu := &instance.VMSpec{AnvilGuest: true, NoGuestAgent: true}
	if out, _ := seedUserData(qemu, ""); strings.Contains(out, "anvil-guest") {
		t.Errorf("a QEMU VM got anvil-guest:\n%s", out)
	}
}

func TestGuestISO(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not installed, skipping")
	}
	dir := t.TempDir()
	t.Setenv("ANVIL_GUEST_TOOLS", dir)
	b := &FCBackend{Cache: &image.FCCache{Dir: t.TempDir()}}
	if _, err := b.guestISO(); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Fatalf("guestISO without the binary = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, guestBinary), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	iso, err := b.guestISO()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("xorriso", "-indev", iso, "-pvd_info").CombinedOutput()
	if err != nil || !strings.Contains(string(out), guestTag) {
		t.Fatalf("ISO label: %v\n%s", err, out)
	}
	out, err = exec.Command("xorriso", "-indev", iso, "-find", "/"+guestBinary, "-exec", "lsdl").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "-rwxr-xr-x") {
		t.Errorf("anvil-guest in the ISO isn't executable: %v\n%s", err, out)
	}
}
