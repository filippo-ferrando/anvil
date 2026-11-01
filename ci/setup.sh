#!/bin/sh
# Toolchain for anvil-ci.yaml, baked once into a warm snapshot.
# Rebuilt when this script, go.mod or go.sum change.
set -eu

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git make gcc libc6-dev \
	qemu-system-x86 qemu-utils ovmf xorriso zstd openssh-client
apt-cache show virtiofsd >/dev/null 2>&1 && apt-get install -y virtiofsd

# Go version from go.mod, since distros ship an older one.
v=$(sed -n 's/^go //p' go.mod)
curl -fsSL "https://go.dev/dl/go${v}.linux-amd64.tar.gz" | tar -C /usr/local -xz
ln -sf /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/bin/

# Module cache outside /work, which is removed after setup.
export GOMODCACHE=/opt/go/mod GOTOOLCHAIN=local
go mod download
