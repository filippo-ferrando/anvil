package migrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/anvil-project/anvil/internal/config"
)

// target is a parsed "user@host[:port]" SSH destination, plus an optional
// identity file.
type target struct {
	User     string
	Host     string
	Port     int // 0 means ssh's own default (22)
	Identity string
}

// parseTarget parses "user@host[:port]" into its parts. user is required.
func parseTarget(s string) (target, error) {
	user, rest, ok := strings.Cut(s, "@")
	if !ok || user == "" {
		return target{}, fmt.Errorf("migrate: target %q must be \"user@host[:port]\"", s)
	}
	host := rest
	port := 0
	if h, p, ok := strings.Cut(rest, ":"); ok {
		host = h
		n, err := strconv.Atoi(p)
		if err != nil {
			return target{}, fmt.Errorf("migrate: target %q has an invalid port: %w", s, err)
		}
		port = n
	}
	if host == "" {
		return target{}, fmt.Errorf("migrate: target %q must be \"user@host[:port]\"", s)
	}
	// Reject a user/host starting with "-": ssh/scp would otherwise parse
	// it as an option rather than a destination.
	if strings.HasPrefix(user, "-") || strings.HasPrefix(host, "-") {
		return target{}, fmt.Errorf("migrate: target %q: user/host must not start with \"-\"", s)
	}
	return target{User: user, Host: host, Port: port}, nil
}

// commonArgs returns the -p/-i/host-key-checking flags shared by ssh and
// scp invocations against t.
func commonArgs(t target, portFlag string) []string {
	var args []string
	if t.Port != 0 {
		args = append(args, portFlag, strconv.Itoa(t.Port))
	}
	identity := t.Identity
	if identity == "" {
		identity = config.MigrateIdentityPath()
	}
	args = append(args, "-i", identity)
	args = append(args,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+config.MigrateKnownHostsPath(),
		"-o", "BatchMode=yes", // never prompt for credentials
	)
	return args
}

// scpUpload copies localPath to t's remotePath, sending progress a
// heartbeat status line every 5 seconds while the transfer runs.
func scpUpload(ctx context.Context, t target, localPath, remotePath string, progress func(status string)) error {
	scpBin, err := exec.LookPath("scp")
	if err != nil {
		return fmt.Errorf("migrate: scp not found on PATH")
	}
	var sizeNote string
	if info, err := os.Stat(localPath); err == nil {
		sizeNote = fmt.Sprintf(" (%s)", humanBytes(info.Size()))
	}

	if progress != nil {
		progress("uploading disk to target" + sizeNote)
	}

	args := commonArgs(t, "-P")
	args = append(args, localPath, fmt.Sprintf("%s@%s:%s", t.User, t.Host, remotePath))

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, scpBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("migrate: scp to %s@%s: %w", t.User, t.Host, err)
	}

	done := make(chan struct{})
	if progress != nil {
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			start := time.Now()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					progress(fmt.Sprintf("uploading disk to target%s: still running after %s",
						sizeNote, time.Since(start).Round(time.Second)))
				}
			}
		}()
	}
	err = cmd.Wait()
	close(done)
	if err != nil {
		return fmt.Errorf("migrate: scp to %s@%s: %w: %s", t.User, t.Host, err, stderr.String())
	}
	return nil
}

// humanBytes renders n as a short, human-readable size (KiB/MiB/...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// sshRun runs remoteCommand on t, piping stdin to it and returning
// everything it wrote to stdout.
func sshRun(ctx context.Context, t target, remoteCommand string, stdin io.Reader) (string, error) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("migrate: ssh not found on PATH")
	}
	args := commonArgs(t, "-p")
	args = append(args, fmt.Sprintf("%s@%s", t.User, t.Host), remoteCommand)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, sshBin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("migrate: ssh to %s@%s: %w: %s", t.User, t.Host, err, stderr.String())
	}
	return stdout.String(), nil
}
