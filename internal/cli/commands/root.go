// Package commands implements anvil's CLI verbs (cobra commands). Each
// command dials anvild over its gRPC unix socket via pkg/client and does
// nothing but translate flags to a request and print the reply — all
// actual logic lives in the daemon, per the plan's thin-client mandate.
package commands

import (
	"github.com/spf13/cobra"

	"github.com/anvil-project/anvil/internal/config"
	"github.com/anvil-project/anvil/pkg/client"
)

type globalFlags struct {
	socket string
}

// NewRootCommand builds the full `anvil` command tree.
func NewRootCommand() *cobra.Command {
	flags := &globalFlags{}
	root := &cobra.Command{
		Use:          "anvil",
		Short:        "Manage cloud-init VMs and Docker/Podman containers",
		SilenceUsage: true,
		// cmd/anvil/main.go already prints whatever error Execute()
		// returns (prefixed "anvil:"), so cobra printing its own "Error:
		// ..." line first would just be the same message twice — that's
		// exactly what was happening before this was set to true.
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flags.socket, "socket", config.SocketPath(), "anvild unix socket path")

	root.AddCommand(
		newLaunchCommand(flags),
		newListCommand(flags),
		newInfoCommand(flags),
		newStartCommand(flags),
		newStopCommand(flags),
		newDeleteCommand(flags),
		newPurgeCommand(flags),
		newCloudInitCommand(flags),
		newMirrorCommand(flags),
		newShellCommand(flags),
		newExecCommand(flags),
		newTransferCommand(flags),
		newLogsCommand(flags),
		newImageCommand(flags),
		newMountCommand(flags),
		newUmountCommand(flags),
		newCreateDirCommand(flags),
		newIntentCommand(flags),
		newHostCommand(flags),
		newMigrateCommand(flags),
		newMigrateImportCommand(flags),
		newMigrateRollbackCommand(flags),
		newMigrateKeyCommand(flags),
		newMigrateGuestKeyCommand(flags),
		newTuiCommand(flags),
	)
	return root
}

func dial(flags *globalFlags) (*client.Client, error) {
	return client.Dial(flags.socket)
}
