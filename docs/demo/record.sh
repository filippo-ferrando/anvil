#!/usr/bin/env bash
# Record docs/demo/scenes/*.sh with asciinema, then render GIFs with cast2gif.py.
# Usage: docs/demo/record.sh [scene...]   e.g. docs/demo/record.sh 02-snapshot
set -euo pipefail
cd "$(dirname "$0")/../.."
mkdir -p docs/demo/casts docs/demo/gifs
# Same output on every machine: no localized numbers or dates (0,095s, "3 nov").
export LC_ALL=C.UTF-8
scenes=("$@")
[ ${#scenes[@]} -gt 0 ] || scenes=(01-launch 02-snapshot 03-fork 04-intent 05-export
	06-firecracker 07-ephemeral 08-pipeline 09-events)
# Every cast gets the same size, whatever the terminal is: DEMO_SIZE=COLSxROWS changes it.
size=${DEMO_SIZE:-110x30}; cols=${size%x*}; rows=${size#*x}
# The casts are right on a smaller terminal, but live redraws wrap and pile up on screen.
if [ -t 1 ] && { [ "$(tput cols)" -lt "$cols" ] || [ "$(tput lines)" -lt "$rows" ]; }; then
	echo "note: terminal is $(tput cols)x$(tput lines), smaller than $size: the casts are fine, the live view is garbled" >&2
fi
# asciinema 3 writes asciicast v3 by default; v2 is what most web players read.
if asciinema --version | grep -q '^asciinema 3'; then
	opts=(--output-format asciicast-v2 --window-size "$size")
else
	opts=(--cols "$cols" --rows "$rows")
fi
DEMO_FAILED=$(mktemp); export DEMO_FAILED
trap 'rm -f "$DEMO_FAILED"' EXIT
for s in "${scenes[@]}"; do
	[ -f "docs/demo/scenes/$s.sh" ] || { echo "no scene docs/demo/scenes/$s.sh" >&2; exit 1; }
	# A scene's <name>.prep.sh runs first, off camera (slow setup the cast should not show).
	[ ! -f "docs/demo/scenes/$s.prep.sh" ] || bash "docs/demo/scenes/$s.prep.sh"
	# stty makes the scene itself see that size, so its output wraps like the cast.
	asciinema rec "${opts[@]}" --overwrite -q -i 2 \
		-c "stty cols $cols rows $rows; bash docs/demo/scenes/$s.sh" "docs/demo/casts/$s.cast"
	if [ -s "$DEMO_FAILED" ]; then
		echo "$s: '$(cat "$DEMO_FAILED")' failed, no GIF made; fix it and run: $0 $s" >&2
		exit 1
	fi
	python3 docs/demo/cast2gif.py "docs/demo/casts/$s.cast" "docs/demo/gifs/$s.gif"
done
