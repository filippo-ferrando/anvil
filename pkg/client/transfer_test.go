package client

import (
	"slices"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestParseTransfer(t *testing.T) {
	got, err := ParseTransfer("./a.txt", "dev:/tmp/a.txt")
	if err != nil || got != (Transfer{Instance: "dev", InstancePath: "/tmp/a.txt", LocalPath: "./a.txt", ToInstance: true}) {
		t.Fatalf("to instance: %+v %v", got, err)
	}
	got, err = ParseTransfer("dev:/etc/hosts", "hosts")
	if err != nil || got != (Transfer{Instance: "dev", InstancePath: "/etc/hosts", LocalPath: "hosts"}) {
		t.Fatalf("from instance: %+v %v", got, err)
	}
	for _, args := range [][2]string{{"a", "b"}, {"x:a", "y:b"}} {
		if _, err := ParseTransfer(args[0], args[1]); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestTransferCommandVM(t *testing.T) {
	t.Setenv(muxEnv, "off") // the plain -J form; TestJumpArgs covers the shared connection
	fakeBins(t, "scp")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	inst := &anvilv1.Instance{
		Id: "id1", Name: "dev", State: anvilv1.State_STATE_RUNNING,
		Vm: &anvilv1.VMSpec{SshPort: 2222, DefaultUser: "ubuntu"},
	}
	c := &Client{Remote: "me@far"}

	cmd, err := c.TransferCommand(inst, Transfer{Instance: "dev", InstancePath: "/tmp/x", LocalPath: "x", ToInstance: true}, "", "/k")
	if err != nil {
		t.Fatal(err)
	}
	args := cmd.Args[1:]
	if !slices.Equal(args[:2], []string{"-P", "2222"}) || !slices.Contains(args, "-r") {
		t.Errorf("flags: %v", args)
	}
	if i := slices.Index(args, "-J"); i < 0 || args[i+1] != "me@far" {
		t.Errorf("no jump through the remote host: %v", args)
	}
	if i := slices.Index(args, "-i"); i < 0 || args[i+1] != "/k" {
		t.Errorf("identity: %v", args)
	}
	if tail := args[len(args)-2:]; !slices.Equal(tail, []string{"x", "ubuntu@localhost:/tmp/x"}) {
		t.Errorf("to instance: %v", tail)
	}

	cmd, err = c.TransferCommand(inst, Transfer{Instance: "dev", InstancePath: "/tmp/x", LocalPath: "x"}, "root", "/k")
	if err != nil {
		t.Fatal(err)
	}
	if tail := cmd.Args[len(cmd.Args)-2:]; !slices.Equal(tail, []string{"root@localhost:/tmp/x", "x"}) {
		t.Errorf("from instance: %v", tail)
	}

	inst.State = anvilv1.State_STATE_STOPPED
	if _, err := c.TransferCommand(inst, Transfer{}, "", "/k"); err == nil || !strings.Contains(err.Error(), "isn't running") {
		t.Errorf("stopped VM: %v", err)
	}
}

func TestTransferCommandContainer(t *testing.T) {
	fakeBins(t, "docker")
	inst := &anvilv1.Instance{Name: "web", Container: &anvilv1.ContainerSpec{ContainerId: "abc"}}
	c := &Client{Remote: "me@far"}

	cmd, err := c.TransferCommand(inst, Transfer{InstancePath: "/srv", LocalPath: "site", ToInstance: true}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args[1:], []string{"cp", "site", "abc:/srv"}) {
		t.Errorf("to container: %v", cmd.Args)
	}
	if !slices.Contains(cmd.Env, "DOCKER_HOST=ssh://me@far") {
		t.Error("no DOCKER_HOST for the remote host")
	}
	cmd, _ = c.TransferCommand(inst, Transfer{InstancePath: "/srv", LocalPath: "site"}, "", "")
	if !slices.Equal(cmd.Args[1:], []string{"cp", "abc:/srv", "site"}) {
		t.Errorf("from container: %v", cmd.Args)
	}

	inst.Container.ContainerId = ""
	if _, err := c.TransferCommand(inst, Transfer{}, "", ""); err == nil {
		t.Error("a container without an ID must be rejected")
	}
}
