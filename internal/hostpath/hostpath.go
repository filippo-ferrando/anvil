// Package hostpath figures out, and optionally grants, the filesystem
// access the "anvil" system user needs to a host directory — used by
// both `anvil mount`'s permission-denied error (vm.Backend.Mount) and
// `anvil create-dir` (which actually runs the commands instead of just
// printing them), so the two stay consistent. Pure stdlib: no anvil
// package depends on this one, so the CLI can call it directly without
// pulling in the daemon-side packages' transitive dependencies.
package hostpath

import (
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// AnvilUser is the dedicated system user anvild runs as (see
// packaging/anvild.service) — the account that actually needs
// filesystem access for a 9p mount, not whoever runs the CLI.
const AnvilUser = "anvil"

// lookupUser is user.Lookup, indirected so tests can force the
// "anvil user doesn't exist" path regardless of what's actually on the
// machine running the tests.
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

// Hint builds copy-pasteable setfacl commands granting AnvilUser access
// to path: traverse-only (x) on every ancestor directory, full rwx plus
// a default ACL (so new files created later inherit it) on path itself.
// stat(2) failing with EACCES specifically means some ancestor lacks
// search permission, not the target itself, so this covers every
// ancestor rather than guessing which one is the actual blocker —
// granting one that's already fine is harmless.
func Hint(path string) string {
	clean := filepath.Clean(path)
	var b strings.Builder
	b.WriteString("\nthe \"anvil\" system user needs real access to this path, not just you")
	b.WriteString(" (anvild doesn't run as root, see PLAN.md's M6 notes) — try:\n")
	for _, dir := range Ancestors(clean) {
		fmt.Fprintf(&b, "  setfacl -m u:%s:x %s\n", AnvilUser, dir)
	}
	fmt.Fprintf(&b, "  setfacl -R -m u:%s:rwx %s\n", AnvilUser, clean)
	fmt.Fprintf(&b, "  setfacl -R -d -m u:%s:rwx %s", AnvilUser, clean)
	return b.String()
}

// Grant actually runs the setfacl commands Hint describes, so a caller
// doesn't have to copy-paste them by hand. If the "anvil" system user
// doesn't exist on this machine, there's nothing to grant — that's the
// normal shape of a manual dev setup running anvild as root directly
// (see PLAN.md's M6 notes), which needs no ACLs at all.
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
