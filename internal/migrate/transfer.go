package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anvil-project/anvil/internal/instance"
)

// uploadAttempts is how many times one disk upload resumes after a broken connection.
const uploadAttempts = 4

// remoteStagingDir is where the target keeps an uploaded disk until anvild adopts it.
// /var/tmp is disk-backed on most distros, unlike /tmp, which is often a small tmpfs.
const remoteStagingDir = "/var/tmp"

// remoteStateDir/remoteCacheDir are where the target's anvild keeps instance disks
// and cached base images, mirroring config.StateDir/CacheDir on Linux.
const (
	remoteStateDir = "/var/lib/anvil"
	remoteCacheDir = "/var/cache/anvil"
)

// remoteCaps is what the target host has, as seen in one probe.
type remoteCaps struct {
	zstd      bool
	sha256sum bool
	baseSHA   string // target's SHA256 of the VM's base image; "" if not cached or unknown

	anvil  string // path to the target's anvil binary; "" if not installed
	anvild string
	arch   string // target's CPU arch, in VM Arch naming ("x86_64", "aarch64")
	kvm    bool   // /dev/kvm exists
	qemu   bool   // qemu-system-<the migrating VM's arch> is installed
	docker bool
	podman bool
	tar    bool

	nameTaken     bool  // the destination name is already used on the target
	memTotalKB    int64 // 0 when unknown
	freeStagingKB int64
	freeStateKB   int64
	freeCacheKB   int64
}

// imageChecksumCmd asks the target's own anvil for its cached checksum of
// imageRef, printing nothing when it has no copy of it.
func imageChecksumCmd(imageRef, arch string) string {
	return "anvil image checksum " + shQuote(imageRef) + " --arch " + shQuote(arch) + " 2>/dev/null"
}

// remoteBaseSHA re-reads that checksum on its own, for a target that may have
// cached the image since the probe ran.
func remoteBaseSHA(ctx context.Context, t target, imageRef, arch string) string {
	if imageRef == "" {
		return ""
	}
	if arch == "" {
		arch = "x86_64"
	}
	out, _ := sshRun(ctx, t, remoteSh(imageChecksumCmd(imageRef, arch)), nil)
	if sum := strings.TrimSpace(out); isSHA256Hex(sum) {
		return sum
	}
	return ""
}

// probeRemote asks t, in one SSH round trip, everything preflight and the disk
// upload need to know. Any failure just leaves the matching field unset.
func probeRemote(ctx context.Context, t target, spec *instance.Spec, destName string) remoteCaps {
	var b strings.Builder
	b.WriteString("echo arch=$(uname -m); " +
		"echo anvil=$(command -v anvil); echo anvild=$(command -v anvild); " +
		"command -v zstd >/dev/null 2>&1 && echo zstd; " +
		"command -v sha256sum >/dev/null 2>&1 && echo sha256sum; " +
		"command -v docker >/dev/null 2>&1 && echo docker; " +
		"command -v podman >/dev/null 2>&1 && echo podman; " +
		"command -v tar >/dev/null 2>&1 && echo tar; " +
		"[ -e /dev/kvm ] && echo kvm; " +
		"echo mem=$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null); " +
		"echo staging=$(df -Pk " + remoteStagingDir + " 2>/dev/null | awk 'NR==2{print $4}'); " +
		"echo state=$(df -Pk " + remoteStateDir + " 2>/dev/null | awk 'NR==2{print $4}'); " +
		"echo cache=$(df -Pk " + remoteCacheDir + " 2>/dev/null | awk 'NR==2{print $4}'); ")
	if destName != "" {
		b.WriteString("anvil info " + shQuote(destName) + " >/dev/null 2>&1 && echo nametaken; ")
	}
	if spec != nil && spec.VM != nil {
		arch := spec.VM.Arch
		if arch == "" {
			arch = "x86_64"
		}
		b.WriteString("command -v " + qemuBinary(arch) + " >/dev/null 2>&1 && echo qemu; ")
		if spec.VM.ImageRef != "" {
			b.WriteString("echo base=$(" + imageChecksumCmd(spec.VM.ImageRef, arch) + "); ")
		}
	}
	// Each line reports itself, so a failing step must not fail the probe.
	b.WriteString("exit 0")

	out, _ := sshRun(ctx, t, remoteSh(b.String()), nil)
	return parseRemoteCaps(out)
}

