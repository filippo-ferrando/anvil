package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func newTransferCommand(flags *globalFlags) *cobra.Command {
	var user, identity string
	cmd := &cobra.Command{
		Use:   "transfer <source> <destination>",
		Short: "Copy a file to or from a VM (exactly one side must be <name>:<path>)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcName, srcPath, srcIsRemote := splitInstanceRef(args[0])
			dstName, dstPath, dstIsRemote := splitInstanceRef(args[1])
			if srcIsRemote == dstIsRemote {
				return fmt.Errorf("exactly one of <source>/<destination> must be an instance reference (<name>:<path>); " +
					"transferring between two instances, or between two local paths, isn't supported")
			}

			remoteName := srcName
			if dstIsRemote {
				remoteName = dstName
			}

			key, err := resolveIdentity(identity)
			if err != nil {
				return err
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			target, err := resolveSSHTarget(cmd.Context(), c, remoteName, user)
			c.Close()
			if err != nil {
				return err
			}

			scpBin, err := exec.LookPath("scp")
			if err != nil {
				return fmt.Errorf("scp: not found on PATH (needed for `anvil transfer`)")
			}
			scpArgs, err := commonSSHArgs(target, "-P", key)
			if err != nil {
				return err
			}
			remotePrefix := fmt.Sprintf("%s@%s:", target.User, target.Host)
			if srcIsRemote {
				scpArgs = append(scpArgs, remotePrefix+srcPath, dstPath)
			} else {
				scpArgs = append(scpArgs, srcPath, remotePrefix+dstPath)
			}

			scpCmd := exec.Command(scpBin, scpArgs...)
			scpCmd.Stdin, scpCmd.Stdout, scpCmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return scpCmd.Run()
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "SSH user for the remote side (defaults to the image's default user)")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "path to a private key to use, instead of anvil's own managed key")
	return cmd
}

// splitInstanceRef splits "<name>:<path>" into its parts. A plain local
// path (the common case) has no colon and isRemote comes back false.
func splitInstanceRef(s string) (name, path string, isRemote bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", s, false
	}
	return s[:idx], s[idx+1:], true
}
