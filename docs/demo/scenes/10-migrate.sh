. "$(dirname "$0")/../lib.sh"
# Needs a second host with anvild; set TARGET=user@host.
say "move an instance to another host over plain SSH"
run "$ANVIL host discover"
run "$ANVIL migrate box --to ${TARGET:?set TARGET=user@host} --dry-run"
run "$ANVIL migrate box --to $TARGET"
run "$ANVIL --remote $TARGET list"
