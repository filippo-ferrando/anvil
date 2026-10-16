#!/usr/bin/env bash
# Prints CHANGELOG.md for every v* tag, newest first, from release-notes.sh.
#
# Usage: scripts/changelog.sh > CHANGELOG.md
set -euo pipefail

cd "$(dirname "$0")/.."

# cat -s squeezes the blank lines left between release-notes.sh sections.
{
  printf '# Changelog\n\nBuilt from commit subjects by `scripts/changelog.sh`.\n'
  for tag in $(git tag --list 'v*' --sort=-v:refname); do
    printf '\n## %s (%s)\n\n' "$tag" "$(git log -1 --format=%cs "$tag")"
    NOTES_NO_INSTALL=1 scripts/release-notes.sh "$tag"
  done
} | cat -s
