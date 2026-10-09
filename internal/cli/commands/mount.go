package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newMountCommand(flags *globalFlags) *cobra.Command {
	var readOnly bool
	cmd := &cobra.Command{
		Use:   "mount <host-path> <name>:<guest-path>",
		Short: "Share a host directory into a VM (via virtiofs)",
		Long: "Share a host directory into a VM, over virtiofs (needs virtiofsd on the host). " +
			"On a running VM whose guest agent is connected, the share is hot-plugged and " +
			"mounted live. Without the agent, the guest OS restarts to attach it: the disk is " +
			"untouched, but anything running inside is interrupted, same as a reboot. " +
			"A VM holds at most 8 mounts.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostPath := args[0]
			name, guestPath, ok := splitInstanceRef(args[1])
			if !ok {
				return fmt.Errorf(`second argument must be "<name>:<guest-path>"`)
			}
			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			// With --remote, host-path is a directory on the daemon's host.
			absHostPath, err := c.HostPath(hostPath)
			if err != nil {
				return err
			}
			_, err = c.Mount(cmd.Context(), &anvilv1.MountRequest{
				Name:      name,
				HostPath:  absHostPath,
				GuestPath: guestPath,
				ReadOnly:  readOnly,
			})
			return err
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "mount read-only in the guest")
	return cmd
}

func newUmountCommand(flags *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "umount <name>:<guest-path>",
		Short: "Remove a mount added with `anvil mount`",
		Long: "Remove a mount added with `anvil mount`. Like `anvil mount`, this happens live " +
			"when the guest agent is connected (it fails if the mount is still in use inside " +
			"the guest), and restarts a running guest OS otherwise.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, guestPath, ok := splitInstanceRef(args[0])
			if !ok {
				return fmt.Errorf(`argument must be "<name>:<guest-path>"`)
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = c.Umount(cmd.Context(), &anvilv1.UmountRequest{Name: name, GuestPath: guestPath})
			return err
		},
	}
}
