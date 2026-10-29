#!/usr/bin/env bash
# Builds static (musl) waypipe and xwayland-satellite in an Alpine container
# (docker, or the current one when already in Alpine as root),
# for `anvil gui`, at the versions in scripts/tools.versions.
# Output goes to build/gui-tools/ (or $1).
#
# Usage: scripts/build-gui-tools.sh [output-dir]
set -euo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/tools.versions"
# Alpine's own rust links build scripts dynamically, which bindgen needs to load libclang.
build_image="docker.io/library/alpine:3.22"

out="$(realpath -m "${1:-build/gui-tools}")"
mkdir -p "$out"

# The build steps, run as root inside Alpine.
build='
    apk add --no-cache rust cargo git musl-dev clang-dev clang-static llvm-dev pkgconf rust-bindgen \
      lz4-dev lz4-static zstd-dev zstd-static \
      libxcb-dev libxcb-static libxau-dev libxdmcp-dev xcb-util-cursor-dev \
      xcb-util-image-dev xcb-util-renderutil-dev
    export CARGO_PROFILE_RELEASE_STRIP=true
    # With an explicit --target, RUSTFLAGS reaches only the final binaries, not proc-macros.
    export RUSTFLAGS="-C target-feature=+crt-static"
    export PKG_CONFIG_ALL_STATIC=1
    target=$(rustc -vV | sed -n "s/^host: //p")

    git clone --depth 1 --branch "$WAYPIPE_VERSION" https://gitlab.freedesktop.org/mstoeckl/waypipe.git /src/waypipe
    cd /src/waypipe
    cargo build --release --locked --target "$target" --no-default-features --features lz4,zstd
    install -m 0755 "target/$target/release/waypipe" "$OUT"/waypipe

    # Alpine ships no static libXau or libxcb-util, and its xcb-cursor.pc leaves out xcb-util.
    apk add --no-cache xorgproto util-macros make xcb-util-dev xz
    wget -qO- "https://xorg.freedesktop.org/releases/individual/lib/libXau-$LIBXAU_VERSION.tar.xz" | tar -xJ -C /src
    wget -qO- "https://xorg.freedesktop.org/releases/individual/lib/xcb-util-$XCB_UTIL_VERSION.tar.xz" | tar -xJ -C /src
    for lib in "libXau-$LIBXAU_VERSION" "xcb-util-$XCB_UTIL_VERSION"; do
      cd "/src/$lib"
      ./configure --prefix=/usr --enable-static --disable-shared >/dev/null
      make -s install
    done
    sed -i "s/^Requires.private: .*/& xcb-util/" /usr/lib/pkgconfig/xcb-cursor.pc

    git clone --depth 1 --branch "$SATELLITE_VERSION" https://github.com/Supreeeme/xwayland-satellite.git /src/satellite
    cd /src/satellite
    cargo build --release --locked --target "$target" --no-default-features
    install -m 0755 "target/$target/release/xwayland-satellite" "$OUT"/xwayland-satellite
'

if [ -f /etc/alpine-release ] && [ "$(id -u)" = 0 ]; then
  # Already in an Alpine container (CI): build here.
  OUT="$out" WAYPIPE_VERSION="$WAYPIPE_VERSION" SATELLITE_VERSION="$SATELLITE_VERSION" \
    LIBXAU_VERSION="$LIBXAU_VERSION" XCB_UTIL_VERSION="$XCB_UTIL_VERSION" sh -euxc "$build"
elif command -v docker >/dev/null; then
  docker run --rm \
    -v "$out:/out" \
    -e OUT=/out \
    -e WAYPIPE_VERSION="$WAYPIPE_VERSION" \
    -e SATELLITE_VERSION="$SATELLITE_VERSION" \
    -e LIBXAU_VERSION="$LIBXAU_VERSION" \
    -e XCB_UTIL_VERSION="$XCB_UTIL_VERSION" \
    "$build_image" sh -euxc "$build
    chown $(id -u):$(id -g) /out/waypipe /out/xwayland-satellite"
else
  echo "this script needs docker, or to run as root in Alpine" >&2
  exit 1
fi

for bin in waypipe xwayland-satellite; do
  if ! file "$out/$bin" | grep -q "static"; then
    echo "$bin is not static" >&2
    exit 1
  fi
done
(cd "$out" && sha256sum waypipe xwayland-satellite)
