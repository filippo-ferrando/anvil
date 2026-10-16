#!/usr/bin/env bash
# Record docs/demo/scenes/*.sh with asciinema, then render GIFs with cast2gif.py.
# Usage: docs/demo/record.sh [scene...]   e.g. docs/demo/record.sh 02-snapshot
set -euo pipefail
cd "$(dirname "$0")/../.."
mkdir -p docs/demo/casts docs/demo/gifs
scenes=("$@"); [ ${#scenes[@]} -gt 0 ] || scenes=(01-launch 02-snapshot 03-fork 04-intent 05-export)
# asciinema 3 writes asciicast v3 by default; v2 is what most web players read.
fmt=(); asciinema --version | grep -q '^asciinema 3' && fmt=(--output-format asciicast-v2)
for s in "${scenes[@]}"; do
	asciinema rec "${fmt[@]}" --overwrite -q -i 2 -c "bash docs/demo/scenes/$s.sh" "docs/demo/casts/$s.cast"
	python3 docs/demo/cast2gif.py "docs/demo/casts/$s.cast" "docs/demo/gifs/$s.gif"
done
