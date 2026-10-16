package daemon

import (
	"fmt"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostpath"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/migrate"
)

// adoptStagedData moves the shared folders a migration uploaded into this host's
// own state dir, and points each mount at where they landed. Everything else is
// left alone, so an ordinary launch does nothing here.
func adoptStagedData(params *instance.LaunchParams) error {
	if params.VM != nil {
		for i := range params.VM.Mounts {
			m := &params.VM.Mounts[i]
			dest := config.MountedFolderDir(params.Name, i)
			if err := adoptOne(&m.SourceDataPath, &m.HostPath, dest); err != nil {
				return fmt.Errorf("daemon: restoring the folder shared at %s: %w", m.GuestPath, err)
			}
		}
	}
	if params.Container != nil {
		for i := range params.Container.Volumes {
			v := &params.Container.Volumes[i]
			dest := config.ImportedVolumeDir(params.Name, i)
			if err := adoptOne(&v.SourceDataPath, &v.HostPath, dest); err != nil {
				return fmt.Errorf("daemon: restoring the volume mounted at %s: %w", v.ContainerPath, err)
			}
		}
	}
	return nil
}

// sharedPaths returns the host paths params shares into its instance, except data a
// migration staged: that is moved into the daemon's own state dir instead.
func sharedPaths(params instance.LaunchParams) []*string {
	var paths []*string
	if params.VM != nil {
		for i := range params.VM.Mounts {
			if params.VM.Mounts[i].SourceDataPath == "" {
				paths = append(paths, &params.VM.Mounts[i].HostPath)
			}
		}
	}
	if params.Container != nil {
		for i := range params.Container.Volumes {
			if params.Container.Volumes[i].SourceDataPath == "" {
				paths = append(paths, &params.Container.Volumes[i].HostPath)
			}
		}
	}
	return paths
}

func adoptOne(stagedPath, hostPath *string, dest string) error {
	if *stagedPath == "" {
		return nil
	}
	// Adopting deletes the source, so only ever accept a staged upload's own
	// name, never an arbitrary path a caller put in the request.
	if !migrate.IsStagedDataPath(*stagedPath) {
		return fmt.Errorf("daemon: %q is not a staged migration upload", *stagedPath)
	}
	if err := hostpath.MoveDir(*stagedPath, dest); err != nil {
		return err
	}
	*hostPath = dest
	*stagedPath = ""
	return nil
}
