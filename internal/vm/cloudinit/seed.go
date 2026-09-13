// Package cloudinit builds NoCloud cloud-init seed images (user-data,
// meta-data, optional network-config) that get attached to a VM as a
// virtio-blk drive.
package cloudinit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Seed is the set of files a NoCloud datasource looks for on the seed
// volume.
type Seed struct {
	UserData      string
	MetaData      string
	NetworkConfig string // optional; empty means omit network-config entirely
}

// Builder writes a Seed out as a disk image QEMU can attach with
// `-drive if=virtio,format=raw,readonly=on`. There are two implementations:
// xorrisoBuilder (shells out, no extra Go dependency, available today) and
// a planned pure-Go ISO9660 writer (github.com/kdomanski/iso9660) that
// removes the xorriso/genisoimage runtime dependency once network access
// allows fetching it — swapping the default returned by NewBuilder is the
// only change needed, callers depend on this interface, not a concrete type.
type Builder interface {
	// Build writes an ISO9660 image containing seed's files to outputPath.
	Build(seed Seed, outputPath string) error
}

// NewBuilder returns anvil's current default Builder. Today that's
// xorrisoBuilder; see the Builder doc comment for the pure-Go follow-up.
func NewBuilder() Builder { return xorrisoBuilder{} }

type xorrisoBuilder struct{}

// Build shells out to `xorriso -as genisoimage` (Arch's officially-packaged
// equivalent of genisoimage/mkisofs — see the Arch packaging plan) to
// produce a volume labeled "cidata", the label cloud-init's NoCloud
// datasource requires.
func (xorrisoBuilder) Build(seed Seed, outputPath string) error {
	if seed.UserData == "" {
		return fmt.Errorf("cloudinit: seed.UserData must not be empty")
	}
	if seed.MetaData == "" {
		// cloud-init tolerates an empty meta-data file, but an explicitly
		// empty string here almost always indicates a caller bug (missing
		// instance-id), so fail fast rather than writing a broken seed.
		return fmt.Errorf("cloudinit: seed.MetaData must not be empty")
	}

	stageDir, err := os.MkdirTemp("", "anvil-cloudinit-seed-*")
	if err != nil {
		return fmt.Errorf("cloudinit: creating stage dir: %w", err)
	}
	defer os.RemoveAll(stageDir)

	files := map[string]string{
		"user-data": seed.UserData,
		"meta-data": seed.MetaData,
	}
	if seed.NetworkConfig != "" {
		files["network-config"] = seed.NetworkConfig
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(stageDir, name), []byte(content), 0o640); err != nil {
			return fmt.Errorf("cloudinit: writing %s: %w", name, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o750); err != nil {
		return fmt.Errorf("cloudinit: creating output dir: %w", err)
	}

	cmd := exec.Command("xorriso", "-as", "genisoimage",
		"-output", outputPath,
		"-volid", "cidata",
		"-joliet", "-rock",
		stageDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cloudinit: xorriso failed: %w: %s", err, string(out))
	}
	return nil
}
