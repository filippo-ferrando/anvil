#!/usr/bin/env bash
# Finds the latest stable release of every binary bundled into anvild and rewrites
# scripts/tools.versions with it. Review the diff, then rebuild with
# `make gui-tools firecracker` before committing.
#
# Usage: scripts/update-tools.sh
set -euo pipefail

file="$(dirname "${BASH_SOURCE[0]}")/tools.versions"

# Highest vX.Y.Z tag of a git repo; release candidates and -dev tags are skipped.
latest_tag() {
  git ls-remote --tags --refs "$1" | sed -n 's,.*refs/tags/\(v[0-9]*\.[0-9]*\.[0-9]*\)$,\1,p' | sort -V | tail -n 1
}

# Highest X.Y.Z of an xorg library tarball.
latest_xorg() {
  curl -fsSL https://xorg.freedesktop.org/releases/individual/lib/ |
    sed -n "s/.*$1-\([0-9]*\.[0-9]*\.[0-9]*\)\.tar\.xz.*/\1/p" | sort -uV | tail -n 1
}

fc_sha256() {
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/$1/firecracker-$1-$2.tgz.sha256.txt" |
    cut -d ' ' -f 1
}

fc="$(latest_tag https://github.com/firecracker-microvm/firecracker.git)"
declare -A new=(
  [WAYPIPE_VERSION]="$(latest_tag https://gitlab.freedesktop.org/mstoeckl/waypipe.git)"
  [SATELLITE_VERSION]="$(latest_tag https://github.com/Supreeeme/xwayland-satellite.git)"
  [LIBXAU_VERSION]="$(latest_xorg libXau)"
  [XCB_UTIL_VERSION]="$(latest_xorg xcb-util)"
  [FIRECRACKER_VERSION]="$fc"
  [FIRECRACKER_SHA256_X86_64]="$(fc_sha256 "$fc" x86_64)"
  [FIRECRACKER_SHA256_AARCH64]="$(fc_sha256 "$fc" aarch64)"
)

# Nothing is written unless every lookup worked.
for key in "${!new[@]}"; do
  if [ -z "${new[$key]}" ]; then
    echo "update-tools: no value found for $key" >&2
    exit 1
  fi
done

for key in "${!new[@]}"; do
  old="$(sed -n "s/^$key=//p" "$file")"
  if [ "$old" != "${new[$key]}" ]; then
    echo "$key: ${old:-none} -> ${new[$key]}"
    sed -i "s/^$key=.*/$key=${new[$key]}/" "$file"
  fi
done
