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

// sshCommand builds the ssh invocation running remoteCommand on t. A variable
// so tests can run the remote side locally instead.
var sshCommand = func(ctx context.Context, t target, remoteCommand string) (*exec.Cmd, error) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return nil, fmt.Errorf("migrate: ssh not found on PATH")
	}
	args := commonArgs(t, "-p")
	args = append(args, fmt.Sprintf("%s@%s", t.User, t.Host), remoteCommand)
	return exec.CommandContext(ctx, sshBin, args...), nil
}

// sshRun runs remoteCommand on t, piping stdin to it and returning
// everything it wrote to stdout.
func sshRun(ctx context.Context, t target, remoteCommand string, stdin io.Reader) (string, error) {
	cmd, err := sshCommand(ctx, t, remoteCommand)
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("migrate: ssh to %s@%s: %w: %s", t.User, t.Host, err, stderr.String())
	}
	return stdout.String(), nil
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// remoteSh wraps script so it runs under sh, whatever the remote user's login shell is.
func remoteSh(script string) string {
	return "sh -c " + shQuote(script)
}
