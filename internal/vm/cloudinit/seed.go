// Package cloudinit builds NoCloud cloud-init seed images attached to a
// VM as a virtio-blk drive.
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

// Builder writes a Seed out as a disk image QEMU can attach as a drive.
type Builder interface {
	// Build writes an ISO9660 image containing seed's files to outputPath.
	Build(seed Seed, outputPath string) error
}

// NewBuilder returns anvil's default Builder.
func NewBuilder() Builder { return xorrisoBuilder{} }

type xorrisoBuilder struct{}

// Build shells out to `xorriso -as genisoimage` to produce a volume
// labeled "cidata", as cloud-init's NoCloud datasource requires.
func (xorrisoBuilder) Build(seed Seed, outputPath string) error {
	if seed.UserData == "" {
		return fmt.Errorf("cloudinit: seed.UserData must not be empty")
	}
	if seed.MetaData == "" {
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
