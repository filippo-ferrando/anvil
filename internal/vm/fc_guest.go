//go:build linux

package vm

import (
	"fmt"
	"os"
	"path/filepath"

	"git.fferrando.cc/rdfilippo/anvil/internal/config"
	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
)

// guestTag labels the anvil-guest drive; the guest finds it by this label.
const guestTag = "ANVILGUEST"

const guestBinary = "anvil-guest"

// guestISO packs anvil-guest into a read-only ISO drive, like the GUI tools.
func (b *FCBackend) guestISO() (string, error) {
	dir := config.GuestToolsDir()
	if _, err := os.Stat(filepath.Join(dir, guestBinary)); err != nil {
		return "", fmt.Errorf("vm: %s isn't installed in %s (ANVIL_GUEST_TOOLS overrides it): %w", guestBinary, dir, err)
	}
	return b.packISO(dir, guestTag, guestBinary+".iso")
}

var anvilGuestUnit = fmt.Sprintf(`[Unit]
Description=anvil guest command runner on vsock

[Service]
ExecStart=/usr/local/bin/anvil-guest --port %d
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`, guestexec.Port)

// anvilGuestOpenRC is the same service for OpenRC guests (Alpine).
var anvilGuestOpenRC = fmt.Sprintf(`#!/sbin/openrc-run
description="anvil guest command runner on vsock"
command=/usr/local/bin/anvil-guest
command_args="--port %d"
command_background=true
pidfile=/run/anvil-guest.pid
`, guestexec.Port)

// anvilGuestOpenRCPath keeps the OpenRC script out of /etc/init.d until the guest is known
// to use OpenRC: on a systemd guest the SysV compatibility would pick it up and fail.
const anvilGuestOpenRCPath = "/usr/local/share/anvil/anvil-guest.openrc"

// anvilGuestInstallCmd copies anvil-guest off its drive, then starts it with systemd or
// OpenRC. Its output goes to the serial console, where `anvil logs` shows a failure.
const anvilGuestInstallCmd = `(set -e; d=$(mktemp -d); ` +
	`mount -o ro "$(blkid -L ` + guestTag + ` || findfs LABEL=` + guestTag + `)" "$d"; ` +
	`install -m 0755 "$d/` + guestBinary + `" /usr/local/bin/` + guestBinary + `; umount "$d"; rmdir "$d"; ` +
	`if [ -d /run/systemd/system ]; then systemctl daemon-reload; systemctl enable --now anvil-guest; ` +
	`else install -m 0755 ` + anvilGuestOpenRCPath + ` /etc/init.d/anvil-guest; ` +
	`rc-update add anvil-guest default; rc-service anvil-guest start; fi) >/dev/console 2>&1`

// addAnvilGuest installs and starts anvil-guest from its drive.
func addAnvilGuest(doc map[string]any) {
	files, _ := doc["write_files"].([]any)
	doc["write_files"] = append(files,
		map[string]any{"path": "/etc/systemd/system/anvil-guest.service", "content": anvilGuestUnit},
		map[string]any{"path": anvilGuestOpenRCPath, "content": anvilGuestOpenRC},
	)
	runcmd, _ := doc["runcmd"].([]any)
	doc["runcmd"] = append(runcmd, anvilGuestInstallCmd)
}
