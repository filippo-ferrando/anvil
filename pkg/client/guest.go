package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
	"git.fferrando.cc/rdfilippo/anvil/internal/sshkey"
)

// SSHTarget is what's needed to reach a running VM over SSH.
type SSHTarget struct {
	InstanceID string
	Host       string // "localhost" for a standalone VM, or the guest's address for a bridged intent member
	Port       int
	User       string
	Jump       []string // ssh flags to hop through a remote daemon's host, nil when local
}

// SSHTarget builds a VM's connection info from an already-resolved instance.
// Host is relative to the daemon's host, so a remote daemon adds a jump through it.
func (c *Client) SSHTarget(inst *anvilv1.Instance, userOverride string) (SSHTarget, error) {
	name := inst.GetName()
	vmSpec := inst.GetVm()
	if vmSpec == nil {
		return SSHTarget{}, fmt.Errorf("%q isn't a VM", name)
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		state := strings.ToLower(strings.TrimPrefix(inst.GetState().String(), "STATE_"))
		return SSHTarget{}, fmt.Errorf("%q isn't running (state: %s)", name, state)
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
		ip, _, _ := strings.Cut(vmSpec.GetStaticIp(), "/")
		if ip == "" {
			return SSHTarget{}, fmt.Errorf("%q has no known bridge address yet", name)
		}
		return SSHTarget{InstanceID: inst.GetId(), Host: ip, Port: 22, User: user, Jump: c.JumpArgs()}, nil
	}

	if vmSpec.GetSshPort() == 0 {
		return SSHTarget{}, fmt.Errorf("%q has no known SSH port yet", name)
	}
	return SSHTarget{InstanceID: inst.GetId(), Host: "localhost", Port: int(vmSpec.GetSshPort()), User: user, Jump: c.JumpArgs()}, nil
}

// SSHKnownHostsPath returns a per-instance known_hosts file, keyed by instance ID
// rather than host:port so a reused ephemeral port doesn't trigger false host-key warnings.
func SSHKnownHostsPath(instanceID string) (string, error) {
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

// SSHArgs returns the ssh/scp flags (before the destination) to reach target.
// portFlag is "-p" for ssh and "-P" for scp.
func SSHArgs(target SSHTarget, portFlag, identity string) ([]string, error) {
	knownHosts, err := SSHKnownHostsPath(target.InstanceID)
	if err != nil {
		return nil, err
	}
	args := []string{
		portFlag, fmt.Sprintf("%d", target.Port),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR", // hides the "Permanently added" notice of accept-new
		"-o", "UserKnownHostsFile=" + knownHosts,
	}
	args = append(args, ttyCompatSSHArgs...)
	args = append(args, target.Jump...)
	if identity != "" {
		args = append(args, "-i", identity)
	}
	return args, nil
}

// runSSHWithStdin runs command on target with stdin from an in-memory reader,
// putting ssh's stderr into the returned error.
func runSSHWithStdin(target SSHTarget, identity string, command []string, stdin io.Reader) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH")
	}
	args, err := SSHArgs(target, "-p", identity)
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

// InjectGuestKeys authorizes the target host's guest-access key in every running VM
// about to be migrated. It returns one warning per stopped VM it had to skip.
func (c *Client) InjectGuestKeys(ctx context.Context, name, to string) (warnings []string, err error) {
	vms, err := c.MigratingVMs(ctx, name)
	if err != nil || len(vms) == 0 {
		return nil, err
	}

	reply, err := c.Migrate.GuestKey(ctx, &anvilv1.MigrateGuestKeyRequest{To: to})
	if err != nil {
		return nil, fmt.Errorf("fetching the target's guest-access key: %w", err)
	}
	key := reply.GetPublicKey()

	identity, err := sshkey.EnsureDefault()
	if err != nil {
		return nil, err
	}

	// Appends the key (piped via stdin) to authorized_keys unless it's already present.
	const remoteCommand = `mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && ` +
		`key=$(cat) && (grep -qxF "$key" ~/.ssh/authorized_keys || echo "$key" >> ~/.ssh/authorized_keys) && ` +
		`chmod 600 ~/.ssh/authorized_keys`

	for _, inst := range vms {
		if inst.GetState() != anvilv1.State_STATE_RUNNING {
			warnings = append(warnings, fmt.Sprintf("%s isn't running, skipping guest-key setup. Add the target's key to it manually if needed", inst.GetName()))
			continue
		}
		target, err := c.SSHTarget(inst, "")
		if err != nil {
			return warnings, fmt.Errorf("%s: %w", inst.GetName(), err)
		}
		if err := runSSHWithStdin(target, identity, []string{remoteCommand}, strings.NewReader(key)); err != nil {
			return warnings, fmt.Errorf("authorizing the target's key on %s: %w", inst.GetName(), err)
		}
	}
	return warnings, nil
}

// MigratingVMs looks up name as a single instance first, then as an intent,
// and returns every VM involved. Container members are skipped.
func (c *Client) MigratingVMs(ctx context.Context, name string) ([]*anvilv1.Instance, error) {
	infoReply, err := c.Info(ctx, &anvilv1.InfoRequest{Names: []string{name}})
	switch {
	case err == nil && len(infoReply.GetInstances()) > 0:
		inst := infoReply.GetInstances()[0]
		if inst.GetVm() == nil {
			return nil, nil
		}
		return []*anvilv1.Instance{inst}, nil
	case err != nil && status.Code(err) != codes.NotFound:
		return nil, fmt.Errorf("migrate: looking up %q: %w", name, err)
	}

	intentReply, err := c.Intent.Info(ctx, &anvilv1.IntentInfoRequest{Name: name})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("no instance or intent named %q", name)
		}
		return nil, fmt.Errorf("migrate: looking up %q as an intent: %w", name, err)
	}
	memberIDs := make(map[string]bool)
	for _, mem := range intentReply.GetIntent().GetMembers() {
		if mem.GetKind() == anvilv1.Kind_KIND_VM {
			memberIDs[mem.GetInstanceId()] = true
		}
	}
	if len(memberIDs) == 0 {
		return nil, nil
	}

	// List all instances and match by ID, since Info only resolves by name.
	listReply, err := c.List(ctx, &anvilv1.ListRequest{})
	if err != nil {
		return nil, err
	}
	var vms []*anvilv1.Instance
	for _, inst := range listReply.GetInstances() {
		if memberIDs[inst.GetId()] {
			vms = append(vms, inst)
		}
	}
	return vms, nil
}
