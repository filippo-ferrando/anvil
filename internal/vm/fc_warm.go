//go:build linux

package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/ephemeral"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/hostdev"
	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/firecracker"
	"git.fferrando.cc/rdfilippo/anvil/internal/vm/image"
)

var _ ephemeral.Engine = (*FCBackend)(nil)

var errNeedsJailer = errors.New("vm: ephemeral clones need firecracker's jailer, which anvild isn't using " +
	"(it needs jailer and setcap at anvild's start, see docs/firecracker.md)")

// warmMAC is the MAC every warm build VM, and so every clone, has.
const warmMAC = "06:00:0a:c8:00:02"

// warmDiskGiB is a warm VM's disk, sparse; each clone gets a copy-on-write copy of it.
const warmDiskGiB = 10

// The files of a warm snapshot, under the same names in a clone's jail as in the build's.
const (
	warmState = "vmstate"
	warmMem   = "mem"
	warmDisk  = "disk.raw"
	warmSeed  = "seed.iso"
	warmGuest = "guest.iso"
)

// warmVMSpec is the VM a warm snapshot of s is built from, in the clone network layout.
func warmVMSpec(s ephemeral.WarmSpec, defaultUser string) *instance.VMSpec {
	gw := netip.MustParsePrefix(hostdev.CloneTapAddr).Addr().String()
	return &instance.VMSpec{
		ImageRef: s.Distro, Arch: s.Arch, CPUs: s.CPUs, MemoryMiB: s.MemoryMiB, DiskGiB: s.DiskGiB,
		Hypervisor: instance.VMHypervisorFirecracker, DefaultUser: defaultUser,
		NetworkMode: "bridge", StaticIP: hostdev.CloneGuestIP + "/30", Gateway: gw, DNSServers: []string{gw},
		MACAddress: warmMAC, NoGuestAgent: true, AnvilGuest: true,
	}
}

func (b *FCBackend) WarmSpec(p ephemeral.WarmParams) (ephemeral.WarmSpec, error) {
	if err := b.canClone(); err != nil {
		return ephemeral.WarmSpec{}, err
	}
	entry, distro, kernel, err := b.resolve(&instance.VMSpec{ImageRef: p.Distro, Arch: p.Arch})
	if err != nil {
		return ephemeral.WarmSpec{}, err
	}
	fcVersion, err := firecrackerVersion()
	if err != nil {
		return ephemeral.WarmSpec{}, err
	}
	s := ephemeral.WarmSpec{
		Distro: entry.ID, Arch: entry.Arch, CPUs: p.CPUs, MemoryMiB: p.MemoryMiB,
		DiskGiB: max(warmDiskGiB, entry.MinDiskGiB), FirecrackerVersion: fcVersion,
	}
	if s.CPUs <= 0 {
		s.CPUs = ephemeral.DefaultCPUs
	}
	if s.MemoryMiB <= 0 {
		s.MemoryMiB = ephemeral.DefaultMemoryMiB
	}
	v := warmVMSpec(s, entry.DefaultUser)
	userData, err := seedUserData(v, "")
	if err != nil {
		return ephemeral.WarmSpec{}, err
	}
	kernelSum := kernel.SHA256
	if path := os.Getenv(image.KernelEnv); path != "" {
		if kernelSum, err = fileSHA256(path); err != nil {
			return ephemeral.WarmSpec{}, err
		}
	}
	guestSum, err := fileSHA256(filepath.Join(config.GuestToolsDir(), guestBinary))
	if err != nil {
		return ephemeral.WarmSpec{}, fmt.Errorf("vm: %s isn't installed in %s: %w", guestBinary, config.GuestToolsDir(), err)
	}
	h := sha256.New()
	for _, part := range []string{
		"warm-v1", entry.ID, entry.Arch, entry.Version, entry.URL, entry.SHA256, distro.KernelArgs,
		fmt.Sprint(s.CPUs, s.MemoryMiB, s.DiskGiB), userData, bridgeNetworkConfig(v, v.MACAddress),
		fcVersion, kernelSum, guestSum,
	} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	s.Key = hex.EncodeToString(h.Sum(nil))[:16]
	return s, nil
}

