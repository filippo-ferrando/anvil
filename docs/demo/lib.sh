# Sourced by every scene: fake typing, then run the command for real.
ANVIL=${ANVIL:-anvil}
p() { printf '\033[1;32m$\033[0m '; printf '%s' "$*" | while IFS= read -r -n1 c; do printf '%s' "$c"; sleep 0.03; done; sleep 0.4; echo; }
run() { p "$*"; eval "$*"; sleep "${PAUSE:-1.5}"; }
say() { printf '\033[2m# %s\033[0m\n' "$*"; sleep 0.8; }
