#!/bin/sh
# Shared full-removal cleanup for the anvild package: stops the service,
# deletes every directory anvild ever writes to (per
# internal/config/paths.go: RunDir, StateDir, CacheDir — cached base
# images, per-instance state, the bbolt registry, the saved cloud-init
# library, the migration SSH keypair, everything), and removes the
# "anvil" system user/group post-install.sh created. This is the one
# canonical copy of the logic, but *how* each format reaches it differs,
# because a package manager only tells its removal hook "this is a full
# purge, not just a remove" at a point where that format has already
# deleted the package's own regular files — including this one, if it
# were shipped as a plain installed file everywhere:
#   - packaging/archlinux: installed as a real file
#     (/usr/lib/anvil/purge.sh) and called from anvild.install's
#     pre_remove, which runs *before* pacman deletes the package's own
#     files — pacman has no separate "keep data" removal mode anyway, so
#     every `pacman -R anvild` is already a full removal.
#   - packaging/deb: NOT installed as a file — dpkg only distinguishes
#     `remove` (keep data, the default, matching Debian policy) from
#     `purge` inside postrm, which runs *after* dpkg has already deleted
#     the package's regular files. So packaging/deb/build.sh inlines this
#     script's body directly into DEBIAN/postrm's `purge)` case at build
#     time instead.
#   - packaging/rpm: same reasoning, not installed as a file — %postun
#     (guarded by `[ "$1" = 0 ]`, a real removal rather than an
#     in-place upgrade) also runs after rpm has removed the old
#     package's files, so packaging/rpm/build.sh inlines this script's
#     body into anvil.spec.in's %postun at build time too.
#
# This is destructive on purpose — every VM, container record, cached
# image, and saved cloud-init config anvild knows about is gone
# afterward, with no undo. That's what "delete removes everything"
# means; if you want to keep the data, don't purge, just remove/upgrade.
set -e

systemctl stop anvild 2>/dev/null || true
systemctl disable anvild 2>/dev/null || true

rm -rf /run/anvil /var/lib/anvil /var/cache/anvil /etc/anvil

if getent passwd anvil >/dev/null 2>&1; then
  userdel anvil 2>/dev/null || true
fi
if getent group anvil >/dev/null 2>&1; then
  groupdel anvil 2>/dev/null || true
fi

echo "==> anvild removed: /run/anvil, /var/lib/anvil, /var/cache/anvil, and the"
echo "    anvil system user/group are gone. This included every VM/container"
echo "    record, cached image, and saved cloud-init config anvild knew about."
