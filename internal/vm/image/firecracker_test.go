package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// partitioned makes a sparse disk at path laid out by an sfdisk script.
func partitioned(t *testing.T, path, script string) {
	t.Helper()
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk not installed, skipping")
	}
	if err := sparseFile(path, 64<<20); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sfdisk", "--quiet", path)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v: %s", err, out)
	}
}

func sparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func TestRootPartUUID(t *testing.T) {
	const (
		esp       = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
		bootUUID  = "11111111-2222-3333-4444-555555555555"
		dataUUID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		rootUUID  = "01234567-89ab-cdef-0123-456789abcdef"
		linuxData = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	)
	dir := t.TempDir()
	cases := []struct {
		name, script, arch, want string
	}{
		{
			// Ubuntu/Debian style: a small ESP and /boot, then the large Linux data root.
			name: "largest linux data",
			script: "label: gpt\n" +
				"size=8MiB, type=" + esp + "\n" +
				"size=8MiB, type=" + linuxData + ", uuid=" + bootUUID + "\n" +
				"type=" + linuxData + ", uuid=" + dataUUID + "\n",
			arch: "x86_64", want: dataUUID,
		},
		{
			// A partition typed as the arch's root wins even when it isn't the largest.
			name: "discoverable root type",
			script: "label: gpt\n" +
				"size=8MiB, type=4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709, uuid=" + rootUUID + "\n" +
				"type=" + linuxData + ", uuid=" + dataUUID + "\n",
			arch: "x86_64", want: rootUUID,
		},
		{
			name:   "mbr",
			script: "label: dos\nlabel-id: 0x1234abcd\nsize=8MiB, type=c\ntype=83\n",
			arch:   "x86_64", want: "1234abcd-02",
		},
	}
	for _, c := range cases {
		path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+".raw")
		partitioned(t, path, c.script)
		got, err := RootPartUUID(path, c.arch)
		if err != nil || got != c.want {
			t.Errorf("%s: RootPartUUID = %q, %v; want %q", c.name, got, err, c.want)
		}
	}

	blank := filepath.Join(dir, "blank.raw")
	if err := sparseFile(blank, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := RootPartUUID(blank, "x86_64"); err == nil {
		t.Error("expected an error for a disk without a partition table")
	}
}

func TestFirecrackerCatalogMatchesMain(t *testing.T) {
	c, err := LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.fcDistros) == 0 {
		t.Fatal("firecracker catalog lists no distros")
	}
	// Every Firecracker distro must resolve to an image in the main catalog.
	for _, d := range c.fcDistros {
		if _, err := c.Find(d.v.ID, ""); err != nil {
			t.Errorf("firecracker distro %s: %v", d.v.ID, err)
		}
	}
	if _, err := c.FirecrackerKernel("x86_64"); err != nil {
		t.Error(err)
	}
	if _, err := c.FirecrackerDistro("no-such-distro"); err == nil {
		t.Error("expected an error for an unknown distro")
	}
}

func TestFirecrackerMirror(t *testing.T) {
	c, err := LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	mirror := `{"schema_version": 1, "distros": [], "firecracker": {
		"kernels": [{"arch": "x86_64", "version": "9.9", "url": "https://mirror/k"}],
		"distros": [{"id": "fedora-44", "kernel_args": "rootflags=subvol=@"}, {"id": "mine-1"}]}}`
	merged, err := c.WithMirrors([]MirrorManifest{{ManifestJSON: mirror, Priority: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := merged.FirecrackerKernel("x86_64"); k.Version != "9.9" {
		t.Errorf("kernel = %+v, want the higher-priority mirror's", k)
	}
	if d, _ := merged.FirecrackerDistro("fedora-44"); d.KernelArgs != "rootflags=subvol=@" {
		t.Errorf("fedora-44 = %+v, want the mirror's kernel args", d)
	}
	if _, err := merged.FirecrackerDistro("mine-1"); err != nil {
		t.Error(err)
	}
	if k, _ := c.FirecrackerKernel("x86_64"); k.Version == "9.9" {
		t.Error("WithMirrors changed the base catalog")
	}

	// A manifest without the section, as older ones are, still loads and adds nothing.
	plain, err := c.WithMirrors([]MirrorManifest{{ManifestJSON: `{"schema_version": 1, "distros": []}`, Priority: 10}})
	if err != nil || len(plain.fcKernels) != len(c.fcKernels) {
		t.Fatalf("plain mirror: %v, %d kernels", err, len(plain.fcKernels))
	}
}

func TestCloneDisk(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "base.raw")
	if err := os.WriteFile(src, []byte("bootsector"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(src, 1<<30); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dst := filepath.Join(dir, "disk.raw")
	if err := CloneDisk(ctx, src, dst, 2); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dst)
	if info.Size() != 2<<30 {
		t.Fatalf("clone size = %d, want 2GiB", info.Size())
	}
	if data, _ := os.ReadFile(dst); !strings.HasPrefix(string(data[:10]), "bootsector") {
		t.Fatal("clone lost the source's content")
	}

	same := filepath.Join(dir, "same.raw")
	if err := CloneDisk(ctx, src, same, 0); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(same); info.Size() != 1<<30 {
		t.Fatalf("size 0 should keep the source size, got %d", info.Size())
	}

	if err := CloneDisk(ctx, dst, filepath.Join(dir, "small.raw"), 1); err == nil {
		t.Fatal("expected an error when asked to shrink")
	}
}

func TestFCCacheRawBase(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed, skipping")
	}
	vault := NewVault(t.TempDir())
	entry := DistroEntry{ID: "test-1", Arch: "x86_64", URL: "http://unused.invalid/"}
	// An already cached base with no published checksum is used as is, never downloaded.
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", vault.preparedPath(entry), "16M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %v: %s", err, out)
	}
	cache := &FCCache{Dir: t.TempDir(), Vault: vault}
	raw, err := cache.RawBase(context.Background(), entry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(raw); err != nil || info.Size() != 16<<20 {
		t.Fatalf("raw base = %v, %v; want a 16MiB file", info, err)
	}
	before, _ := os.Stat(raw)
	if again, err := cache.RawBase(context.Background(), entry, nil); err != nil || again != raw {
		t.Fatalf("second RawBase = %q, %v", again, err)
	}
	if after, _ := os.Stat(raw); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("a fresh raw copy was converted again")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(cache.Dir, "*.part-*")); len(leftovers) > 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestFCCacheKernelOverride(t *testing.T) {
	t.Setenv(KernelEnv, "/opt/vmlinux")
	cache := &FCCache{Dir: t.TempDir(), Vault: NewVault(t.TempDir())}
	if got, err := cache.Kernel(context.Background(), FCKernel{Arch: "x86_64"}, nil); err != nil || got != "/opt/vmlinux" {
		t.Fatalf("Kernel = %q, %v; want the %s path", got, err, KernelEnv)
	}
}
