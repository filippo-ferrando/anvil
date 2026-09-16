#!/usr/bin/env bash
set -euo pipefail

# Builds anvil-<version>-<release>.<arch>.rpm and
# anvild-<version>-<release>.<arch>.rpm via rpmbuild. Same two-package
# split as packaging/archlinux/PKGBUILD and packaging/deb, built with
# rpmbuild's own subpackage mechanism (%package -n anvild in anvil.spec.in)
# instead of a separate control file per package.
#
# Usage: packaging/rpm/build.sh [version] [release]
#   version defaults to 0.1.0, release defaults to 1, matching
#   packaging/archlinux/PKGBUILD's pkgver/pkgrel.
#
# Needs: rpmbuild (the rpm-build package on Fedora/RHEL/openSUSE), go,
# git. Output lands in packaging/rpm/ itself (gitignored).
#
# The source tarball is built from the live working tree (via `git
# ls-files`, so it respects .gitignore and skips anything currently
# deleted-but-still-tracked), not `git archive HEAD` — this repo can
# have real uncommitted work in it (new files not yet `git add`ed, local
# edits to tracked ones), and a build from HEAD alone would silently
# ship a stale source tree instead. Same reasoning packaging/deb/build.sh
# follows by just running `go build` straight against this checkout.

version="${1:-0.1.0}"
release="${2:-1}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$root/../.." && pwd)"

for tool in go git rpmbuild; do
  command -v "$tool" >/dev/null || { echo "packaging/rpm/build.sh: needs '$tool' on PATH" >&2; exit 1; }
done

topdir="$(mktemp -d)"
trap 'rm -rf "$topdir"' EXIT
mkdir -p "$topdir"/{SOURCES,SPECS,BUILD,RPMS,SRPMS,BUILDROOT}

echo "==> archiving the working tree as anvil-${version} (tracked + untracked-and-not-gitignored files, skipping anything deleted-but-still-tracked)"
filelist="$topdir/files.lst"
git -C "$repo" ls-files --others --cached --exclude-standard -z \
  | while IFS= read -r -d '' f; do [ -f "$repo/$f" ] && printf '%s\0' "$f"; done \
  > "$filelist"
tar --null -C "$repo" -T "$filelist" \
  --transform "s,^,anvil-${version}/," \
  -czf "$topdir/SOURCES/anvil-${version}.tar.gz"

echo "==> rendering spec"
# %postun's cleanup body needs packaging/common/purge.sh's commands
# inlined verbatim (see anvil.spec.in's own comment on %postun for why
# it can't just call an installed file the way %post does): strip
# purge.sh's shebang, then splice its body in at the @PURGE_BODY@
# marker with sed's `r` (read file) command, and drop the marker line.
# Done as a second sed pass since `r` can't be mixed with -e on one line.
purge_body="$topdir/purge-body.sh"
tail -n +2 "$repo/packaging/common/purge.sh" > "$purge_body"
sed -e "s/@VERSION@/$version/g" -e "s/@RELEASE@/$release/g" \
    -e "s/@CHANGELOG_DATE@/$(date +'%a %b %d %Y')/" \
    "$root/anvil.spec.in" \
  | sed "/^@PURGE_BODY@\$/{
r $purge_body
d
}" > "$topdir/SPECS/anvil.spec"

echo "==> rpmbuild"
# --nodeps: rpmbuild checks BuildRequires/Requires against the *local*
# rpm package database, which doesn't exist at all on a non-RPM host
# (this script's own CI usage builds on Ubuntu, which has no rpm db to
# check against, regardless of whether e.g. Go is actually installed —
# it came from actions/setup-go, not an rpm-tracked package). This only
# skips rpmbuild's own local check on this host; the Requires:/
# BuildRequires: lines are still baked into the resulting .rpm's
# metadata and get properly enforced by dnf/yum/zypper wherever it's
# actually installed later.
rpmbuild --define "_topdir $topdir" --nodeps -bb "$topdir/SPECS/anvil.spec"

echo "==> done:"
find "$topdir/RPMS" -name '*.rpm' | while IFS= read -r f; do
  cp "$f" "$root/"
  echo "    $root/$(basename "$f")"
done

echo "==> sanity-check before shipping, e.g.:"
echo "    rpm -qip $root/anvild-${version}-${release}*.rpm"
echo "    rpmlint $root/anvil-${version}-${release}*.rpm $root/anvild-${version}-${release}*.rpm"
