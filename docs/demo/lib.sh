# Sourced by every scene: fake typing, then run the command for real.
ANVIL=${ANVIL:-anvil}
p() { printf '\033[1;32m$\033[0m '; printf '%s' "$*" | while IFS= read -r -n1 c; do printf '%s' "$c"; sleep 0.03; done; sleep 0.4; echo; }
# A failed command ends the scene, so later steps don't act on a broken state (deleting
# an intent whose export failed); record.sh reads $DEMO_FAILED to skip the GIF and stop.
run() {
	p "$*"
	if ! eval "$*"; then
		[ -z "${DEMO_FAILED:-}" ] || echo "$*" >"$DEMO_FAILED"
		exit 1
	fi
	sleep "${PAUSE:-1.5}"
}
say() { printf '\033[2m# %s\033[0m\n' "$*"; sleep 0.8; }
