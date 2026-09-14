#!/usr/bin/env bash
# Downloads every entry in data/distros/distribution-info.json, verifies
# the URL resolves to a real image, and prints its sha256.
#
# Usage: scripts/verify-catalog.sh [--keep]
#   --keep   don't delete the downloaded images afterward (default: delete)
set -euo pipefail

manifest="data/distros/distribution-info.json"
keep=false
[[ "${1:-}" == "--keep" ]] && keep=true

# A real cloud image is realistically never smaller than this; an HTML
# directory listing almost always is.
min_size_bytes=$((50 * 1024 * 1024))

if ! command -v jq >/dev/null; then
  echo "this script needs jq (pacman -S jq)" >&2
  exit 1
fi

workdir=$(mktemp -d)
trap '[[ "$keep" == false ]] && rm -rf "$workdir"' EXIT

count=$(jq '.distros | length' "$manifest")
echo "checking $count entries from $manifest"
echo

fail=0
for i in $(seq 0 $((count - 1))); do
  id=$(jq -r ".distros[$i].id" "$manifest")
  url=$(jq -r ".distros[$i].url" "$manifest")
  dest="$workdir/$id"
  headers="$workdir/$id.headers"

  echo "== $id =="
  echo "url: $url"

  status=$(curl -sL -o "$dest" -D "$headers" -w '%{http_code}' "$url" || echo "000")
  if [[ "$status" != "200" ]]; then
    echo "FAIL: got HTTP $status"
    fail=1
    echo
    continue
  fi

  content_type=$(grep -i '^content-type:' "$headers" | tail -1 | cut -d: -f2- | tr -d '\r' | xargs || true)
  size=$(stat -c%s "$dest" 2>/dev/null || stat -f%z "$dest")

  if [[ "$content_type" == text/html* ]]; then
    echo "FAIL: got HTTP 200 but Content-Type is $content_type — this URL is almost"
    echo "      certainly a directory listing page, not a direct file. Find the exact"
    echo "      filename and point the manifest at that instead."
    fail=1
    echo
    continue
  fi
  if [[ "$size" -lt "$min_size_bytes" ]]; then
    echo "FAIL: only got ${size} bytes, way too small for a real cloud image"
    echo "      (got Content-Type: ${content_type:-<none>}) — same likely cause as above."
    fail=1
    echo
    continue
  fi

  sha=$(sha256sum "$dest" | cut -d' ' -f1)
  echo "OK ($size bytes, $content_type), sha256: $sha"
  echo "-> paste this into distribution-info.json's \"$id\" entry's \"sha256\" field"
  [[ "$keep" == false ]] && rm -f "$dest"
  echo
done

if [[ "$fail" != 0 ]]; then
  echo "one or more entries failed, fix the URL in $manifest and rerun" >&2
  exit 1
fi
echo "all entries resolved"
