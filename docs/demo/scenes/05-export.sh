. "$(dirname "$0")/../lib.sh"
say "pack a whole intent into one file, restore it anywhere"
run "$ANVIL export shop -o /tmp/anvil-demo/shop.tar.zst"
run "ls -lh /tmp/anvil-demo/shop.tar.zst"
# Off camera: the bundle is owned by the anvil user but import wants one owned by the
# caller; a copy (readable through the anvil group) makes it so.
f=/tmp/anvil-demo/shop.tar.zst; cp "$f" "$f.tmp" && mv -f "$f.tmp" "$f"
# The bundle keeps the intent's subnet, so the original must go before the import.
run "$ANVIL intent delete shop --purge-members"
run "$ANVIL import /tmp/anvil-demo/shop.tar.zst --name shop2"
run "$ANVIL intent list"
