#!/bin/sh
# Toolchain for anvil-ci.yaml, baked once into a warm snapshot.
# Rebuilt when this script, go.mod or go.sum change.
set -eu

pacman -Syu --noconfirm
pacman -Syu --noconfirm ca-certificates curl git make gcc libc6-dev \
	qemu-system-x86 qemu-utils ovmf xorriso zstd openssh-client virtiofsd go

# Module cache outside /work, which is removed after setup.
export GOMODCACHE=/opt/go/mod GOTOOLCHAIN=local
go mod download
