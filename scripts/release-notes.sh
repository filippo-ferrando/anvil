#!/usr/bin/env bash
# Prints markdown release notes for a tag, built from conventional commit
# subjects since the previous tag.
#
# Usage: scripts/release-notes.sh <tag>   (e.g. v1.0.11, defaults to HEAD)
set -euo pipefail

tag="${1:-HEAD}"
prev="$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)"
range="${prev:+$prev..}$tag"
version="${tag#v}"

subjects="$(git log --no-merges --format='%s (%h)' "$range")"

# section <title> <regex> [-v]: lists commits matching (or with -v, not matching) the regex.
section() {
  local lines
  lines="$(grep -E ${3:-} "$2" <<<"$subjects" | sed -E 's/^[a-z]+(\([^)]*\))?!?: *//; s/^/- /' || true)"
  [[ -n "$lines" ]] && printf '### %s\n\n%s\n\n' "$1" "$lines"
  return 0
}

section "Breaking changes" '^[a-z]+(\([^)]*\))?!:'
section "Features" '^feat(\([^)]*\))?:'
section "Fixes" '^fix(\([^)]*\))?:'
section "Other changes" '^(chore|refactor|docs|perf|test|build|ci)(\([^)]*\))?:'
section "Uncategorized" '^(feat|fix|chore|refactor|docs|perf|test|build|ci)(\([^)]*\))?!?:' -v

cat <<EOF
### Install

- Arch: \`sudo pacman -U anvil-${version}-*.pkg.tar.zst anvild-${version}-*.pkg.tar.zst\`
- Debian/Ubuntu: \`sudo apt install ./anvil_${version}_*.deb ./anvild_${version}_*.deb\`
- Fedora/RHEL: \`sudo dnf install ./anvil-${version}-*.rpm ./anvild-${version}-*.rpm\`

Then add the user to the \`anvil\` group and enable the daemon: \`sudo systemctl enable --now anvild\`.
EOF

[[ -n "$prev" ]] && printf '\n**Full changelog:** `%s...%s`\n' "$prev" "$tag"
exit 0
