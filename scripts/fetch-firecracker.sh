#!/usr/bin/env bash
# Downloads the static firecracker and jailer from the upstream release pinned in
# scripts/tools.versions, checks the sha256, and puts them in build/firecracker/ (or $1).
#
# Usage: scripts/fetch-firecracker.sh [output-dir] [x86_64|aarch64]
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/tools.versions"

out="$(realpath -m "${1:-build/firecracker}")"
arch="${2:-$(uname -m)}"
case "$arch" in
  x86_64) sum="$FIRECRACKER_SHA256_X86_64" ;;
  aarch64) sum="$FIRECRACKER_SHA256_AARCH64" ;;
  *) echo "unsupported arch $arch" >&2; exit 1 ;;
esac
if [ -z "$sum" ]; then
  echo "no sha256 for $arch in scripts/tools.versions, run scripts/update-tools.sh" >&2
  exit 1
fi

v="$FIRECRACKER_VERSION"
tgz="firecracker-$v-$arch.tgz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/$tgz" "https://github.com/firecracker-microvm/firecracker/releases/download/$v/$tgz"
echo "$sum  $work/$tgz" | sha256sum -c --quiet
tar -xzf "$work/$tgz" -C "$work"

mkdir -p "$out"
for bin in firecracker jailer; do
  install -m 0755 "$work/release-$v-$arch/$bin-$v-$arch" "$out/$bin"
  # A dynamic build can't run in the jailer's chroot (see internal/vm/fc_jail.go).
  if ! file "$out/$bin" | grep -q "static"; then
    echo "$bin is not static" >&2
    exit 1
  fi
done
(cd "$out" && sha256sum firecracker jailer)
