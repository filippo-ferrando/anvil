//go:build linux

package vm

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
)

// fcJailerEnv set to "off" runs firecracker directly even when the jailer is installed.
const fcJailerEnv = "ANVIL_FC_JAILER"

// jailerExe picks the jailer: the private copy with file capabilities the service unit
// prepares, or firecracker.LookPath's one for an anvild running as root. Empty runs firecracker directly.
func jailerExe() string {
	if os.Getenv(fcJailerEnv) == "off" {
		return ""
	}
	// The jailer copies only the binary into an empty chroot, where a dynamic one finds no loader.
	if fc, err := firecracker.ResolveBinary(); err == nil && firecracker.IsDynamic(fc) {
		log.Printf("vm: %s is dynamically linked and can't run in the jailer's chroot, running firecracker unjailed (install a static build to jail it)", fc)
		return ""
	}
	if _, err := os.Stat(config.JailerExe()); err == nil {
		return config.JailerExe()
	}
	if path, err := firecracker.LookPath(firecracker.JailerBinary); err == nil {
		if os.Geteuid() == 0 {
			return path
		}
		log.Printf("vm: %s has no capabilities without the copy anvild.service prepares at %s, running firecracker unjailed",
			path, config.JailerExe())
	}
	return ""
}

// fcPathsFor lays out instanceID's firecracker files: in its instance dir, or in its jail
// root when jail is set. The console log always stays in the instance dir.
func fcPathsFor(instanceID string, jail *firecracker.Jail) fcInstancePaths {
	dir := instanceDir(instanceID)
	base := dir
	if jail != nil {
		base = jail.Root()
	}
	return fcInstancePaths{
		Paths: firecracker.Paths{
			APISocket:  filepath.Join(base, "firecracker.sock"),
			ConfigFile: filepath.Join(base, "firecracker.json"),
			ConsoleLog: filepath.Join(dir, "console.log"),
			LogFile:    filepath.Join(base, "firecracker.log"),
			Jail:       jail,
		},
		vsock: filepath.Join(base, "vsock.sock"),
	}
}

// newJail returns the jail instanceID runs in, nil when this backend runs firecracker directly.
func (b *FCBackend) newJail(instanceID string) *firecracker.Jail {
	if b.Jailer == "" {
		return nil
	}
	return b.jailFor(instanceID, "")
}

// jailFor describes instanceID's jail; execFile is the firecracker binary it runs, empty for
// the one on PATH now.
func (b *FCBackend) jailFor(instanceID, execFile string) *firecracker.Jail {
	if execFile == "" {
		execFile, _ = firecracker.ResolveBinary()
	}
	return &firecracker.Jail{ID: instanceID, ChrootBase: b.ChrootBase, UID: os.Getuid(), GID: os.Getgid(),
		Jailer: b.Jailer, ExecFile: execFile}
}

// pathsOf returns the paths instanceID's running firecracker uses.
func (b *FCBackend) pathsOf(instanceID string) fcInstancePaths {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.paths[instanceID]; ok {
		return p
	}
	return fcPathsFor(instanceID, nil)
}

// prepareJail starts paths' jail root afresh, with firecracker's log linked to the instance
// dir so it outlives the jail. Without a jail it does nothing.
func prepareJail(instanceID string, paths fcInstancePaths) error {
	if paths.Jail == nil {
		return nil
	}
	root := paths.Jail.Root()
	_ = os.RemoveAll(filepath.Dir(root)) // left by a crash; nothing runs in it now
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("vm: creating jail %s: %w", root, err)
	}
	log := filepath.Join(instanceDir(instanceID), "firecracker.log")
	f, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("vm: %w", err)
	}
	f.Close()
	return os.Link(log, paths.LogFile)
}

// jailFile makes src reachable by a jailed firecracker as name, and returns the path it uses.
// A small file (copyOK) is copied; the disk is hard-linked, never copied.
func jailFile(paths fcInstancePaths, src, name string, copyOK bool) (string, error) {
	if paths.Jail == nil {
		return src, nil
	}
	dst := filepath.Join(paths.Jail.Root(), name)
	// Shared files (kernel, GUI tools) get a private copy, so one VM's firecracker can't
	// change what another one boots. Copy-on-write makes that cheap where the filesystem allows.
	if copyOK {
		if out, err := exec.Command("cp", "--reflink=auto", src, dst).CombinedOutput(); err != nil {
			return "", fmt.Errorf("vm: copying %s into the jail: %w: %s", src, err, out)
		}
		return paths.InVMM(dst), nil
	}
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return "", fmt.Errorf("vm: putting %s into the jail: the jail dir %s must be on the same filesystem: %w",
				src, paths.Jail.ChrootBase, err)
		}
		return "", fmt.Errorf("vm: putting %s into the jail: %w", src, err)
	}
	return paths.InVMM(dst), nil
}

// removeJail deletes paths' jail dir once firecracker is gone; the linked files stay.
func removeJail(paths fcInstancePaths) {
	if paths.Jail != nil {
		_ = os.RemoveAll(filepath.Dir(paths.Jail.Root()))
	}
}
