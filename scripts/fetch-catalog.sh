#!/usr/bin/env bash
# Pulls the VM image catalog from the anvil/catalog repo into data/distros, where
# data/distros/distros.go embeds it. Run before any build, test or vet.
#
# Usage: scripts/fetch-catalog.sh [dest]
#   dest           defaults to data/distros next to this script
#   CATALOG_URL    git repo to clone (default https://git.fferrando.cc/anvil/catalog.git)
#   CATALOG_REF    branch or tag to check out (default main)
#   CATALOG_SRC    an existing local checkout to copy from instead of cloning
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dest="${1:-$repo/data/distros}"
url="${CATALOG_URL:-https://git.fferrando.cc/anvil/catalog.git}"
ref="${CATALOG_REF:-main}"

src="${CATALOG_SRC:-}"
if [ -z "$src" ]; then
  command -v git >/dev/null || { echo "fetch-catalog.sh: needs git on PATH" >&2; exit 1; }
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' EXIT
  echo "==> fetching catalog $url ($ref)"
  git clone --quiet --depth 1 --branch "$ref" "$url" "$work/catalog"
  src="$work/catalog"
fi

shopt -s nullglob
files=("$src"/distros_*.json "$src"/firecracker_*.json)
if [ ${#files[@]} -eq 0 ]; then
  echo "fetch-catalog.sh: no distros_*.json or firecracker_*.json in $src" >&2
  exit 1
fi

mkdir -p "$dest"
# Drop manifests from an older fetch, so an arch removed upstream doesn't linger.
rm -f "$dest"/distros_*.json "$dest"/firecracker_*.json
cp "${files[@]}" "$dest"/
echo "==> catalog: ${#files[@]} manifests in $dest"
