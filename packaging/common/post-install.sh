#!/bin/sh

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
