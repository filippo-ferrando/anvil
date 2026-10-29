#!/bin/sh
# Run as root by anvild.service before each start. Gives anvild a private copy of
# Firecracker's jailer that holds the jailer's capabilities as file capabilities, runnable
# only by the anvil user. Without jailer or setcap there's no copy and anvild runs
# firecracker unjailed. Redone every start, so a firecracker upgrade is picked up.
# The static jailer the package ships comes first, one on PATH second.

dir=/var/lib/anvil-jailer
rm -rf "$dir"
src=/usr/lib/anvil/firecracker/jailer
[ -x "$src" ] || src=$(command -v jailer) || exit 0
if ! command -v setcap >/dev/null 2>&1; then
  echo "anvild: setcap not found (libcap), firecracker VMs run without the jailer" >&2
  exit 0
fi
install -d -m 0755 -o root -g root "$dir"
install -m 0500 -o anvil -g root "$src" "$dir/jailer"
if ! setcap cap_sys_admin,cap_sys_chroot,cap_mknod,cap_chown,cap_setuid,cap_setgid,cap_dac_override+ep "$dir/jailer"; then
  echo "anvild: setcap on $dir/jailer failed, firecracker VMs run without the jailer" >&2
  rm -rf "$dir"
fi
exit 0
