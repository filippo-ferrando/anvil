package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
	"github.com/anvil-project/anvil/pkg/client"
)

// resolveMigratingVMs resolves name to the VM instances that would be migrated,
// whether it names a single instance or an intent group.
func resolveMigratingVMs(ctx context.Context, c *client.Client, name string) ([]*anvilv1.Instance, error) {
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

	// List everything and match by ID; there's no lookup-by-ID RPC.
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

// injectGuestKeys authorizes the destination host's guest-access key in
// every running VM about to be migrated, before anything is stopped or transferred.
func injectGuestKeys(ctx context.Context, c *client.Client, name, to string) error {
	vms, err := resolveMigratingVMs(ctx, c, name)
	if err != nil {
		return err
	}
	if len(vms) == 0 {
		return nil
	}

	reply, err := c.Migrate.GuestKey(ctx, &anvilv1.MigrateGuestKeyRequest{To: to})
	if err != nil {
		return fmt.Errorf("fetching the target's guest-access key: %w", err)
	}
	key := reply.GetPublicKey()

	identity, err := sshkey.EnsureDefault()
	if err != nil {
		return err
	}

	// The key travels over stdin and is appended to authorized_keys only if not already present.
	const remoteCommand = `mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && ` +
		`key=$(cat) && (grep -qxF "$key" ~/.ssh/authorized_keys || echo "$key" >> ~/.ssh/authorized_keys) && ` +
		`chmod 600 ~/.ssh/authorized_keys`

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH")
	}

	for _, inst := range vms {
		if inst.GetState() != anvilv1.State_STATE_RUNNING {
			// Nothing to SSH into; skip stopped VMs.
			continue
		}
		vm := inst.GetVm()
		host, port := "localhost", int(vm.GetSshPort())
		if vm.GetNetworkMode() == "bridge" {
			ip, _, _ := strings.Cut(vm.GetStaticIp(), "/")
			if ip == "" {
				return fmt.Errorf("%s has no known bridge address yet", inst.GetName())
			}
			host, port = ip, 22
		} else if port == 0 {
			return fmt.Errorf("%s has no known SSH port yet", inst.GetName())
		}
		user := vm.GetDefaultUser()
		if user == "" {
			user = "root"
		}
		knownHosts, err := knownHostsPath(inst.GetId())
		if err != nil {
			return err
		}

		args := []string{
			"-p", fmt.Sprintf("%d", port),
			"-i", identity,
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "UserKnownHostsFile=" + knownHosts,
			fmt.Sprintf("%s@%s", user, host),
			remoteCommand,
		}
		cmd := exec.Command(sshBin, args...)
		cmd.Stdin = strings.NewReader(key)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("authorizing the target's key on %s: %w (%s)", inst.GetName(), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
