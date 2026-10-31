//go:build linux

package hostdev

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// dmsetup is the device-mapper tool; tests may point it elsewhere.
var dmsetup = "dmsetup"

// createCloneDisk builds clone d's disk as CreateCloneDisk describes, the node owned by uid:gid.
func createCloneDisk(d CloneDisk, uid, gid int) (err error) {
	if err := d.validate(); err != nil {
		return err
	}
	_ = deleteCloneDisk(d.ID) // left by a crash; nothing uses it now
	// Opened here, by the helper, and checked to be anvild's own: anvild can't point the
	// helper at a file it couldn't already write.
	origin, size, err := openOwned(filepath.Join(WarmRoot, d.Key, CloneDiskFile), uid)
	if err != nil {
		return err
	}
	defer origin.Close()
	cow, cowSize, err := openOwned(filepath.Join(InstanceRoot, d.ID, CloneCOWFile), uid)
	if err != nil {
		return err
	}
	defer cow.Close()
	if size == 0 || size%512 != 0 {
		return fmt.Errorf("warm disk of %s: size %d is not a whole number of sectors", d.Key, size)
	}
	if cowSize < size {
		return fmt.Errorf("copy-on-write file of %s: %d bytes, smaller than the disk's %d", d.ID, cowSize, size)
	}
	jail, err := openJail(d.Jail)
	if err != nil {
		return err
	}
	defer jail.Close()
	if err := loadDMSnapshot(); err != nil {
		return err
	}

	// dm-snapshot never writes the origin (only a snapshot-origin target would), but may
	// open it for writing, so its loop device isn't read-only.
	originLoop, err := attachLoop(origin, d.ID+"-origin")
	if err != nil {
		return err
	}
	defer originLoop.Close() // autoclear: the loop goes once the dm device lets it go
	cowLoop, err := attachLoop(cow, d.ID+"-cow")
	if err != nil {
		return err
	}
	defer cowLoop.Close()

	name := DMName(d.ID)
	table := fmt.Sprintf("0 %d snapshot %s %s N 8", size/512, originLoop.Name(), cowLoop.Name())
	if out, err := exec.Command(dmsetup, "create", name, "--noudevsync", "--table", table).CombinedOutput(); err != nil {
		return fmt.Errorf("dmsetup create %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	defer func() {
		if err != nil {
			_ = deleteCloneDisk(d.ID)
		}
	}()
	dev, err := dmDevice(name)
	if err != nil {
		return err
	}
	if err := unix.Mknodat(int(jail.Fd()), CloneDiskFile, unix.S_IFBLK|0o600, int(dev)); err != nil {
		return fmt.Errorf("creating %s in the jail: %w", CloneDiskFile, err)
	}
	if err := unix.Fchownat(int(jail.Fd()), CloneDiskFile, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("handing %s in the jail to uid %d: %w", CloneDiskFile, uid, err)
	}
	return nil
}

// deleteCloneDisk removes clone id's dm device; its loop devices go with it (autoclear).
// The node in the jail goes with the jail.
func deleteCloneDisk(id string) error {
	if !cloneIDRe.MatchString(id) {
		return fmt.Errorf("invalid clone id %q", id)
	}
	name := DMName(id)
	if !dmExists(name) {
		return nil
	}
	out, err := exec.Command(dmsetup, "remove", "--retry", "--noudevsync", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such device") {
		return fmt.Errorf("dmsetup remove %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// openOwned opens path for reading and writing, without following a symlink, and only
// when it is a regular file owned by uid. It returns the file and its size.
func openOwned(path string, uid int) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("opening %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != uid {
		f.Close()
		return nil, 0, fmt.Errorf("%s is not a regular file owned by uid %d", path, uid)
	}
	return f, st.Size, nil
}

// openJail opens the jail root dir, resolved under JailerRoot without any symlink.
func openJail(jail string) (*os.File, error) {
	rel, err := jailRel(jail)
	if err != nil {
		return nil, err
	}
	root, err := unix.Open(JailerRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", JailerRoot, err)
	}
	defer unix.Close(root)
	fd, err := unix.Openat2(root, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("opening jail %s: %w", jail, err)
	}
	return os.NewFile(uintptr(fd), jail), nil
}

// attachLoop puts backing behind a free loop device, which detaches itself on its last close.
func attachLoop(backing *os.File, label string) (*os.File, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer ctl.Close()
	cfg := unix.LoopConfig{Fd: uint32(backing.Fd()), Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_AUTOCLEAR}}
	copy(cfg.Info.File_name[:unix.LO_NAME_SIZE-1], label)
	for range 16 {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, fmt.Errorf("finding a free loop device: %w", err)
		}
		dev, err := os.OpenFile("/dev/loop"+strconv.Itoa(n), os.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		err = unix.IoctlLoopConfigure(int(dev.Fd()), &cfg)
		if errors.Is(err, unix.EBUSY) { // taken by someone else in between
			dev.Close()
			continue
		}
		if err != nil {
			dev.Close()
			return nil, fmt.Errorf("setting up %s: %w", dev.Name(), err)
		}
		return dev, nil
	}
	return nil, errors.New("no free loop device after 16 tries")
}

// loadDMSnapshot makes sure the kernel has the snapshot target.
func loadDMSnapshot() error {
	if _, err := os.Stat("/sys/module/dm_snapshot"); err == nil {
		return nil
	}
	if out, err := exec.Command("modprobe", "dm_snapshot").CombinedOutput(); err != nil {
		return fmt.Errorf("loading dm_snapshot: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dmDevice is dm device name's device number.
func dmDevice(name string) (uint64, error) {
	out, err := exec.Command(dmsetup, "info", "-c", "--noheadings", "-o", "major,minor", "--separator", ":", name).Output()
	if err != nil {
		return 0, fmt.Errorf("dmsetup info %s: %w", name, err)
	}
	return parseDevNum(string(out))
}

func parseDevNum(s string) (uint64, error) {
	maj, min, ok := strings.Cut(strings.TrimSpace(s), ":")
	ma, err1 := strconv.ParseUint(strings.TrimSpace(maj), 10, 32)
	mi, err2 := strconv.ParseUint(strings.TrimSpace(min), 10, 32)
	if !ok || err1 != nil || err2 != nil {
		return 0, fmt.Errorf("unexpected device number %q", s)
	}
	return unix.Mkdev(uint32(ma), uint32(mi)), nil
}

// dmExists reports whether a dm device named name is set up, from sysfs.
func dmExists(name string) bool {
	matches, _ := filepath.Glob("/sys/block/dm-*/dm/name")
	for _, m := range matches {
		if data, err := os.ReadFile(m); err == nil && strings.TrimSpace(string(data)) == name {
			return true
		}
	}
	return false
}
