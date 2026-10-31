package client

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestMuxArgs(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	args := muxArgs()
	if !slices.Contains(args, "ControlMaster=auto") || !slices.Contains(args, "ControlPersist=60s") {
		t.Fatalf("muxArgs = %v", args)
	}
	if path := args[slices.Index(args, "ControlMaster=auto")+2]; !strings.HasPrefix(path, "ControlPath="+os.Getenv("XDG_RUNTIME_DIR")) ||
		!strings.HasSuffix(path, "%C") {
		t.Errorf("control path %q, want one per host in the runtime dir", path)
	}
	t.Setenv(muxEnv, "off")
	if args := muxArgs(); args != nil {
		t.Errorf("with %s=off: %v", muxEnv, args)
	}
}

// Without XDG_RUNTIME_DIR the socket goes in a private dir of ours, never a shared one.
func TestMuxArgsPrivateDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if args := muxArgs(); args == nil {
		t.Fatal("no mux with a fresh private dir")
	}
	dir := tmp + "/anvil-ssh-" + strconv.Itoa(os.Getuid())
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("private dir %v, %v", st, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if args := muxArgs(); args != nil {
		t.Errorf("used a dir others can write: %v", args)
	}
}

func TestJumpArgs(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	c := &Client{Remote: "ops@build-1"}
	args := c.jumpArgs()
	if len(args) != 2 || args[0] != "-o" {
		t.Fatalf("jumpArgs = %v", args)
	}
	want := "ProxyCommand=ssh '-o' 'ControlMaster=auto' '-o' 'ControlPath=/run/user/1000/anvil-ssh-%%C' '-o' 'ControlPersist=60s' -W %h:%p 'ops@build-1'"
	if args[1] != want {
		t.Errorf("proxy command\n got %s\nwant %s", args[1], want)
	}
	t.Setenv(muxEnv, "off")
	if args := c.jumpArgs(); !slices.Equal(args, []string{"-J", "ops@build-1"}) {
		t.Errorf("without mux: %v", args)
	}
	if args := (&Client{}).jumpArgs(); args != nil {
		t.Errorf("local: %v", args)
	}
}
