// Package commands implements anvil's CLI verbs (cobra commands), each
// dialing anvild over its gRPC unix socket via pkg/client.
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
		// The caller prints the returned error itself, so cobra shouldn't print its own.
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flags.socket, "socket", config.SocketPath(), "anvild unix socket path")

	root.AddCommand(
		newLaunchCommand(flags),
		newListCommand(flags),
		newInfoCommand(flags),
		newStatsCommand(flags),
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
		newFindCommand(flags),
		newMountCommand(flags),
		newUmountCommand(flags),
		newPortCommand(flags),
		newSnapshotCommand(flags),
		newCreateDirCommand(flags),
		newIntentCommand(flags),
		newHostCommand(flags),
		newMigrateCommand(flags),
		newMigrateImportCommand(flags),
		newMigrateRollbackCommand(flags),
		newMigrateKeyCommand(flags),
		newMigrateGuestKeyCommand(flags),
		newExportCommand(flags),
		newImportCommand(flags),
		newTuiCommand(flags),
	)
	return root
}

func dial(flags *globalFlags) (*client.Client, error) {
	return client.Dial(flags.socket)
}
