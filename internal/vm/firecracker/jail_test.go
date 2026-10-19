//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestJailedCommand(t *testing.T) {
	jail := &Jail{ID: "01JX", ChrootBase: "/var/lib/anvil/jailer", UID: 961, GID: 961}
	root := jail.Root()
	if root != "/var/lib/anvil/jailer/firecracker/01JX/root" {
		t.Fatalf("Root = %s", root)
	}
	p := Paths{
		APISocket: root + "/firecracker.sock", ConfigFile: root + "/firecracker.json",
		LogFile: root + "/firecracker.log", ConsoleLog: "/var/lib/anvil/instances/01JX/console.log", Jail: jail,
	}
	if got := p.InVMM(root + "/disk.raw"); got != "/disk.raw" {
		t.Errorf("InVMM = %s, want /disk.raw", got)
	}

	bin, args, err := p.command()
	if err != nil {
		t.Skipf("firecracker not on PATH: %v", err)
	}
	line := strings.Join(args, " ")
	for _, want := range []string{
		"--id 01JX", "--uid 961 --gid 961", "--chroot-base-dir /var/lib/anvil/jailer", "--cgroup-version 2",
		"-- --api-sock /firecracker.sock --config-file /firecracker.json --log-path /firecracker.log",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("jailer args lack %q: %s", want, line)
		}
	}
	if bin != JailerBinary || !strings.Contains(line, "--exec-file /") {
		t.Errorf("command = %s %s", bin, line)
	}

	direct := Paths{APISocket: "/i/fc.sock", ConfigFile: "/i/fc.json", LogFile: "/i/fc.log"}
	bin, args, _ = direct.command()
	if bin != Binary || !slices.Contains(args, "/i/fc.sock") || direct.InVMM("/x") != "/x" {
		t.Errorf("unjailed command = %s %v", bin, args)
	}
}

func TestHasID(t *testing.T) {
	for _, args := range [][]string{{"firecracker", "--id=01JX"}, {"firecracker", "--id", "01JX"}} {
		if !hasID(args, "01JX") {
			t.Errorf("hasID(%v) = false", args)
		}
	}
	if hasID([]string{"firecracker", "--id=01JXY"}, "01JX") || hasID([]string{"firecracker", "--id"}, "01JX") {
		t.Error("hasID matched another id")
	}
}

func TestJailNamedAfterExecFile(t *testing.T) {
	jail := &Jail{ID: "J2", ChrootBase: "/b", ExecFile: "/opt/fc/firecracker-v1.17.0-x86_64", Jailer: "/var/lib/anvil-jailer/jailer"}
	// The jailer names the chroot after the file it runs.
	if root := jail.Root(); root != "/b/firecracker-v1.17.0-x86_64/J2/root" {
		t.Fatalf("Root = %s", root)
	}
	p := Paths{APISocket: jail.Root() + "/fc.sock", ConfigFile: jail.Root() + "/fc.json", LogFile: jail.Root() + "/fc.log", Jail: jail}
	bin, args, err := p.command()
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(args, " ")
	if bin != jail.Jailer || !strings.Contains(line, "--exec-file /opt/fc/firecracker-v1.17.0-x86_64") {
		t.Fatalf("command = %s %s", bin, line)
	}
}

// helperEnv makes the test binary, re-run by startNamed, sleep under another argv[0].
const helperEnv = "ANVIL_FC_TEST_HELPER"

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

// startNamed runs a process whose cmdline is argv, as firecracker's would be.
func startNamed(t *testing.T, argv ...string) *exec.Cmd {
	t.Helper()
	cmd := &exec.Cmd{Path: os.Args[0], Args: append([]string{argv[0], "-test.run=^TestHelperProcess$", "--"}, argv[1:]...), Env: append(os.Environ(), helperEnv+"=1")}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// Right after the fork the cmdline is still the parent's, until the exec lands.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", cmd.Process.Pid)); strings.HasPrefix(string(data), argv[0]+"\x00") {
			return cmd
		}
	}
	t.Fatalf("%s never showed up as %s", os.Args[0], argv[0])
	return nil
}

func TestLooksLikeOursJailed(t *testing.T) {
	jail := &Jail{ID: "J3", ChrootBase: "/b", ExecFile: "/opt/firecracker-v1.17.0"}
	paths := Paths{APISocket: jail.Root() + "/firecracker.sock", Jail: jail}
	ours := startNamed(t, "firecracker-v1.17.0", "--id", "J3", "--api-sock", "/firecracker.sock")
	if !looksLikeOurs(ours.Process.Pid, paths) {
		t.Error("a jailed firecracker under its versioned name wasn't recognised")
	}
	other := startNamed(t, "firecracker-v1.17.0", "--id", "J4", "--api-sock", "/firecracker.sock")
	if looksLikeOurs(other.Process.Pid, paths) {
		t.Error("another jail's firecracker was taken as ours")
	}
	stranger := startNamed(t, "sleep", "--id", "J3", "--api-sock", "/firecracker.sock")
	if looksLikeOurs(stranger.Process.Pid, paths) {
		t.Error("a process that isn't firecracker was taken as ours")
	}
}

// Once the process is known gone its pid may be someone else's, so kill must not signal it.
func TestKillAfterExitSendsNothing(t *testing.T) {
	bystander := startNamed(t, "bystander")
	exited := make(chan struct{})
	close(exited)
	p := &Process{pid: bystander.Process.Pid, exited: exited}
	done := make(chan error, 1)
	go func() { done <- p.kill() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("kill blocked on a process already gone")
	}
	if err := syscall.Kill(bystander.Process.Pid, 0); err != nil {
		t.Fatalf("kill signalled the reused pid: %v", err)
	}
}
