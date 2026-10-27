package client

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// Transfer is a copy between a local path and a path inside an instance.
type Transfer struct {
	Instance     string // the instance's name
	InstancePath string
	LocalPath    string
	ToInstance   bool // false copies from the instance to the local path
}

// SplitInstanceRef splits "<name>:<path>" into its parts. A plain local
// path has no colon and ok comes back false.
func SplitInstanceRef(s string) (name, path string, ok bool) {
	name, path, ok = strings.Cut(s, ":")
	if !ok {
		return "", s, false
	}
	return name, path, true
}

// ParseTransfer reads `anvil transfer <source> <destination>`, where exactly one side is <name>:<path>.
func ParseTransfer(src, dst string) (Transfer, error) {
	srcName, srcPath, srcIn := SplitInstanceRef(src)
	dstName, dstPath, dstIn := SplitInstanceRef(dst)
	switch {
	case srcIn == dstIn:
		return Transfer{}, errors.New("exactly one of <source>/<destination> must be an instance reference (<name>:<path>); " +
			"transferring between two instances, or between two local paths, isn't supported")
	case dstIn:
		return Transfer{Instance: dstName, InstancePath: dstPath, LocalPath: srcPath, ToInstance: true}, nil
	default:
		return Transfer{Instance: srcName, InstancePath: srcPath, LocalPath: dstPath}, nil
	}
}

// TransferCommand builds the copy for t with the current stdio: `scp -r` for a VM, where
// user and identity override the defaults, and docker/podman cp for a container.
func (c *Client) TransferCommand(inst *anvilv1.Instance, t Transfer, user, identity string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	var err error
	if inst.GetContainer() != nil {
		cmd, err = c.containerCopyCmd(inst, t)
	} else {
		cmd, err = c.scpCmd(inst, t, user, identity)
	}
	if err != nil {
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd, nil
}

func (c *Client) scpCmd(inst *anvilv1.Instance, t Transfer, user, identity string) (*exec.Cmd, error) {
	key, err := ResolveIdentity(identity)
	if err != nil {
		return nil, err
	}
	target, err := c.SSHTarget(inst, user)
	if err != nil {
		return nil, err
	}
	scpBin, err := exec.LookPath("scp")
	if err != nil {
		return nil, fmt.Errorf("scp: not found on PATH (needed for `anvil transfer`)")
	}
	args, err := SSHArgs(target, "-P", key)
	if err != nil {
		return nil, err
	}
	args = append(args, "-r") // harmless for a single file, required for a directory
	remote := fmt.Sprintf("%s@%s:%s", target.User, target.Host, t.InstancePath)
	if t.ToInstance {
		args = append(args, t.LocalPath, remote)
	} else {
		args = append(args, remote, t.LocalPath)
	}
	return exec.Command(scpBin, args...), nil
}

func (c *Client) containerCopyCmd(inst *anvilv1.Instance, t Transfer) (*exec.Cmd, error) {
	spec := inst.GetContainer()
	if spec.GetContainerId() == "" {
		return nil, fmt.Errorf("%q has no known container ID yet", inst.GetName())
	}
	bin, err := ContainerBin(spec.GetEngine())
	if err != nil {
		return nil, err
	}
	remote := spec.GetContainerId() + ":" + t.InstancePath
	args := []string{"cp", remote, t.LocalPath}
	if t.ToInstance {
		args = []string{"cp", t.LocalPath, remote}
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = c.DockerEnv()
	return cmd, nil
}
