//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	if filepath.Base(bin) != JailerBinary || !strings.Contains(line, "--exec-file /") {
		t.Errorf("command = %s %s", bin, line)
	}

	// A snapshot restore starts with no config file, in the clone's network namespace.
	p.ConfigFile = ""
	p.Jail.NetNS = "/run/netns/anvil-01JX"
	_, args, _ = p.command()
	line = strings.Join(args, " ")
	if slices.Contains(args, "--config-file") ||
		!strings.HasSuffix(line, "--netns /run/netns/anvil-01JX -- --api-sock /firecracker.sock --log-path /firecracker.log") {
		t.Errorf("restore args = %s", line)
	}

	// Unjailed with a working directory: relative paths, and nsenter for the netns.
	rel := Paths{APISocket: "/i/root/fc.sock", LogFile: "/i/fc.log", Chdir: "/i/root", NetNS: "/run/netns/x"}
	if got := rel.InVMM("/i/root/disk.raw"); got != "disk.raw" {
		t.Errorf("InVMM under Chdir = %s, want disk.raw", got)
	}
	if got := rel.InVMM("/i/fc.log"); got != "/i/fc.log" {
		t.Errorf("InVMM outside Chdir = %s, want it unchanged", got)
	}
	bin, args, _ = rel.command()
	if bin != "nsenter" || args[0] != "--net=/run/netns/x" || filepath.Base(args[2]) != Binary || !slices.Contains(args, "fc.sock") {
		t.Errorf("unjailed netns command = %s %v", bin, args)
	}

	direct := Paths{APISocket: "/i/fc.sock", ConfigFile: "/i/fc.json", LogFile: "/i/fc.log"}
	bin, args, _ = direct.command()
	if filepath.Base(bin) != Binary || !slices.Contains(args, "/i/fc.sock") || direct.InVMM("/x") != "/x" {
		t.Errorf("unjailed command = %s %v", bin, args)
	}
}

func TestLookPathPrefersBundledDir(t *testing.T) {
	bundled, onPath := t.TempDir(), t.TempDir()
	for _, f := range []string{filepath.Join(bundled, Binary), filepath.Join(onPath, Binary), filepath.Join(onPath, JailerBinary)} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANVIL_FIRECRACKER_DIR", bundled)
	t.Setenv("PATH", onPath)

	if got, err := LookPath(Binary); err != nil || got != filepath.Join(bundled, Binary) {
		t.Errorf("LookPath(firecracker) = %q, %v; want the bundled one", got, err)
	}
	if got, err := LookPath(JailerBinary); err != nil || got != filepath.Join(onPath, JailerBinary) {
		t.Errorf("LookPath(jailer) = %q, %v; want the PATH one", got, err)
	}
	if _, err := LookPath("missing"); err == nil {
		t.Error("LookPath(missing) found something")
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

func TestIsDynamic(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	sh, _ = filepath.EvalSymlinks(sh)
	if !IsDynamic(sh) {
		t.Skipf("%s is static here", sh)
	}
	if IsDynamic(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing file counts as dynamic")
	}
}

func TestLogTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fc.log")
	os.WriteFile(path, []byte("a\nb\nc\nFailed to exec\nError: Exec\n"), 0o640)
	if got := logTail(path); got != "c; Failed to exec; Error: Exec" {
		t.Fatalf("logTail = %q", got)
	}
}
