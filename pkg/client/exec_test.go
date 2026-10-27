package client

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// fakeBins puts empty executables with the given names first on PATH.
func fakeBins(t *testing.T, names ...string) {
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestContainerExecCommand(t *testing.T) {
	fakeBins(t, "docker", "podman")
	t.Setenv("HOME", t.TempDir())
	c := &Client{}
	inst := &anvilv1.Instance{
		Name:      "web",
		State:     anvilv1.State_STATE_RUNNING,
		Container: &anvilv1.ContainerSpec{ContainerId: "abc"},
	}
	cmd, err := c.ContainerExecCommand(inst, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args[1:]; !slices.Equal(got, []string{"exec", "-i", "-t", "abc", "/bin/sh"}) {
		t.Errorf("shell args: %v", got)
	}
	cmd, err = c.ContainerExecCommand(inst, []string{"ls", "-l"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.Args[1:]; !slices.Equal(got, []string{"exec", "-i", "abc", "ls", "-l"}) {
		t.Errorf("exec args: %v", got)
	}

	inst.Container.Engine = anvilv1.ContainerEngine_CONTAINER_ENGINE_PODMAN
	if cmd, _ = c.ContainerExecCommand(inst, nil, false); filepath.Base(cmd.Path) != "podman" {
		t.Errorf("podman engine ran %s", cmd.Path)
	}

	inst.State = anvilv1.State_STATE_STOPPED
	if _, err := c.ContainerExecCommand(inst, nil, false); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("stopped container: %v", err)
	}
	if _, err := c.ContainerExecCommand(&anvilv1.Instance{Name: "vm"}, nil, false); err == nil {
		t.Error("a VM must be rejected")
	}
}

func TestSSHCommand(t *testing.T) {
	fakeBins(t, "ssh")
	t.Setenv("HOME", t.TempDir())
	target := SSHTarget{InstanceID: "id1", Host: "localhost", Port: 2222, User: "ubuntu"}
	cmd, err := SSHCommand(target, "/key", []string{"uptime"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-p 2222", "-i /key", "ubuntu@localhost uptime"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in %q", want, args)
		}
	}
	if cmd.Stdin != os.Stdin {
		t.Error("ssh must inherit stdin")
	}
}
