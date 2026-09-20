// Package hostpath computes and grants the filesystem access the "anvil"
// system user needs to a host directory.
package hostpath

import (
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// AnvilUser is the dedicated system user anvild runs as.
const AnvilUser = "anvil"

// lookupUser is user.Lookup, indirected for testing.
var lookupUser = user.Lookup

// Ancestors returns path's parent directories, root-to-leaf, excluding
// path itself and "/".
func Ancestors(path string) []string {
	clean := filepath.Clean(path)
	var dirs []string
	for dir := filepath.Dir(clean); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		dirs = append([]string{dir}, dirs...)
	}
	return dirs
}

// Hint builds copy-pasteable setfacl commands granting AnvilUser
// traverse access on path's ancestors and full rwx on path itself.
func Hint(path string) string {
	clean := filepath.Clean(path)
	var b strings.Builder
	b.WriteString("\nthe \"anvil\" system user needs real access to this path, not just you")
	b.WriteString(". Try:\n")
	for _, dir := range Ancestors(clean) {
		fmt.Fprintf(&b, "  setfacl -m u:%s:x %s\n", AnvilUser, dir)
	}
	fmt.Fprintf(&b, "  setfacl -R -m u:%s:rwx %s\n", AnvilUser, clean)
	fmt.Fprintf(&b, "  setfacl -R -d -m u:%s:rwx %s", AnvilUser, clean)
	return b.String()
}

// Grant runs the setfacl commands Hint describes. It no-ops if the
// "anvil" system user doesn't exist on this machine.
func Grant(path string) error {
	if _, err := lookupUser(AnvilUser); err != nil {
		return nil
	}
	setfaclBin, err := exec.LookPath("setfacl")
	if err != nil {
		return fmt.Errorf("hostpath: setfacl not found on PATH (needed to grant the %q user access)", AnvilUser)
	}

	clean := filepath.Clean(path)
	run := func(args ...string) error {
		out, err := exec.Command(setfaclBin, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("hostpath: setfacl %s: %w: %s", strings.Join(args, " "), err, out)
		}
		return nil
	}
	for _, dir := range Ancestors(clean) {
		if err := run("-m", fmt.Sprintf("u:%s:x", AnvilUser), dir); err != nil {
			return err
		}
	}
	if err := run("-R", "-m", fmt.Sprintf("u:%s:rwx", AnvilUser), clean); err != nil {
		return err
	}
	return run("-R", "-d", "-m", fmt.Sprintf("u:%s:rwx", AnvilUser), clean)
}
