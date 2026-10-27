package container

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/container/docker"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

// Overridden by tests.
var (
	guiRelayDir = config.GUIRelayDir
	driDir      = "/dev/dri"
)

var _ instance.GUIPreparer = (*DockerBackend)(nil)

func guiX11Dir(id string) string { return filepath.Join(guiRelayDir(id), "x11") }

// addGUIParams binds the instance's relay directories and passes the host GPU through, if any.
func addGUIParams(p *docker.CreateContainerParams, id string) {
	p.Volumes = append(p.Volumes,
		docker.VolumeMount{HostPath: guiRelayDir(id), ContainerPath: gui.ContainerRuntimeDir},
		docker.VolumeMount{HostPath: guiX11Dir(id), ContainerPath: gui.ContainerX11Dir},
	)
	if gids := deviceGroups(driDir); gids != nil {
		p.Devices = append(p.Devices, driDir)
		p.GroupAdd = append(p.GroupAdd, gids...)
	}
}

// deviceGroups returns the group IDs owning the device files in dir, or nil when dir has none.
func deviceGroups(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var gids []string
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.Mode()&os.ModeDevice == 0 {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if gid := strconv.FormatUint(uint64(st.Gid), 10); !slices.Contains(gids, gid) {
			gids = append(gids, gid)
		}
	}
	return gids
}

// ensureGUIDirs creates the relay directories (world-writable, as container users vary; the
// parent run dir keeps other host users out). It reports whether they had to be created.
func ensureGUIDirs(id string) (bool, error) {
	x11 := guiX11Dir(id)
	if _, err := os.Stat(x11); err == nil {
		return false, nil
	}
	for _, dir := range []string{guiRelayDir(id), x11} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("docker: creating GUI directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			return false, fmt.Errorf("docker: chmod GUI directory %s: %w", dir, err)
		}
	}
	return true, nil
}

func removeGUIDirs(id string) {
	if err := os.RemoveAll(guiRelayDir(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("docker: removing GUI directory of %s: %v", id, err)
	}
}

// PrepareGUI makes sure spec's relay directories exist and are the ones its container sees.
func (b *DockerBackend) PrepareGUI(ctx context.Context, spec *instance.Spec, x11 bool, progress func(status string)) (instance.GUISession, error) {
	if spec.Container == nil {
		return instance.GUISession{}, fmt.Errorf("docker: PrepareGUI called with a nil ContainerSpec")
	}
	created, err := ensureGUIDirs(spec.ID)
	if err != nil {
		return instance.GUISession{}, err
	}
	if created {
		// The running container still has the old, deleted directories bound.
		progress("restarting " + spec.Name + " to reattach its GUI directory")
		if err := b.Stop(ctx, spec, false, 10*time.Second); err != nil {
			return instance.GUISession{}, err
		}
		if err := b.Start(ctx, spec); err != nil {
			return instance.GUISession{}, err
		}
	}
	return instance.GUISession{RelayDir: guiRelayDir(spec.ID), X11Dir: guiX11Dir(spec.ID)}, nil
}

var _ instance.GuestFileReader = (*DockerBackend)(nil)

// maxGuestFile skips container files bigger than this.
const maxGuestFile = 4 << 20

// listedFileMax is the biggest file a directory listing keeps the content of, so reading
// the small files of a listed dir (like .desktop entries) needs no further Docker call.
const listedFileMax = 256 << 10

// listedTTL is how long a listing's file contents are served to ReadGuestFile.
const listedTTL = 30 * time.Second

// guestFileCache holds the files that recent ListGuestDir calls fetched.
type guestFileCache struct {
	mu    sync.Mutex
	files map[string]listedFile // container ID + "\x00" + absolute path
}

type listedFile struct {
	entry docker.ArchiveEntry
	at    time.Time
}

func (c *guestFileCache) put(containerID, dir string, entries []docker.ArchiveEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = make(map[string]listedFile)
	}
	now := time.Now()
	for k, f := range c.files {
		if now.Sub(f.at) > listedTTL {
			delete(c.files, k)
		}
	}
	parent := path.Dir(dir)
	for _, e := range entries {
		if e.Typeflag == tar.TypeSymlink || (e.Typeflag == tar.TypeReg && e.Data != nil) {
			c.files[containerID+"\x00"+path.Join(parent, e.Name)] = listedFile{e, now}
		}
	}
}

func (c *guestFileCache) get(containerID, p string) (docker.ArchiveEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.files[containerID+"\x00"+path.Clean(p)]
	if !ok || time.Since(f.at) > listedTTL {
		return docker.ArchiveEntry{}, false
	}
	return f.entry, true
}

// ListGuestDir lists dir in the container through Docker's archive API. The same call
// fetches its small files, which ReadGuestFile then serves for a short while.
func (b *DockerBackend) ListGuestDir(ctx context.Context, spec *instance.Spec, dir string) ([]string, error) {
	if spec.Container == nil || spec.Container.ContainerID == "" {
		return nil, fmt.Errorf("docker: %s has no container yet", spec.Name)
	}
	entries, ok, err := b.Client.Archive(ctx, spec.Container.ContainerID, dir, listedFileMax)
	if err != nil || !ok {
		return nil, err
	}
	b.listed.put(spec.Container.ContainerID, dir, entries)
	base := path.Base(dir) + "/"
	var names []string
	for _, e := range entries {
		rest, found := strings.CutPrefix(strings.TrimSuffix(e.Name, "/"), base)
		if found && rest != "" && !strings.Contains(rest, "/") {
			names = append(names, rest)
		}
	}
	return names, nil
}

// ReadGuestFile reads p from the container, following symlinks (Docker's archive API doesn't).
func (b *DockerBackend) ReadGuestFile(ctx context.Context, spec *instance.Spec, p string) ([]byte, bool, error) {
	if spec.Container == nil || spec.Container.ContainerID == "" {
		return nil, false, fmt.Errorf("docker: %s has no container yet", spec.Name)
	}
	for range 8 {
		e, cached := b.listed.get(spec.Container.ContainerID, p)
		if !cached {
			entries, ok, err := b.Client.Archive(ctx, spec.Container.ContainerID, p, maxGuestFile)
			if err != nil || !ok || len(entries) == 0 {
				return nil, false, err
			}
			e = entries[0]
		}
		switch e.Typeflag {
		case tar.TypeReg:
			return e.Data, e.Data != nil, nil
		case tar.TypeSymlink:
			if path.IsAbs(e.Linkname) {
				p = e.Linkname
			} else {
				p = path.Join(path.Dir(p), e.Linkname)
			}
		default:
			return nil, false, nil
		}
	}
	return nil, false, nil // a symlink loop
}
