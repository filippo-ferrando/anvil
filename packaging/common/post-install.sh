#!/bin/sh
# Shared post-install steps for the anvild package, run identically on
# every packaging format after anvild's files are unpacked (a fresh
# install or an upgrade, no distinction needed: every step here is
# already idempotent). Installed as part of the anvild package itself
# (at /usr/lib/anvil/post-install.sh) and invoked by each format's own
# tiny maintainer-script hook:
#   - packaging/archlinux/anvild.install's post_install/post_upgrade
#   - packaging/deb/postinst-anvild
#   - packaging/rpm/anvil.spec.in's %post -n anvild
# Edit this one file, every format picks it up on its next build — never
# copy these steps into a format's own hook script directly.
#
# Deliberately does NOT enable/start the systemd service, on any format
# — left to the operator, same as a fresh `systemctl enable --now
# anvild` on any other daemon you've just installed.
set -e

systemd-sysusers anvil.conf
systemd-tmpfiles --create anvil.conf

# Fix ownership left behind by an older build that ran anvild as root.
chown -R anvil:anvil /var/lib/anvil /var/cache/anvil 2>/dev/null || true

if [ ! -f /var/lib/anvil/.ssh/id_ed25519 ]; then
  install -d -m 700 /var/lib/anvil/.ssh
  ssh-keygen -t ed25519 -N "" -C anvil-migrate -f /var/lib/anvil/.ssh/id_ed25519 -q
  chown -R anvil:anvil /var/lib/anvil/.ssh
  chmod 700 /var/lib/anvil/.ssh
  chmod 600 /var/lib/anvil/.ssh/id_ed25519
  chmod 644 /var/lib/anvil/.ssh/id_ed25519.pub

  echo "==> Generated a passwordless SSH key for \`anvil migrate\` at:"
  echo "        /var/lib/anvil/.ssh/id_ed25519.pub"
  echo "==> Copy that public key into ~/.ssh/authorized_keys on any host you plan to"
  echo "    migrate to before using it. Adding a host with \`anvil host add\` does not"
  echo "    grant trust by itself, this is the actual key anvild connects with."
fi

if getent group docker >/dev/null 2>&1; then
  gpasswd -a anvil docker >/dev/null
else
  echo "==> Docker is not installed yet, so the anvil user was not added to the"
  echo "    docker group. Once you install docker, run:"
  echo "        gpasswd -a anvil docker && systemctl restart anvild"
fi

echo "==> anvild runs as the unprivileged anvil system user, not root."
echo "==> \`anvil mount\` needs the anvil user to have access to whatever"
echo "    host directory you share."
