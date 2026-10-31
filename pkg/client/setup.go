package client

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	anvilv1 "git.fferrando.cc/rdfilippo/anvil/api/gen/anvil/v1"
)

// MaxSetupBytes bounds a setup script and its key files, the same as anvild does.
const MaxSetupBytes = 3 << 20

// LoadSetup reads a setup script and its key files, relative to the current directory,
// for a warm snapshot built with them; nil when script is empty.
func LoadSetup(script string, keyFiles []string) (*anvilv1.WarmSetup, error) {
	if script == "" {
		if len(keyFiles) > 0 {
			return nil, fmt.Errorf("--key-files needs --setup")
		}
		return nil, nil
	}
	data, err := os.ReadFile(script)
	if err != nil {
		return nil, fmt.Errorf("reading the setup script: %w", err)
	}
	s := &anvilv1.WarmSetup{Name: filepath.ToSlash(filepath.Clean(script)), Script: data}
	total := len(data)
	for _, f := range keyFiles {
		name := path.Clean(filepath.ToSlash(f))
		if filepath.IsAbs(f) || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("key file %s: want a path inside the current directory", f)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading key file: %w", err)
		}
		total += len(b)
		s.Files = append(s.Files, &anvilv1.SetupFile{Name: name, Data: b})
	}
	if total > MaxSetupBytes {
		return nil, fmt.Errorf("the setup script and key files take %d KiB, more than %d MiB", total>>10, MaxSetupBytes>>20)
	}
	return s, nil
}