// firecrackerVersion is `firecracker --version`'s first line, e.g. "Firecracker v1.17.0".
func firecrackerVersion() (string, error) {
	bin, err := firecracker.LookPath(firecracker.Binary)
	if err != nil {
		return "", fmt.Errorf("vm: firecracker isn't installed on this host: %w", err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("vm: %s --version: %w", bin, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildWarm boots a build VM for s in a clone netns, waits for cloud-init and anvil-guest,
// then pauses it and keeps its snapshot, disk and drives in dir.
func (b *FCBackend) BuildWarm(ctx context.Context, s ephemeral.WarmSpec, dir string, progress func(string)) (err error) {
	if err := b.canClone(); err != nil {
		return err
	}
	if progress == nil {
		progress = func(string) {}
	}
	id := "eph-W" + s.Key
	entry, _, _, err := b.resolve(&instance.VMSpec{ImageRef: s.Distro, Arch: s.Arch})
	if err != nil {
		return err
	}
	spec := &instance.Spec{ID: id, Name: id, Kind: instance.KindVM, VM: warmVMSpec(s, entry.DefaultUser)}
	// Gone on every path: the snapshot keeps all it needs.
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_ = b.Stop(cleanup, spec, true, 0)
		_ = b.Delete(cleanup, spec)
		_ = b.Clones.Teardown(cleanup, id)
	}()

	if err := b.Create(ctx, spec, progress); err != nil {
		return err
	}
	progress("starting the build VM")
	cn, err := b.Clones.Setup(ctx, id)
	if err != nil {
		return err
	}
	if err := b.start(ctx, spec, &cn); err != nil {
		return err
	}
	if err := b.WaitReady(ctx, spec, progress); err != nil {
		return err
	}
	progress("waiting for anvil-guest")
	paths := b.pathsOf(id)
	// The page cache stays in the snapshot anyway; sync only keeps the disk copy current.
	if err := guestCommand(ctx, paths.vsock, 60*time.Second, "sync"); err != nil {
		return fmt.Errorf("vm: anvil-guest in the build VM: %w", err)
	}

	progress("taking the snapshot")
	proc, ok := b.lookup(id)
	if !ok {
		return fmt.Errorf("vm: the build VM is gone")
	}
	if err := proc.API.Pause(ctx); err != nil {
		return fmt.Errorf("vm: pausing the build VM: %w", err)
	}
	root := paths.root()
	if err := proc.API.CreateSnapshot(ctx, firecracker.SnapshotCreate{
		SnapshotPath: paths.InVMM(filepath.Join(root, warmState)), MemFilePath: paths.InVMM(filepath.Join(root, warmMem)),
		SnapshotType: firecracker.SnapshotFull,
	}); err != nil {
		return fmt.Errorf("vm: snapshotting the build VM: %w", err)
	}

	part := dir + ".part"
	_ = os.RemoveAll(part)
	if err := os.MkdirAll(part, 0o750); err != nil {
		return err
	}
	defer os.RemoveAll(part)
	for _, name := range []string{warmState, warmMem} {
		if err := os.Rename(filepath.Join(root, name), filepath.Join(part, name)); err != nil {
			return fmt.Errorf("vm: keeping the snapshot: %w", err)
		}
	}
	// The VM is paused, so the disk is exactly what the memory file expects.
	if err := image.CloneDisk(ctx, spec.VM.DiskPath, filepath.Join(part, warmDisk), 0); err != nil {
		return err
	}
	for name, src := range map[string]string{warmSeed: spec.VM.SeedISOPath, warmGuest: filepath.Join(root, warmGuest)} {
		if out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", src, filepath.Join(part, name)).CombinedOutput(); err != nil {
			return fmt.Errorf("vm: keeping %s: %w: %s", name, err, out)
		}
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}
	return os.Rename(part, dir)
}

// guestCommand runs argv through anvil-guest behind vsock, retrying the dial for up to
// wait while anvil-guest starts, and fails on a non-zero exit.
func guestCommand(ctx context.Context, vsock string, wait time.Duration, argv ...string) error {
	conn, err := dialGuest(ctx, vsock, wait)
	if err != nil {
		return err
	}
	var out strings.Builder
	code, err := guestexec.Run(ctx, conn, guestexec.Request{Header: guestexec.Header{Argv: argv}, Stdout: &out, Stderr: &out})
	if err == nil && code != 0 {
		err = fmt.Errorf("%s exited %d: %s", argv[0], code, strings.TrimSpace(out.String()))
	}
	return err
}

// dialGuest connects to anvil-guest, retrying for up to wait while nothing listens yet.
func dialGuest(ctx context.Context, vsock string, wait time.Duration) (io.ReadWriteCloser, error) {
	deadline := time.Now().Add(wait)
	for {
		conn, err := firecracker.DialVsock(ctx, vsock, guestexec.Port)
		if err == nil || time.Now().After(deadline) {
			return conn, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
