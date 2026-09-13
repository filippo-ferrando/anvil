package commands

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	anvilv1 "github.com/anvil-project/anvil/api/gen/anvil/v1"
)

func newMountCommand(flags *globalFlags) *cobra.Command {
	var readOnly bool
	cmd := &cobra.Command{
		Use:   "mount <host-path> <name>:<guest-path>",
		Short: "Share a host directory into a VM (via 9p)",
		Long: "Share a host directory into a VM, over 9p. If the instance is currently " +
			"running, this restarts its guest OS to attach the share — there's no way to " +
			"hot-plug a 9p share into a live QEMU instance (checked against a real QEMU " +
			"build, not assumed). The instance's disk is untouched, but anything running " +
			"inside it is interrupted, same as a reboot.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hostPath := args[0]
			name, guestPath, ok := splitInstanceRef(args[1])
			if !ok {
				return fmt.Errorf(`second argument must be "<name>:<guest-path>"`)
			}
			absHostPath, err := filepath.Abs(hostPath)
			if err != nil {
				return fmt.Errorf("resolving %s: %w", hostPath, err)
			}

			c, err := dial(flags)
			if err != nil {
				return err
			}
			defer c.Close()
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
		Long: "Remove a mount added with `anvil mount`. Like `anvil mount`, this restarts " +
			"the guest OS if the instance is currently running.",
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
