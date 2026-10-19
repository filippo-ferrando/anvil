#!/usr/bin/env bash
# Builds anvil's Firecracker guest kernel: Firecracker's own 6.18 guest config plus
# scripts/fc-kernel.config, everything built in, in a Debian container (docker, or the
# current one when already in Debian as root). Output: <out>/anvil-fc-kernel-<version>-<arch>.
#
# Usage: scripts/build-fc-kernel.sh [output-dir] [x86_64|aarch64]
set -euo pipefail

kernel_version="6.18.57"
# The Firecracker release whose guest config is the base.
firecracker_ref="v1.17.0"
build_image="docker.io/library/debian:trixie"

out="$(realpath -m "${1:-build/fc-kernel}")"
arch="${2:-x86_64}"
case "$arch" in
  x86_64) karch=x86 target=bzImage image=arch/x86/boot/bzImage cross="" pkgs="" skip="" ;;
  # arm64 has no i8042 keyboard, so those fragment lines can't apply there.
  aarch64) karch=arm64 target=Image image=arch/arm64/boot/Image cross=aarch64-linux-gnu- pkgs=gcc-aarch64-linux-gnu \
    skip="CONFIG_SERIO_I8042=y CONFIG_KEYBOARD_ATKBD=y" ;;
  *) echo "unknown arch $arch (want x86_64 or aarch64)" >&2; exit 1 ;;
esac
fragment="$(realpath "$(dirname "$0")/fc-kernel.config")"
name="anvil-fc-kernel-$kernel_version-$arch"
mkdir -p "$out"

# The build steps, run as root inside Debian. The fixed timestamp, user and host keep
# rebuilds of the same inputs byte-identical.
build='
    apt-get update -qq
    apt-get install -y -qq --no-install-recommends build-essential bc bison flex libelf-dev \
      libssl-dev cpio wget xz-utils ca-certificates $PKGS >/dev/null
    major=${KERNEL_VERSION%%.*}
    cd /tmp
    wget -q "https://cdn.kernel.org/pub/linux/kernel/v$major.x/linux-$KERNEL_VERSION.tar.xz"
    wget -q "https://cdn.kernel.org/pub/linux/kernel/v$major.x/sha256sums.asc"
    grep " linux-$KERNEL_VERSION.tar.xz\$" sha256sums.asc | sha256sum -c -
    tar -xJf "linux-$KERNEL_VERSION.tar.xz"
    cd "linux-$KERNEL_VERSION"
    wget -qO .config "https://raw.githubusercontent.com/firecracker-microvm/firecracker/$FIRECRACKER_REF/resources/guest_configs/microvm-kernel-ci-$ARCH-6.18.config"

    export ARCH=$KARCH CROSS_COMPILE=$CROSS
    export KBUILD_BUILD_TIMESTAMP="2026-01-01" KBUILD_BUILD_USER=anvil KBUILD_BUILD_HOST=anvil
    scripts/kconfig/merge_config.sh -m .config /fragment.config
    make olddefconfig
    # merge_config only warns when a symbol was dropped; a missing one here is an error.
    missing=0
    for opt in $(grep -E "^CONFIG_[A-Z0-9_]+=y" /fragment.config); do
      case " $SKIP " in *" $opt "*) continue ;; esac
      grep -qx "$opt" .config || { echo "missing after olddefconfig: $opt" >&2; missing=1; }
    done
    [ "$missing" = 0 ]
    make -s -j"$(nproc)" "$TARGET"
    install -m 0644 "$IMAGE" "$OUT/$NAME"
    install -m 0644 .config "$OUT/$NAME.config"
'

envs=(-e OUT=/out -e NAME="$name" -e KERNEL_VERSION="$kernel_version" -e FIRECRACKER_REF="$firecracker_ref"
  -e ARCH="$arch" -e KARCH="$karch" -e TARGET="$target" -e IMAGE="$image" -e CROSS="$cross" -e PKGS="$pkgs"
  -e SKIP="$skip")

if [ -f /etc/debian_version ] && [ "$(id -u)" = 0 ]; then
  # Already in a Debian container (CI): build here.
  cp "$fragment" /fragment.config
  env OUT="$out" NAME="$name" KERNEL_VERSION="$kernel_version" FIRECRACKER_REF="$firecracker_ref" \
    ARCH="$arch" KARCH="$karch" TARGET="$target" IMAGE="$image" CROSS="$cross" PKGS="$pkgs" SKIP="$skip" \
    bash -euxc "$build"
elif command -v docker >/dev/null; then
  docker run --rm -v "$out:/out" -v "$fragment:/fragment.config:ro" "${envs[@]}" "$build_image" \
    bash -euxc "$build
    chown $(id -u):$(id -g) /out/$name /out/$name.config"
else
  echo "this script needs docker, or to run as root in Debian" >&2
  exit 1
fi

(cd "$out" && sha256sum "$name")
