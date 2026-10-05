package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// localSSH makes sshCommand run the "remote" side on this machine. rewrite, when
// non-nil, may replace a command before it runs.
func localSSH(t *testing.T, rewrite func(remote string) string) {
	t.Helper()
	orig := sshCommand
	t.Cleanup(func() { sshCommand = orig })
	sshCommand = func(ctx context.Context, _ target, remote string) (*exec.Cmd, error) {
		if rewrite != nil {
			remote = rewrite(remote)
		}
		return exec.CommandContext(ctx, "sh", "-c", remote), nil
	}
}

func writeRandomFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseRemoteCaps(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	caps := parseRemoteCaps("zstd\nsha256sum\nbase=" + sum + "\n")
	if !caps.zstd || !caps.sha256sum || caps.baseSHA != sum {
		t.Errorf("unexpected caps: %+v", caps)
	}
	caps = parseRemoteCaps("base=\n")
	if caps.zstd || caps.sha256sum || caps.baseSHA != "" {
		t.Errorf("expected empty caps, got %+v", caps)
	}
	if parseRemoteCaps("base=not-a-sum").baseSHA != "" {
		t.Error("expected a malformed checksum to be ignored")
	}
}

func TestParseRemoteCapsFields(t *testing.T) {
	caps := parseRemoteCaps("arch=aarch64\nanvil=/usr/bin/anvil\nanvild=/usr/bin/anvild\n" +
		"kvm\nqemu\ndocker\npodman\nnametaken\nmem=16303532\nstaging=104857600\nstate=52428800\ncache=20971520\n")
	if caps.arch != "aarch64" || caps.anvil != "/usr/bin/anvil" || caps.anvild != "/usr/bin/anvild" {
		t.Errorf("unexpected identity fields: %+v", caps)
	}
	if !caps.kvm || !caps.qemu || !caps.docker || !caps.podman || !caps.nameTaken {
		t.Errorf("unexpected flags: %+v", caps)
	}
	if caps.memTotalKB != 16303532 || caps.freeStagingKB != 104857600 || caps.freeStateKB != 52428800 || caps.freeCacheKB != 20971520 {
		t.Errorf("unexpected sizes: %+v", caps)
	}
	// uname -m naming is mapped onto the naming VM specs use.
	if got := parseRemoteCaps("arch=amd64\n").arch; got != "x86_64" {
		t.Errorf("arch amd64 parsed as %q, want x86_64", got)
	}
	// A df or awk that printed nothing leaves the reading unknown, not zero-free.
	if got := parseRemoteCaps("staging=\n").freeStagingKB; got != 0 {
		t.Errorf("empty free space parsed as %d, want 0", got)
	}
}

func TestShQuoteRoundTrip(t *testing.T) {
	in := "it's a /path with spaces"
	out, err := exec.Command("sh", "-c", "printf %s "+shQuote(in)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Errorf("got %q, want %q", out, in)
	}
}

func TestUploadDisk(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "plain"
		if compress {
			name = "zstd"
			if localZstd() == "" {
				t.Log("zstd not installed, skipping the compressed case")
				continue
			}
		}
		t.Run(name, func(t *testing.T) {
			localSSH(t, nil)
			dir := t.TempDir()
			src := filepath.Join(dir, "disk.qcow2")
			data := writeRandomFile(t, src, 1<<20)
			dst := filepath.Join(dir, "remote", "it's here.qcow2")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}

			caps := remoteCaps{zstd: compress, sha256sum: true}
			if err := uploadDisk(context.Background(), target{}, src, dst, caps, func(string) {}); err != nil {
				t.Fatalf("uploadDisk: %v", err)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("uploaded disk content differs")
			}
		})
	}
}

func TestUploadDiskResumesAfterBrokenConnection(t *testing.T) {
	var uploads atomic.Int32
	localSSH(t, func(remote string) string {
		if !strings.Contains(remote, "cat >") {
			return remote
		}
		if uploads.Add(1) == 1 {
			// The first upload dies after 3000 bytes.
			return "head -c 3000 | " + remote + "; exit 1"
		}
		return remote
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "disk.qcow2")
	data := writeRandomFile(t, src, 256<<10)
	dst := filepath.Join(dir, "remote.qcow2")

	var statuses []string
	caps := remoteCaps{sha256sum: true}
	if err := uploadDisk(context.Background(), target{}, src, dst, caps, func(s string) { statuses = append(statuses, s) }); err != nil {
		t.Fatalf("uploadDisk: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("resumed upload differs: got %d bytes, want %d", len(got), len(data))
	}
	if uploads.Load() != 2 {
		t.Errorf("expected 2 upload attempts, got %d", uploads.Load())
	}
	resumed := false
	for _, s := range statuses {
		if strings.Contains(s, "resuming") {
			resumed = true
		}
	}
	if !resumed {
		t.Errorf("expected a resume status, got %q", statuses)
	}
}

func TestUploadDiskDetectsCorruption(t *testing.T) {
	localSSH(t, func(remote string) string {
		if strings.Contains(remote, "sha256sum") {
			return "echo " + strings.Repeat("0", 64) + "  x"
		}
		return remote
	})
	dir := t.TempDir()
	src := filepath.Join(dir, "disk.qcow2")
	writeRandomFile(t, src, 4096)
	dst := filepath.Join(dir, "remote.qcow2")

	err := uploadDisk(context.Background(), target{}, src, dst, remoteCaps{sha256sum: true}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("expected a corruption error, got %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("expected the corrupt remote file to be removed")
	}
}

func TestCommonArgsHostKeyChecking(t *testing.T) {
	args := strings.Join(commonArgs(target{}, "-p"), " ")
	if !strings.Contains(args, "StrictHostKeyChecking=accept-new") {
		t.Errorf("expected trust on first use by default, got %q", args)
	}
	args = strings.Join(commonArgs(target{StrictHostKey: true}, "-p"), " ")
	if !strings.Contains(args, "StrictHostKeyChecking=yes") {
		t.Errorf("expected a strict host key check, got %q", args)
	}
}
