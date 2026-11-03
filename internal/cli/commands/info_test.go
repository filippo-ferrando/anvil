package commands

import (
	"bytes"
	"strings"
	"testing"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

func TestSSHHint(t *testing.T) {
	local := sshHint("", "", 2222, "ubuntu", "web")
	if want := "SSH:\tssh -p 2222 ubuntu@localhost  (or just: anvil shell web)\n"; local != want {
		t.Errorf("local hint = %q, want %q", local, want)
	}
	remote := sshHint("ops@box", "10.0.0.7", 2222, "ubuntu", "web")
	want := "SSH:\tssh -J ops@box -p 2222 ubuntu@localhost  (or just: anvil --remote ops@box shell web)\nHost:\t10.0.0.7\n"
	if remote != want {
		t.Errorf("remote hint = %q, want %q", remote, want)
	}
}

func TestPrintContainerInfo(t *testing.T) {
	var buf bytes.Buffer
	printContainerInfo(&buf, &anvilv1.ContainerSpec{
		ImageRef: "alpine",
		Volumes:  []*anvilv1.VolumeMount{{HostPath: "/srv", ContainerPath: "/data", ReadOnly: true}},
	})
	out := buf.String()
	for _, want := range []string{"Image:\talpine\n", "Volume:\t/srv -> /data (ro)\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "Container ID") {
		t.Errorf("output %q shows an empty container ID", out)
	}
}
