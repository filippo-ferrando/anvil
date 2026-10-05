package migrate

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anvil-project/anvil/internal/instance"
)

// preflight checks t can actually run spec before anything is stopped or
// transferred. It returns the probe result and any non-blocking warnings.
func (m *Manager) preflight(ctx context.Context, t target, spec *instance.Spec, destName string) (remoteCaps, []string, error) {
	caps := probeRemote(ctx, t, spec, destName)
	if caps.anvil == "" || caps.anvild == "" {
		return caps, nil, fmt.Errorf("migrate: %s@%s has no anvil/anvild on PATH, install anvil there first", t.User, t.Host)
	}

	var fatal, warn []string
	if dataDirs(spec) > 0 && !caps.tar {
		fatal = append(fatal, "tar is not installed there, and this instance's shared folders need it to travel")
	}
	if caps.nameTaken {
		fatal = append(fatal, fmt.Sprintf("an instance named %q already exists on the target (use --dest-name)", destName))
	}

	switch spec.Kind {
	case instance.KindVM:
		f, w := preflightVM(spec, caps)
		fatal, warn = append(fatal, f...), append(warn, w...)
	case instance.KindContainer:
		f, w := preflightContainer(spec, caps)
		fatal, warn = append(fatal, f...), append(warn, w...)
	}

	if len(fatal) > 0 {
		return caps, warn, fmt.Errorf("migrate: target %s@%s cannot run %q: %s", t.User, t.Host, spec.Name, strings.Join(fatal, "; "))
	}
	return caps, warn, nil
}

func preflightVM(spec *instance.Spec, caps remoteCaps) (fatal, warn []string) {
	arch := spec.VM.Arch
	if arch == "" {
		arch = "x86_64"
	}
	switch {
	case !caps.qemu:
		fatal = append(fatal, fmt.Sprintf("%s is not installed there (the VM's arch is %s, the target's is %s)",
			qemuBinary(arch), arch, caps.arch))
	case caps.arch != "" && caps.arch != arch:
		warn = append(warn, fmt.Sprintf("the target is %s and the VM is %s, so it will run under emulation and be slow", caps.arch, arch))
	case !caps.kvm:
		warn = append(warn, "the target has no /dev/kvm, so the VM will run under emulation and be slow")
	}

	if caps.memTotalKB > 0 && spec.VM.MemoryMiB > caps.memTotalKB/1024 {
		warn = append(warn, fmt.Sprintf("the VM asks for %d MiB and the target has %d MiB of RAM in total",
			spec.VM.MemoryMiB, caps.memTotalKB/1024))
	}

	// The real transferred size is only known once the disk is exported, so
	// this uses the current overlay as a floor.
	if size := fileSize(spec.VM.DiskPath) + dataSize(spec); size > 0 {
		fatal = append(fatal, caps.spaceShortfall(size)...)
	}

	if n := dataDirs(spec); n > 0 {
		warn = append(warn, fmt.Sprintf("%d shared folder(s) travel with this VM, and land under %s on the target, not their current host paths",
			n, remoteStateDir+"/vm-mounts"))
	}
	if skipped := len(spec.VM.Mounts) - dataDirs(spec); skipped > 0 {
		warn = append(warn, fmt.Sprintf("%d shared folder(s) are not directories anvild can read, so they are not shared on the target", skipped))
	}
	return fatal, warn
}

func preflightContainer(spec *instance.Spec, caps remoteCaps) (fatal, warn []string) {
	engine := string(spec.Container.Engine)
	if engine == "" {
		engine = "docker"
	}
	if (engine == "docker" && !caps.docker) || (engine == "podman" && !caps.podman) {
		fatal = append(fatal, engine+" is not installed there")
	}
	if caps.arch != "" && caps.arch != hostArch() {
		warn = append(warn, fmt.Sprintf("the target is %s and this host is %s, so %s is re-pulled for a different arch",
			caps.arch, hostArch(), spec.Container.ImageRef))
	}
	if n := dataDirs(spec); n > 0 {
		warn = append(warn, fmt.Sprintf("%d bind-mounted directory(ies) travel with this container, and land under %s on the target, not their current host paths",
			n, remoteStateDir+"/container-volumes"))
	}
	if skipped := bindMountPaths(spec) - dataDirs(spec); skipped > 0 {
		warn = append(warn, fmt.Sprintf("%d bind mount(s) are not directories anvild can read, so the target mounts those same host paths instead", skipped))
	}
	return fatal, warn
}

// spaceShortfall reports which of the target's filesystems cannot hold a
// transfer of need bytes. An unknown amount of free space is not reported.
func (c remoteCaps) spaceShortfall(need int64) []string {
	var out []string
	for _, fs := range []struct {
		dir    string
		freeKB int64
	}{
		{remoteStagingDir, c.freeStagingKB},
		{remoteStateDir, c.freeStateKB},
		{remoteCacheDir, c.freeCacheKB},
	} {
		if fs.freeKB > 0 && fs.freeKB*1024 < need {
			out = append(out, fmt.Sprintf("%s on the target has %s free, the disk needs at least %s",
				fs.dir, humanBytes(fs.freeKB*1024), humanBytes(need)))
		}
	}
	return out
}

// dataDirs counts spec's bind mounts whose contents can actually travel.
func dataDirs(spec *instance.Spec) int {
	n := 0
	for _, path := range bindMounts(spec) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			n++
		}
	}
	return n
}

// dataSize is how much those directories hold in total.
func dataSize(spec *instance.Spec) int64 {
	var total int64
	for _, path := range bindMounts(spec) {
		total += dirSize(path)
	}
	return total
}

// bindMountPaths is how many host paths spec mounts, readable or not.
func bindMountPaths(spec *instance.Spec) int { return len(bindMounts(spec)) }

// bindMounts is every host path spec shares into its guest or container.
func bindMounts(spec *instance.Spec) []string {
	var paths []string
	if spec.VM != nil {
		for _, m := range spec.VM.Mounts {
			paths = append(paths, m.HostPath)
		}
	}
	if spec.Container != nil {
		for _, v := range spec.Container.Volumes {
			paths = append(paths, v.HostPath)
		}
	}
	return paths
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func qemuBinary(arch string) string { return "qemu-system-" + arch }
