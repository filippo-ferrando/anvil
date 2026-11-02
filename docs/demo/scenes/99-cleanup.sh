# Not recorded: removes what the scenes created.
# Plain delete keeps the cached base image, so a re-recording needs no download.
# Warm snapshots stay too, for the same reason: `anvil warm rm` removes them.
for n in box box-copy web micro; do ${ANVIL:-anvil} delete "$n"; done
for i in shop shop2; do ${ANVIL:-anvil} intent delete "$i" --purge-members; done
rm -rf /tmp/anvil-demo
