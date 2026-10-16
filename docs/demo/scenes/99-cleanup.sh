# Not recorded: removes what the scenes created.
# Plain delete keeps the cached base image, so a re-recording needs no download.
for n in box box-copy web; do ${ANVIL:-anvil} delete "$n"; done
for i in shop shop2; do ${ANVIL:-anvil} intent delete "$i" --purge-members; done
rm -f /tmp/shop.tar.zst
