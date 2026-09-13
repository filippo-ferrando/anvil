package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/pkg/client"
)

// defaultAnvilKeyPath is anvil's own managed SSH keypair (private key path;
// the public half is the same path + ".pub"). This is deliberately separate
// from any personal ~/.ssh key the user has for other purposes — the same
// idea behind Vagrant's well-known shared "insecure" keypair and
// Multipass's own managed key: this key isn't protecting anything beyond
// "don't let an unrelated local process into the VM" (SLIRP host-forwarding
// is only ever reachable from this same host to begin with), so a
// passphrase would only add friction with no real security benefit, and
// tying VM access to a user's actual personal identity key is more coupling
// than this needs.
func defaultAnvilKeyPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding a config directory: %w", err)
	}
	return filepath.Join(configDir, "anvil", "ssh", "id_ed25519"), nil
}

// ensureDefaultAnvilKey returns the private key path, generating a fresh
// passphrase-less ed25519 keypair there via `ssh-keygen` (already a
// dependency alongside `ssh`/`scp`) the first time it's needed.
func ensureDefaultAnvilKey() (string, error) {
	path, err := defaultAnvilKeyPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking for anvil's SSH key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("creating anvil's SSH key dir: %w", err)
	}
	keygenBin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return "", fmt.Errorf("ssh-keygen: not found on PATH (needed to generate anvil's default SSH key)")
	}
	cmd := exec.Command(keygenBin, "-t", "ed25519", "-N", "", "-C", "anvil", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("generating anvil's SSH key: %w: %s", err, out)
	}
	return path, nil
}

// resolveIdentity returns explicit if the user passed --identity/-i,
// otherwise anvil's own managed key (generating it on first use) — so
// `shell`/`exec`/`transfer` work by default without depending on ssh's own
// $HOME-based identity lookup (which is what broke under sudo before this
// existed: root's $HOME, not the invoking user's).
func resolveIdentity(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	return ensureDefaultAnvilKey()
}

// resolveInstance looks up name via the daemon's Info RPC — shared by
// shell/exec/transfer to first figure out whether they're dealing with a
// VM (SSH, see resolveSSHTarget/runSSH below) or a container (docker/podman
// exec, see runContainerExec in container_exec.go).
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
	Host       string // "localhost" for a standalone (SLIRP) VM, or the guest's own address for an intent member on a bridged network
	Port       int
	User       string
}

// resolveSSHTarget builds a VM's connection info from an already-resolved
// instance (see resolveInstance) — populated by internal/vm.Backend at
// Create/Start time (VMSpec.ssh_port/default_user for a standalone VM,
// VMSpec.static_ip for a bridged intent member, see internal/vm.Backend.Start).
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

	// A bridged intent member has its own address on the shared network
	// instead of a SLIRP host-forwarded port — see
	// internal/vm.Backend.Start and bridgeNetworkConfig.
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

// sshKnownHostsPath returns a per-instance known_hosts file. Deliberately
// not the daemon's state dir (the CLI isn't necessarily running as
// whatever user/permissions anvild does) and deliberately not the invoking
// user's own ~/.ssh/known_hosts either: a SLIRP host-forwarded port is
// ephemeral and gets reused across unrelated instances, so tracking host
// identity by "localhost:port" the way ssh normally would produces
// confusing "REMOTE HOST IDENTIFICATION HAS CHANGED" warnings any time a
// different instance happens to get the same port a previous one had.
// Keying by instance ID instead sidesteps that: the *same* instance keeps
// the same disk (and so the same real SSH host key) across a stop/start
// cycle, even though its port changes every time.
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

// commonSSHArgs returns the -o/-p flags shared by both `ssh` and `scp`
// invocations against target. identity, if non-empty, is passed as -i: by
// default ssh picks its own identity file from $HOME/.ssh, which is wrong
// under `sudo anvil shell` (root's $HOME, not the invoking user's, so the
// private key ssh tries doesn't match whatever public key actually got
// authorized by --ssh-key) — passing an explicit path sidesteps that
// instead of relying on ssh's own default-identity guessing.
func commonSSHArgs(target sshTarget, portFlag, identity string) ([]string, error) {
	knownHosts, err := sshKnownHostsPath(target.InstanceID)
	if err != nil {
		return nil, err
	}
	args := []string{
		portFlag, fmt.Sprintf("%d", target.Port),
		// accept-new, not "no": a genuine host-key change within one
		// instance's own lifetime (same disk across stop/start) is still
		// worth flagging, this just avoids false positives from unrelated
		// instances reusing the same ephemeral port — see the doc comment
		// on sshKnownHostsPath above.
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + knownHosts,
	}
	if identity != "" {
		args = append(args, "-i", identity)
	}
	return args, nil
}

// runSSH execs the real `ssh` binary against target, inheriting the
// current process's stdio. An empty command opens an interactive shell
// (ssh's own default when given no remote command); a non-empty one runs
// it non-interactively, same as plain `ssh host cmd`. identity is an
// optional -i path, see commonSSHArgs.
//
// Shelling out to the real ssh binary — rather than a Go SSH library — is
// deliberate: it gets real TTY handling, ssh-agent forwarding, and
// known_hosts semantics for free, which matters a lot for an interactive
// session in a way it doesn't for the daemon's own non-interactive
// migration use of a Go SSH client (see the plan's "Cold migration"
// section).
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
