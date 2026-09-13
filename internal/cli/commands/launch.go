package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newLaunchCommand(flags *globalFlags) *cobra.Command {
	var (
		kindStr       string
		name          string
		cpus          int32
		memoryMiB     int64
		diskGiB       int64
		cloudInitFile string
		cloudInitName string
		sshKeys       []string
		noStart       bool
	)

	cmd := &cobra.Command{
		Use:   "launch <image>",
		Short: "Launch a new VM or container instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			imageRef := args[0]

			var kind anvilv1.Kind
			switch kindStr {
			case "vm":
				kind = anvilv1.Kind_KIND_VM
			case "container":
				kind = anvilv1.Kind_KIND_CONTAINER
			default:
				return fmt.Errorf("--kind must be \"vm\" or \"container\"")
			}
			if kind == anvilv1.Kind_KIND_CONTAINER {
				return fmt.Errorf("container instances aren't implemented yet (planned for milestone 3)")
			}

			if cloudInitFile != "" && cloudInitName != "" {
				return fmt.Errorf("--cloud-init and --cloud-init-name are mutually exclusive")
			}
			var cloudInitData string
			if cloudInitFile != "" {
				data, err := readCloudInitFile(cloudInitFile)
				if err != nil {
					return err
				}
				cloudInitData = data
			}

			if name == "" {
				name = imageRef
			}

			resolvedKeys, err := resolveSSHKeys(sshKeys)
			if err != nil {
				return err
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()

			req := &anvilv1.LaunchRequest{
				Name: name,
				Kind: kind,
				Vm: &anvilv1.VMSpec{
					ImageRef:          imageRef,
					Cpus:              cpus,
					MemoryMib:         memoryMiB,
					DiskGib:           diskGiB,
					CloudInitUserData: cloudInitData,
					CloudInitName:     cloudInitName,
					SshPublicKeys:     resolvedKeys,
				},
				NoStart: noStart,
			}

			stream, err := c.Launch(cmd.Context(), req)
			if err != nil {
				return err
			}
			for {
				ev, err := stream.Recv()
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				switch e := ev.GetEvent().(type) {
				case *anvilv1.LaunchProgress_Status:
					fmt.Fprintln(cmd.OutOrStdout(), e.Status)
				case *anvilv1.LaunchProgress_Error:
					return fmt.Errorf("%s", e.Error)
				case *anvilv1.LaunchProgress_Instance:
					fmt.Fprintf(cmd.OutOrStdout(), "Launched %q (%s)\n", e.Instance.GetName(), e.Instance.GetId())
				}
			}
		},
	}

	cmd.Flags().StringVar(&kindStr, "kind", "", `instance kind: "vm" or "container" (required)`)
	_ = cmd.MarkFlagRequired("kind")
	cmd.Flags().StringVar(&name, "name", "", "instance name (defaults to the image reference)")
	cmd.Flags().Int32Var(&cpus, "cpus", 1, "number of vCPUs (VM only)")
	cmd.Flags().Int64Var(&memoryMiB, "memory", 1024, "memory in MiB (VM only)")
	cmd.Flags().Int64Var(&diskGiB, "disk", 0, "disk size in GiB (VM only; 0 = catalog minimum)")
	cmd.Flags().StringVar(&cloudInitFile, "cloud-init", "", `path to a cloud-init user-data file, or "-" for stdin (VM only, ad hoc, not saved to the library)`)
	cmd.Flags().StringVar(&cloudInitName, "cloud-init-name", "", "name of a saved cloud-init config from the library (VM only, see `anvil cloud-init`)")
	cmd.Flags().StringArrayVar(&sshKeys, "ssh-key", nil, "additional SSH public key to authorize, on top of anvil's own managed key "+
		"(always authorized by default): a literal key, or a path to a .pub file; repeatable (VM only)")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "create but don't start the instance")
	return cmd
}

func readCloudInitFile(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		return string(data), err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading cloud-init file %s: %w", path, err)
	}
	return string(data), nil
}

// resolveSSHKeys always includes anvil's own managed public key (generated
// on first use if it doesn't exist yet, see ensureDefaultAnvilKey in
// ssh.go) — this is what makes `anvil shell`/`exec`/`transfer` work by
// default with zero flags, on any instance, without depending on whatever
// personal keys a given user happens to have in ~/.ssh. --ssh-key values
// (literal keys or paths to .pub files) are additive on top of that, for
// anyone who also wants to authorize their own personal key.
func resolveSSHKeys(explicit []string) ([]string, error) {
	anvilKeyPath, err := ensureDefaultAnvilKey()
	if err != nil {
		return nil, err
	}
	anvilPub, err := os.ReadFile(anvilKeyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("reading anvil's default public key: %w", err)
	}
	keys := []string{strings.TrimSpace(string(anvilPub))}

	for _, k := range explicit {
		resolved, err := resolveSSHKey(k)
		if err != nil {
			return nil, err
		}
		keys = append(keys, resolved)
	}
	return keys, nil
}

func resolveSSHKey(k string) (string, error) {
	// A literal public key, e.g. "ssh-ed25519 AAAA... comment", vs. a path
	// to a .pub file — distinguished by the well-known key-type prefixes,
	// since a real file path never starts with one of these.
	for _, prefix := range []string{"ssh-", "ecdsa-", "sk-"} {
		if strings.HasPrefix(k, prefix) {
			return k, nil
		}
	}
	data, err := os.ReadFile(k)
	if err != nil {
		return "", fmt.Errorf("reading SSH key file %s: %w", k, err)
	}
	return strings.TrimSpace(string(data)), nil
}
