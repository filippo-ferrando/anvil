package commands

import "testing"

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
