package daemon

import (
	"context"
	"fmt"
	"os"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostpath"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/migrate"
)

// adoptStagedData moves the shared folders a migration uploaded into this host's
// own state dir, and points each mount at where they landed. Everything else is
// left alone, so an ordinary launch does nothing here. undo removes what was
// adopted, for a launch that then fails: the source still has its copy.
func adoptStagedData(ctx context.Context, params *instance.LaunchParams) (undo func(), err error) {
	var adopted []string
	undo = func() {
		for _, dir := range adopted {
			_ = os.RemoveAll(dir)
		}
	}
	adopt := func(staged, hostPath *string, dest string) error {
		if *staged == "" {
			return nil
		}
		dest = config.FreeDir(dest)
		if err := adoptOne(ctx, staged, hostPath, dest); err != nil {
			return err
		}
		adopted = append(adopted, dest)
		return nil
	}
	if params.VM != nil {
		for i := range params.VM.Mounts {
			m := &params.VM.Mounts[i]
			if err := adopt(&m.SourceDataPath, &m.HostPath, config.MountedFolderDir(params.Name, i)); err != nil {
				undo()
				return nil, fmt.Errorf("daemon: restoring the folder shared at %s: %w", m.GuestPath, err)
			}
		}
	}
	if params.Container != nil {
		for i := range params.Container.Volumes {
			v := &params.Container.Volumes[i]
			if err := adopt(&v.SourceDataPath, &v.HostPath, config.ImportedVolumeDir(params.Name, i)); err != nil {
				undo()
				return nil, fmt.Errorf("daemon: restoring the volume mounted at %s: %w", v.ContainerPath, err)
			}
		}
	}
	return undo, nil
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

func adoptOne(ctx context.Context, stagedPath, hostPath *string, dest string) error {
	if *stagedPath == "" {
		return nil
	}
	// Adopting deletes the source, so only ever accept a staged upload's own
	// name, never an arbitrary path a caller put in the request.
	if !migrate.IsStagedDataPath(*stagedPath) {
		return fmt.Errorf("daemon: %q is not a staged migration upload", *stagedPath)
	}
	// A real directory the caller uploaded, not a symlink to the daemon's own files.
	if info, err := os.Lstat(*stagedPath); err != nil || !info.IsDir() {
		return fmt.Errorf("daemon: %q is not a staged migration upload directory", *stagedPath)
	}
	if err := checkCallerOwns(ctx, stagedPath); err != nil {
		return err
	}
	if err := hostpath.MoveDir(*stagedPath, dest); err != nil {
		return err
	}
	*hostPath = dest
	*stagedPath = ""
	return nil
}