func parseRemoteCaps(out string) remoteCaps {
	var caps remoteCaps
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "zstd":
			caps.zstd = true
		case "sha256sum":
			caps.sha256sum = true
		case "kvm":
			caps.kvm = true
		case "qemu":
			caps.qemu = true
		case "docker":
			caps.docker = true
		case "podman":
			caps.podman = true
		case "tar":
			caps.tar = true
		case "nametaken":
			caps.nameTaken = true
		case "anvil":
			caps.anvil = value
		case "anvild":
			caps.anvild = value
		case "arch":
			caps.arch = normalizeArch(value)
		case "mem":
			caps.memTotalKB = parseKB(value)
		case "staging":
			caps.freeStagingKB = parseKB(value)
		case "state":
			caps.freeStateKB = parseKB(value)
		case "cache":
			caps.freeCacheKB = parseKB(value)
		case "base":
			if isSHA256Hex(value) {
				caps.baseSHA = value
			}
		}
	}
	return caps
}

func parseKB(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// normalizeArch maps what uname -m prints to the naming VM specs use.
func normalizeArch(s string) string {
	switch s := strings.TrimSpace(s); s {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return s
	}
}

// hostArch is this host's CPU arch in the same naming.
func hostArch() string { return normalizeArch(runtime.GOARCH) }

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// uploadDisk copies localPath to t's remotePath over ssh, compressed with zstd when
// both sides have it, resuming from the remote file's size after a broken connection.
func uploadDisk(ctx context.Context, t target, localPath, remotePath string, caps remoteCaps, progress func(status string)) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("migrate: reading %s: %w", localPath, err)
	}
	size := info.Size()
	compress := caps.zstd && localZstd() != ""

	note := humanBytes(size)
	if compress {
		note += ", zstd"
	}
	progress(fmt.Sprintf("uploading disk to target (%s)", note))

	var offset int64
	for attempt := 1; ; attempt++ {
		err = sendDisk(ctx, t, localPath, remotePath, offset, size, compress, progress)
		if err == nil {
			break
		}
		if ctx.Err() != nil || attempt == uploadAttempts {
			_, _ = sshRun(context.WithoutCancel(ctx), t, remoteSh("rm -f "+shQuote(remotePath)), nil)
			return err
		}
		progress(fmt.Sprintf("upload interrupted, resuming (%v)", err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
		offset, err = remoteSize(ctx, t, remotePath)
		if err != nil || offset > size {
			offset = 0
		}
	}

	if !caps.sha256sum {
		return nil
	}
	progress("verifying uploaded disk")
	want, err := fileSHA256(localPath)
	if err != nil {
		return fmt.Errorf("migrate: hashing %s: %w", localPath, err)
	}
	out, err := sshRun(ctx, t, remoteSh("sha256sum "+shQuote(remotePath)), nil)
	if err != nil {
		return fmt.Errorf("migrate: verifying uploaded disk: %w", err)
	}
	if got, _, _ := strings.Cut(strings.TrimSpace(out), " "); got != want {
		_, _ = sshRun(ctx, t, remoteSh("rm -f "+shQuote(remotePath)), nil)
		return fmt.Errorf("migrate: uploaded disk is corrupt (sha256 %s, want %s)", got, want)
	}
	return nil
}

// sendDisk streams localPath from offset to remotePath. offset 0 truncates the
// remote file, anything else appends to it.
func sendDisk(ctx context.Context, t target, localPath, remotePath string, offset, size int64, compress bool, progress func(status string)) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("migrate: opening %s: %w", localPath, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("migrate: seeking %s: %w", localPath, err)
	}

	redirect := " > "
	if offset > 0 {
		redirect = " >> "
	}
	remote := "cat" + redirect + shQuote(remotePath)
	if compress {
		remote = "zstd -q -d -c" + redirect + shQuote(remotePath)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ssh, err := sshCommand(ctx, t, remoteSh(remote))
	if err != nil {
		return err
	}
	var stderr strings.Builder
	ssh.Stderr = &stderr

	counter := &countingReader{r: f}
	counter.n.Store(offset)
	stopProgress := reportUpload(counter, size, progress)
	defer stopProgress()

	var zstd *exec.Cmd
	if compress {
		zstd = exec.CommandContext(ctx, localZstd(), "-q", "-c", "-T0")
		zstd.Stdin = counter
		pipe, err := zstd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("migrate: starting zstd: %w", err)
		}
		ssh.Stdin = pipe
		if err := zstd.Start(); err != nil {
			return fmt.Errorf("migrate: starting zstd: %w", err)
		}
	} else {
		ssh.Stdin = counter
	}

	sshErr := ssh.Run()
	if sshErr != nil {
		cancel() // stops zstd if ssh died first
	}
	var zstdErr error
	if zstd != nil {
		zstdErr = zstd.Wait()
	}
	if sshErr != nil {
		return fmt.Errorf("migrate: uploading to %s@%s: %w: %s", t.User, t.Host, sshErr, strings.TrimSpace(stderr.String()))
	}
	if zstdErr != nil {
		return fmt.Errorf("migrate: compressing disk: %w", zstdErr)
	}
	if got := counter.n.Load(); got != size {
		return fmt.Errorf("migrate: sent %d of %d bytes", got, size)
	}
	return nil
}

