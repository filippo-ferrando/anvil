package hostdev

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
)

// The files of a clone disk. The helper derives every path from ids, never takes one.
const (
	CloneDiskFile = "disk.raw" // the warm disk, and the block device put in the jail
	CloneCOWFile  = "cow.raw"  // the clone's sparse copy-on-write store, in its instance dir
)

// Where the helper looks for clone disk files; tests point them elsewhere.
var (
	WarmRoot     = filepath.Join(config.StateDir, "warm")
	InstanceRoot = filepath.Join(config.StateDir, "instances")
	JailerRoot   = config.JailerDir()
)

var warmKeyRe = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// CloneDisk asks for the disk of one clone: a device-mapper snapshot with the warm disk as
// its read-only origin and the clone's COW file as its store, put in the jail as a block device.
type CloneDisk struct {
	ID   string // the clone
	Key  string // its warm snapshot
	Jail string // the clone's jail root, under JailerRoot
}

// DMName is the device-mapper name of clone id's disk.
func DMName(id string) string { return "anvil-" + id }

func (d CloneDisk) validate() error {
	if !cloneIDRe.MatchString(d.ID) {
		return fmt.Errorf("invalid clone id %q", d.ID)
	}
	if !warmKeyRe.MatchString(d.Key) {
		return fmt.Errorf("invalid warm key %q", d.Key)
	}
	if _, err := jailRel(d.Jail); err != nil {
		return err
	}
	return nil
}

// jailRel is jail relative to JailerRoot, refused when it isn't strictly under it.
func jailRel(jail string) (string, error) {
	rel, err := filepath.Rel(JailerRoot, filepath.Clean(jail))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("jail %q is not under %s", jail, JailerRoot)
	}
	return rel, nil
}

// CreateCloneDisk builds clone d's disk and puts it in its jail as CloneDiskFile. A disk
// left by an earlier run under the same id is replaced.
func (c Client) CreateCloneDisk(ctx context.Context, d CloneDisk) error {
	_, err := c.call(ctx, request{Op: "disk-create", Disk: &d})
	return err
}

// DeleteCloneDisk removes clone id's disk once its firecracker is gone; a missing one is
// not an error.
func (c Client) DeleteCloneDisk(ctx context.Context, id string) error {
	_, err := c.call(ctx, request{Op: "disk-delete", Disk: &CloneDisk{ID: id}})
	return err
}
