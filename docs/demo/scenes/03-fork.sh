. "$(dirname "$0")/../lib.sh"
say "clone a running VM, the source stays up"
run "$ANVIL fork box box-copy"
run "$ANVIL list"
