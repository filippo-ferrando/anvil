//go:build linux

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/gui"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var (
	_ instance.GUIPreparer     = (*FCBackend)(nil)
	_ instance.GuestFileReader = (*FCBackend)(nil)
)

// fcToolsMount mounts the read-only tools drive, found by its ISO label.
var fcToolsMount = `mount -o ro "$(blkid -L ` + gui.ToolsTag + ` || findfs LABEL=` + gui.ToolsTag + `)" ` + gui.GuestToolsDir

var toolsISOMu sync.Mutex

// toolsISO packs the GUI tools dir into a read-only ISO drive, since firecracker has no
// virtiofs. Built once into the cache and again when the tools change.
func (b *FCBackend) toolsISO() (string, error) {
	toolsISOMu.Lock()
	defer toolsISOMu.Unlock()
	src := config.GUIToolsDir()
	iso := filepath.Join(b.Cache.Dir, "gui-tools.iso")
	if fresh, err := newerThanAll(iso, src); err != nil || fresh {
		return iso, err
	}
	if err := os.MkdirAll(b.Cache.Dir, 0o750); err != nil {
		return "", err
	}
	tmp := iso + ".part"
	// -R keeps the binaries executable, -L the label the guest mounts it by.
	cmd := exec.Command("xorriso", "-as", "genisoimage", "-R", "-V", gui.ToolsTag, "-o", tmp, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("vm: packing the GUI tools: %w: %s", err, out)
	}
	return iso, os.Rename(tmp, iso)
}

// newerThanAll reports whether path exists and is newer than every file in dir.
func newerThanAll(path, dir string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("vm: reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(info.ModTime()) {
			return false, nil
		}
	}
	return true, nil
}

func (b *FCBackend) PrepareGUI(ctx context.Context, spec *instance.Spec, x11 bool, progress func(status string)) (instance.GUISession, error) {
	if spec.VM == nil {
		return instance.GUISession{}, fmt.Errorf("vm: PrepareGUI called with a nil VMSpec")
	}
	b.mu.Lock()
	_, running := b.running[spec.ID]
	b.mu.Unlock()
	if !running {
		return instance.GUISession{}, fmt.Errorf("vm: %s isn't running", spec.Name)
	}
	return b.prepareGUI(ctx, spec, fcToolsMount, x11, progress)
}
