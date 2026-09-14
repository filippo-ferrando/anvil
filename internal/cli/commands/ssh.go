package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
	"github.com/anvil-project/anvil/pkg/client"
)

// resolveIdentity returns explicit if set, otherwise anvil's own managed key,
// generating it on first use.
func resolveIdentity(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return sshkey.EnsureDefault()
}

// resolveInstance looks up name via the daemon's Info RPC.
func resolveInstance(ctx context.Context, c *client.Client, name string) (*anvilv1.Instance, error) {
	reply, err := c.Info(ctx, &anvilv1.InfoRequest{Names: []string{name}})
	if err != nil {
		return nil, err
	}
	instances := reply.GetInstances()
	if len(instances) == 0 {
		return nil, fmt.Errorf("no such instance %q", name)
	}
	return instances[0], nil
}

// sshTarget is what's needed to reach a running VM over SSH.
type sshTarget struct {
	InstanceID string
	Host       string // "localhost" for a standalone VM, or the guest's address for a bridged intent member
	Port       int
	User       string
}

// resolveSSHTarget builds a VM's connection info from an already-resolved instance.
func resolveSSHTarget(inst *anvilv1.Instance, userOverride string) (sshTarget, error) {
	name := inst.GetName()
	vmSpec := inst.GetVm()
	if vmSpec == nil {
		return sshTarget{}, fmt.Errorf("%q isn't a VM", name)
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return sshTarget{}, fmt.Errorf("%q isn't running (state: %s)", name, stateLabel(inst.GetState()))
	}

	user := userOverride
	if user == "" {
		user = vmSpec.GetDefaultUser()
	}
	if user == "" {
		user = "root"
	}

	// A bridged intent member has its own address on the shared network.
	if vmSpec.GetNetworkMode() == "bridge" {
		if vmSpec.GetStaticIp() == "" {
			return sshTarget{}, fmt.Errorf("%q has no known bridge address yet", name)
		}
		ip, _, ok := strings.Cut(vmSpec.GetStaticIp(), "/")
		if !ok {
			ip = vmSpec.GetStaticIp()
		}
		return sshTarget{InstanceID: inst.GetId(), Host: ip, Port: 22, User: user}, nil
	}

	if vmSpec.GetSshPort() == 0 {
		return sshTarget{}, fmt.Errorf("%q has no known SSH port yet", name)
	}
	return sshTarget{
		InstanceID: inst.GetId(),
		Host:       "localhost",
		Port:       int(vmSpec.GetSshPort()),
		User:       user,
	}, nil
}

// sshKnownHostsPath returns a per-instance known_hosts file, keyed by instance ID
// rather than host:port so a reused ephemeral port doesn't trigger false host-key warnings.
func sshKnownHostsPath(instanceID string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("ssh: finding a cache directory: %w", err)
	}
	dir := filepath.Join(cacheDir, "anvil", "known_hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ssh: creating known_hosts dir: %w", err)
	}
	return filepath.Join(dir, instanceID), nil
}

// ttyCompatSSHArgs are extra -o flags that normalize terminal behavior (TERM,
// weak-crypto warnings) across the guest/remote session, tolerating older ssh clients.
var ttyCompatSSHArgs = []string{
	"-o", "SetEnv=TERM=xterm-256color",
	"-o", "IgnoreUnknown=WarnWeakCrypto",
	"-o", "WarnWeakCrypto=no-pq-kex",
}

func commonSSHArgs(target sshTarget, portFlag, identity string) ([]string, error) {
	knownHosts, err := sshKnownHostsPath(target.InstanceID)
	if err != nil {
		return nil, err
	}
	args := []string{
		portFlag, fmt.Sprintf("%d", target.Port),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
	}
	args = append(args, ttyCompatSSHArgs...)
	if identity != "" {
		args = append(args, "-i", identity)
	}
	return args, nil
}

// runSSH execs the real `ssh` binary against target, inheriting the current
// process's stdio. An empty command opens an interactive shell; a non-empty one runs it directly.
func runSSH(target sshTarget, identity string, command []string) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH (needed for `anvil shell`/`anvil exec`)")
	}
	args, err := commonSSHArgs(target, "-p", identity)
	if err != nil {
		return err
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	args = append(args, command...)

	cmd := exec.Command(sshBin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// runSSHWithStdin is runSSH for scripted use: stdin comes from an in-memory
// reader and stderr is captured into the returned error instead of the terminal.
func runSSHWithStdin(target sshTarget, identity string, command []string, stdin io.Reader) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH")
	}
	args, err := commonSSHArgs(target, "-p", identity)
	if err != nil {
		return err
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	args = append(args, command...)

	var stderr bytes.Buffer
	cmd := exec.Command(sshBin, args...)
	cmd.Stdin = stdin
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}
