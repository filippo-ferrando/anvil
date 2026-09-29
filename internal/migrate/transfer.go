package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// uploadAttempts is how many times one disk upload resumes after a broken connection.
const uploadAttempts = 4

// remoteStagingDir is where the target keeps an uploaded disk until anvild adopts it.
// /var/tmp is disk-backed on most distros, unlike /tmp, which is often a small tmpfs.
const remoteStagingDir = "/var/tmp"

// remoteCaps is what the target offers for a VM disk upload.
type remoteCaps struct {
	zstd      bool
	sha256sum bool
	baseSHA   string // target's SHA256 of the VM's base image; "" if not cached or unknown
}

// probeRemote asks t, in one SSH round trip, which tools it has and whether it
// caches the base image imageRef/arch. Any failure just means "no extras".
func probeRemote(ctx context.Context, t target, imageRef, arch string) remoteCaps {
	script := "command -v zstd >/dev/null 2>&1 && echo zstd; " +
		"command -v sha256sum >/dev/null 2>&1 && echo sha256sum; "
	if imageRef != "" {
		script += "echo base=$(anvil image checksum " + shQuote(imageRef)
		if arch != "" {
			script += " --arch " + shQuote(arch)
		}
		script += " 2>/dev/null)"
	}
	out, _ := sshRun(ctx, t, remoteSh(script), nil)
	return parseRemoteCaps(out)
}

func parseRemoteCaps(out string) remoteCaps {
	var caps remoteCaps
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "zstd":
			caps.zstd = true
		case line == "sha256sum":
			caps.sha256sum = true
		case strings.HasPrefix(line, "base="):
			if sum := strings.TrimPrefix(line, "base="); isSHA256Hex(sum) {
				caps.baseSHA = sum
			}
		}
	}
	return caps
}

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
