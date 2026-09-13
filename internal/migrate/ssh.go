package migrate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/anvil-project/anvil/internal/config"
)

// target is a parsed "user@host[:port]" SSH destination, plus an optional
// identity file — see store.Host and MigrateRequest.to.
type target struct {
	User     string
	Host     string
	Port     int // 0 means ssh's own default (22)
	Identity string
}

// parseTarget parses "user@host[:port]" into its parts. user is required
// (there's no sensible default to assume for a daemon-driven SSH session,
// unlike an interactive terminal that could fall back to $USER).
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
	return target{User: user, Host: host, Port: port}, nil
}

// commonArgs returns the -p/-i/host-key-checking flags shared by ssh and
// scp invocations against t. identity defaults to
// config.MigrateIdentityPath() (anvild's own generated migration key,
// see that function's doc comment) when t didn't specify one — always
// passed explicitly, never left to ssh's own default identity
// resolution, for the same reason the CLI side of this project already
// stopped relying on that once (see internal/cli/commands/ssh.go).
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
		"-o", "BatchMode=yes", // never prompt — a stuck daemon-side password prompt would hang the whole migration with no way to answer it
	)
	return args
}

// scpUpload copies localPath to t's remotePath.
func scpUpload(ctx context.Context, t target, localPath, remotePath string) error {
	scpBin, err := exec.LookPath("scp")
	if err != nil {
		return fmt.Errorf("migrate: scp not found on PATH")
	}
	args := commonArgs(t, "-P")
	args = append(args, localPath, fmt.Sprintf("%s@%s:%s", t.User, t.Host, remotePath))

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, scpBin, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("migrate: scp to %s@%s: %w: %s", t.User, t.Host, err, stderr.String())
	}
	return nil
}

// sshRun runs remoteCommand on t, piping stdin to it and returning
// everything it wrote to stdout. Used to invoke `anvil migrate-import` on
// the target (see Manager.Migrate) — remoteCommand is always a fixed,
// argument-free string in practice (no untrusted content ever becomes
// part of the command line itself; the actual migration payload travels
// over stdin instead, precisely to avoid needing to shell-quote it).
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
