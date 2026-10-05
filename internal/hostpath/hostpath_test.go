package hostpath

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestHintCoversEveryAncestor(t *testing.T) {
	hint := Hint("/nonexistent-anvil/rdfilippo/Desktop/Workspace/test")
	for _, want := range []string{
		"setfacl -m u:anvil:x /nonexistent-anvil\n",
		"setfacl -m u:anvil:x /nonexistent-anvil/rdfilippo",
		"setfacl -m u:anvil:x /nonexistent-anvil/rdfilippo/Desktop",
		"setfacl -m u:anvil:x /nonexistent-anvil/rdfilippo/Desktop/Workspace",
		"setfacl -R -m u:anvil:rwx /nonexistent-anvil/rdfilippo/Desktop/Workspace/test",
		"setfacl -R -d -m u:anvil:rwx /nonexistent-anvil/rdfilippo/Desktop/Workspace/test",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("expected hint to contain %q, got:\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "setfacl -m u:anvil:x /nonexistent-anvil/rdfilippo/Desktop/Workspace/test\n") {
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
	// Force the "anvil user doesn't exist" path.
	orig := lookupUser
	lookupUser = func(string) (*user.User, error) { return nil, errors.New("no such user") }
	defer func() { lookupUser = orig }()

	if err := Grant(t.TempDir()); err != nil {
		t.Fatalf("expected Grant to no-op without the anvil user, got: %v", err)
	}
}

func TestAncestorsResolvesRelativePath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got := Ancestors("a/b")
	if len(got) == 0 || got[0] == "a" || got[len(got)-1] != filepath.Join(dir, "a") {
		t.Fatalf("expected absolute ancestors ending in %s, got %v", filepath.Join(dir, "a"), got)
	}
	if got[0] != "/"+strings.Split(dir, string(os.PathSeparator))[1] {
		t.Fatalf("expected ancestors to start at the top-level dir, got %v", got)
	}
}

func TestNeedsTraverseSkipsOtherExecutable(t *testing.T) {
	root := t.TempDir()
	open, closed := filepath.Join(root, "open"), filepath.Join(root, "open", "closed")
	if err := os.MkdirAll(filepath.Join(closed, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(needsTraverse(filepath.Join(closed, "target")), " ")
	if strings.Contains(got, open+" ") || !strings.HasSuffix(got, closed) {
		t.Fatalf("expected only %s among the dirs needing an ACL, got %q", closed, got)
	}
}

func TestUnder(t *testing.T) {
	base := "/var/lib/anvil/staging"
	tests := []struct {
		name, rel, want string
		wantErr         bool
	}{
		{name: "plain", rel: "disk.qcow2", want: base + "/disk.qcow2"},
		{name: "nested", rel: "mounts/0", want: base + "/mounts/0"},
		{name: "leading slash is relative", rel: "/disk.qcow2", want: base + "/disk.qcow2"},
		{name: "dot dot is clamped", rel: "../../../etc", want: base + "/etc"},
		{name: "escape attempt", rel: "x/../../../../etc/shadow", want: base + "/etc/shadow"},
		{name: "empty is the base itself", rel: "", want: base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Under(base, tc.rel)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Under(%q, %q) error = %v, wantErr %v", base, tc.rel, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("Under(%q, %q) = %q, want %q", base, tc.rel, got, tc.want)
			}
			if !tc.wantErr && !strings.HasPrefix(got, base) {
				t.Errorf("Under(%q, %q) = %q, which escapes the base", base, tc.rel, got)
			}
		})
	}
}