// remoteSize returns the size of t's remotePath, 0 if it doesn't exist.
func remoteSize(ctx context.Context, t target, remotePath string) (int64, error) {
	out, err := sshRun(ctx, t, remoteSh("stat -c %s "+shQuote(remotePath)+" 2>/dev/null || echo 0"), nil)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrate: unexpected remote size %q", strings.TrimSpace(out))
	}
	return n, nil
}

// reportUpload sends a progress line every 5 seconds until the returned stop func runs.
func reportUpload(c *countingReader, size int64, progress func(status string)) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sent := c.n.Load()
				pct := 100.0
				if size > 0 {
					pct = float64(sent) / float64(size) * 100
				}
				progress(fmt.Sprintf("uploading disk to target: %.0f%% (%s / %s)", pct, humanBytes(sent), humanBytes(size)))
			}
		}
	}()
	return func() { close(done) }
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func localZstd() string {
	path, err := exec.LookPath("zstd")
	if err != nil {
		return ""
	}
	return path
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uploadDir streams localDir's contents to remoteDir on t as a gzipped tar, so a
// shared folder's data travels with the instance that mounts it.
func uploadDir(ctx context.Context, t target, localDir, remoteDir string, progress func(status string)) error {
	tarBin, err := exec.LookPath("tar")
	if err != nil {
		return fmt.Errorf("migrate: tar not found on PATH (needed to send %s)", localDir)
	}
	progress(fmt.Sprintf("sending %s (%s)", localDir, humanBytes(dirSize(localDir))))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	remote := "rm -rf " + shQuote(remoteDir) + " && mkdir -p " + shQuote(remoteDir) +
		" && tar -xzf - -C " + shQuote(remoteDir)
	ssh, err := sshCommand(ctx, t, remoteSh(remote))
	if err != nil {
		return err
	}
	var sshErrOut strings.Builder
	ssh.Stderr = &sshErrOut

	local := exec.CommandContext(ctx, tarBin, "-czf", "-", "-C", localDir, ".")
	var tarErrOut strings.Builder
	local.Stderr = &tarErrOut
	pipe, err := local.StdoutPipe()
	if err != nil {
		return fmt.Errorf("migrate: archiving %s: %w", localDir, err)
	}
	ssh.Stdin = pipe
	if err := local.Start(); err != nil {
		return fmt.Errorf("migrate: archiving %s: %w", localDir, err)
	}

	sshErr := ssh.Run()
	if sshErr != nil {
		cancel() // stops tar if ssh died first
	}
	tarErr := local.Wait()
	if sshErr != nil {
		return fmt.Errorf("migrate: sending %s to %s@%s: %w: %s", localDir, t.User, t.Host, sshErr, strings.TrimSpace(sshErrOut.String()))
	}
	if tarErr != nil {
		return fmt.Errorf("migrate: archiving %s: %w: %s", localDir, tarErr, strings.TrimSpace(tarErrOut.String()))
	}
	return nil
}

// removeRemote deletes staged paths left on t by a migration that failed.
func removeRemote(ctx context.Context, t target, paths []string) {
	if len(paths) == 0 {
		return
	}
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = shQuote(p)
	}
	_, _ = sshRun(context.WithoutCancel(ctx), t, remoteSh("rm -rf "+strings.Join(quoted, " ")), nil)
}

// dirSize is how much data a directory holds, 0 if it can't be read.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
