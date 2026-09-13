package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
	"github.com/anvil-project/anvil/internal/sshkey"
)

// shellIntoVM is the M8 checklist's "shell/SSH handoff": the TUI's own
// direct equivalent of `anvil shell`, called only from inside
// app.tapp.Suspend (see instancesView.shellInto) so the real terminal is
// free for a genuine interactive session — tview's own suspend/resume
// support exists for exactly this, the same technique tools like
// k9s/lazygit use for "shell into pod"/"open editor." Deliberately not a
// from-scratch reimplementation of internal/cli/commands/ssh.go: this
// package only reuses pkg/client, per the plan's TUI section, so this is
// its own (small, intentionally duplicated) copy of the same connection
// logic, not a shared one.
func shellIntoVM(inst *anvilv1.Instance) error {
	vm := inst.GetVm()
	if vm == nil {
		return fmt.Errorf("%q isn't a VM", inst.GetName())
	}
	if inst.GetState() != anvilv1.State_STATE_RUNNING {
		return fmt.Errorf("%q isn't running", inst.GetName())
	}

	user := vm.GetDefaultUser()
	if user == "" {
		user = "root"
	}

	host, port := "localhost", int(vm.GetSshPort())
	if vm.GetNetworkMode() == "bridge" {
		ip, _, _ := strings.Cut(vm.GetStaticIp(), "/")
		if ip == "" {
			return fmt.Errorf("%q has no known bridge address yet", inst.GetName())
		}
		host, port = ip, 22
	} else if port == 0 {
		return fmt.Errorf("%q has no known SSH port yet", inst.GetName())
	}

	identity, err := sshkey.EnsureDefault()
	if err != nil {
		return err
	}

	knownHosts, err := knownHostsPath(inst.GetId())
	if err != nil {
		return err
	}

	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh: not found on PATH")
	}
	cmd := exec.Command(sshBin,
		"-p", fmt.Sprintf("%d", port),
		"-i", identity,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile="+knownHosts,
		fmt.Sprintf("%s@%s", user, host),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// knownHostsPath mirrors internal/cli/commands/ssh.go's sshKnownHostsPath:
// keyed by instance ID rather than "host:port", since a SLIRP
// host-forwarded port is ephemeral and gets reused across unrelated
// instances.
func knownHostsPath(instanceID string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("finding a cache directory: %w", err)
	}
	dir := filepath.Join(cacheDir, "anvil", "known_hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating known_hosts dir: %w", err)
	}
	return filepath.Join(dir, instanceID), nil
}
