//go:build linux

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/anvil-project/anvil/internal/instance"
)

// guestShell runs a guest-side script on this host, with /etc/fstab and the
// generation marker redirected to temp files and mount/umount/mountpoint stubbed.
type guestShell struct {
	t      *testing.T
	fstab  string
	marker string
	bin    string
	log    string // every stubbed mount/umount call, one per line
}

func newGuestShell(t *testing.T, fstab string) *guestShell {
	t.Helper()
	dir := t.TempDir()
	g := &guestShell{
		t:      t,
		fstab:  filepath.Join(dir, "fstab"),
		marker: filepath.Join(dir, "var", "anvil-mounts.gen"),
		bin:    filepath.Join(dir, "bin"),
		log:    filepath.Join(dir, "calls"),
	}
	if err := os.WriteFile(g.fstab, []byte(fstab), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(g.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mount", "umount"} {
		stub := "#!/bin/sh\necho " + name + " \"$@\" >> " + g.log + "\n"
		if err := os.WriteFile(filepath.Join(g.bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Nothing is mounted yet, as far as the scripts can tell.
	if err := os.WriteFile(filepath.Join(g.bin, "mountpoint"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *guestShell) run(script string) {
	g.t.Helper()
	script = strings.ReplaceAll(script, "/etc/fstab", g.fstab)
	script = strings.ReplaceAll(script, mountsGenMarker, g.marker)
	script = strings.ReplaceAll(script, "mkdir -p "+filepath.Dir(mountsGenMarker), "mkdir -p "+filepath.Dir(g.marker))
	// Guest paths are created under the temp dir instead of /.
	script = strings.ReplaceAll(script, "mkdir -p '/", "mkdir -p '"+filepath.Dir(g.fstab)+"/")
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+g.bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		g.t.Fatalf("script failed: %v\n%s\nscript:\n%s", err, out, script)
	}
}

func (g *guestShell) fstabContent() string {
	data, _ := os.ReadFile(g.fstab)
	return string(data)
}

func (g *guestShell) calls() string {
	data, _ := os.ReadFile(g.log)
	return string(data)
}

func bootcmdScript(t *testing.T, userData string) string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(userData)), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, userData)
	}
	bootcmd, _ := doc["bootcmd"].([]any)
	if len(bootcmd) == 0 {
		t.Fatalf("expected a bootcmd, got:\n%s", userData)
	}
	return bootcmd[0].(string)
}

func TestFstabLine(t *testing.T) {
	got := fstabLine(instance.Mount{Tag: "mount2", GuestPath: "/srv/my data", ReadOnly: true})
	want := `mount2 /srv/my\040data virtiofs defaults,nofail,ro 0 0`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMergeMountsNothingToDo(t *testing.T) {
	out, err := mergeMounts("#cloud-config\n{}\n", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out != "#cloud-config\n{}\n" {
		t.Errorf("expected no change with no mounts at generation 0, got %q", out)
	}
}

func TestMergeMountsKeepsExistingBootcmd(t *testing.T) {
	existing := "#cloud-config\nbootcmd:\n  - echo existing\n"
	out, err := mergeMounts(existing, []instance.Mount{{Tag: "mount0", GuestPath: "/mnt/a"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(stripHeader(out)), &doc); err != nil {
		t.Fatal(err)
	}
	bootcmd, _ := doc["bootcmd"].([]any)
	if len(bootcmd) != 2 || bootcmd[1] != "echo existing" {
		t.Errorf("expected the mounts script first and the existing bootcmd kept, got %v", bootcmd)
	}
	if _, ok := doc["mounts"]; ok {
		t.Error("expected no cloud-init mounts module entries, the bootcmd manages fstab")
	}
}

func TestMountsBootScriptRewritesFstabOncePerGeneration(t *testing.T) {
	legacy := "UUID=abc / ext4 defaults 0 1\n" +
		"mount0 /mnt/old 9p trans=virtio,version=9p2000.L,rw,nofail,comment=cloudconfig 0 0\n"
	g := newGuestShell(t, legacy)

	mounts := []instance.Mount{
		{Tag: "mount0", GuestPath: "/mnt/old"},
		{Tag: "mount1", GuestPath: "/mnt/ro", ReadOnly: true},
	}
	out, err := mergeMounts("", mounts, 3)
	if err != nil {
		t.Fatal(err)
	}
	script := bootcmdScript(t, out)
	g.run(script)

	want := "UUID=abc / ext4 defaults 0 1\n" +
		"mount0 /mnt/old virtiofs defaults,nofail 0 0\n" +
		"mount1 /mnt/ro virtiofs defaults,nofail,ro 0 0\n"
	if got := g.fstabContent(); got != want {
		t.Errorf("fstab after first boot:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(g.calls(), "mount /mnt/old") {
		t.Errorf("expected the mounts to be mounted, calls:\n%s", g.calls())
	}

	// A live umount since then removed mount1; later boots of the same generation keep that.
	g.run(umountLiveScript(mounts[1]))
	g.run(script)
	if got := g.fstabContent(); strings.Contains(got, "mount1") {
		t.Errorf("expected a later boot not to bring back a live-removed mount, fstab:\n%s", got)
	}
}

func TestMountLiveScriptReplacesItsOwnLine(t *testing.T) {
	g := newGuestShell(t, "UUID=abc / ext4 defaults 0 1\nmount4 /old virtiofs defaults,nofail 0 0\nmount40 /other virtiofs defaults,nofail 0 0\n")
	m := instance.Mount{Tag: "mount4", GuestPath: "/data"}
	g.run(mountLiveScript(m))
	want := "UUID=abc / ext4 defaults 0 1\nmount40 /other virtiofs defaults,nofail 0 0\nmount4 /data virtiofs defaults,nofail 0 0\n"
	if got := g.fstabContent(); got != want {
		t.Errorf("fstab:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(g.calls(), "mount /data") {
		t.Errorf("expected a mount call, got:\n%s", g.calls())
	}
}
