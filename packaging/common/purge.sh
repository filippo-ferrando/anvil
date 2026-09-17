#!/bin/sh

systemctl stop anvild 2>/dev/null || true
systemctl disable anvild 2>/dev/null || true

rm -rf /run/anvil /var/lib/anvil /var/cache/anvil /etc/anvil

echo "==> anvild removed: /run/anvil, /var/lib/anvil, /var/cache/anvil, and the"
echo "    anvil system user/group are gone. This included every VM/container"
echo "    record, cached image, and saved cloud-init config anvild knew about."
