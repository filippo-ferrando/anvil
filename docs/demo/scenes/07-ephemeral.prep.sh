# Not recorded: builds the warm snapshots 07-ephemeral.sh restores, so the cast shows warm starts.
for d in ubuntu-24.04 debian-13 alpine-3.24.1; do ${ANVIL:-anvil} warm create "$d"; done
