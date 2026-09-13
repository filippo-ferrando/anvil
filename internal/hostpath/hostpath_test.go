package hostpath

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestHintCoversEveryAncestor(t *testing.T) {
	hint := Hint("/home/rdfilippo/Desktop/Workspace/test")
	for _, want := range []string{
		"setfacl -m u:anvil:x /home",
		"setfacl -m u:anvil:x /home/rdfilippo",
		"setfacl -m u:anvil:x /home/rdfilippo/Desktop",
		"setfacl -m u:anvil:x /home/rdfilippo/Desktop/Workspace",
		"setfacl -R -m u:anvil:rwx /home/rdfilippo/Desktop/Workspace/test",
		"setfacl -R -d -m u:anvil:rwx /home/rdfilippo/Desktop/Workspace/test",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("expected hint to contain %q, got:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "setfacl -m u:anvil:x /home/rdfilippo/Desktop/Workspace/test\n") {
		t.Errorf("did not expect the target itself in the ancestor (traverse-only) list:\n%s", hint)
	}
}

func TestHintTrailingSlash(t *testing.T) {
	hint := Hint("/srv/shared/")
	if !strings.Contains(hint, "setfacl -R -m u:anvil:rwx /srv/shared\n") {
		t.Errorf("expected the cleaned path without a trailing slash, got:\n%s", hint)
	}
}

func TestAncestorsExcludesRootAndTarget(t *testing.T) {
	got := Ancestors("/a/b/c")
	want := []string{"/a", "/a/b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestGrantSkipsWhenAnvilUserMissing(t *testing.T) {
	// Force the "anvil user doesn't exist" path regardless of whatever
	// this machine actually has, so the test is deterministic — Grant
	// should no-op rather than failing, same as a manual dev setup
	// running anvild as root.
	orig := lookupUser
	lookupUser = func(string) (*user.User, error) { return nil, errors.New("no such user") }
	defer func() { lookupUser = orig }()

	if err := Grant(t.TempDir()); err != nil {
		t.Fatalf("expected Grant to no-op without the anvil user, got: %v", err)
	}
}
