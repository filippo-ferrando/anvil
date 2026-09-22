# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Anvil is a Multipass-style VM & container manager: one daemon (`anvild`) drives both
QEMU VMs (direct QMP, no libvirt) and Docker containers (direct HTTP API, no SDK) behind
a single gRPC API, with a CLI (`anvil`) and a Bubble Tea TUI (`anvil tui`) as clients.

## Commands

```
make proto   # regenerate api/gen/anvil/v1 from api/proto/anvil/v1/anvil.proto (needs protoc)
make build   # anvil (CGO_ENABLED=0) + anvild
make test    # go test ./...
make vet     # go vet ./...
make fmt     # gofmt -l -s . (lists files that need formatting; doesn't rewrite them)
```

Single test / package:

```
go test ./internal/vm/qemu/...
go test ./internal/instance/... -run TestManager_Create -v
```

Some tests are real integration tests, not unit tests, and skip themselves when the
tooling isn't present rather than failing (e.g.
`internal/vm/qemu/spawn_integration_test.go` spawns a real `qemu-system-x86_64` process
and is `//go:build linux`, self-skips if `qemu-system-x86_64`/`qemu-img` aren't on
`PATH`). Don't "fix" a skip like that by removing the tool-presence check.

Building from source needs Go 1.27+, `protoc`, and (Linux) `qemu-system-x86_64`,
`qemu-img`, `xorriso` on `PATH`.

## Architecture

```
anvil (CLI + TUI, one binary)  ──gRPC over a unix socket──▶  anvild (daemon)
                                                                  │
                                                                  ├─ QEMU processes, driven over QMP
                                                                  ├─ Docker's real HTTP API
                                                                  ├─ bbolt (instances / intents / images / mirrors)
                                                                  └─ SSH, for migration to another anvild
```

**The daemon owns all business logic. The CLI and TUI are dumb clients**: build a
request, call the daemon over the unix socket, print/render the reply. Keep it that way
— logic added to `internal/cli/commands` or `internal/tui` instead of behind the gRPC
boundary in `internal/` will drift between the two clients. Access control is just unix
group membership (`anvil` group) plus socket reachability; there are no accounts, no
portal, no password.

Migration/host-to-host work never uses daemon-to-daemon gRPC — it shells out over plain
SSH to the target's own local `anvil` CLI (`internal/migrate`). This means Anvil never
solves cross-host trust itself: whatever SSH access already exists is sufficient.

### Layout

```
cmd/anvild/      daemon entrypoint (platform_linux.go / platform_darwin.go pick the VM backend)
cmd/anvil/       CLI entrypoint
pkg/client/      thin gRPC client, shared by the CLI and TUI
api/proto/       gRPC API definition (anvil.proto) — source of truth; api/gen is generated, don't hand-edit
internal/
  daemon/        gRPC service implementation (one *_server.go per resource area)
  instance/      domain model + Backend interface + the Manager that dispatches to it
  vm/            QEMU backend (Linux): process mgmt, QMP, cloud-init, image catalog
  vm/vz/         Apple Virtualization.framework backend (darwin) — stub, not implemented yet
  container/docker/   Docker backend
  container/podman/   empty package, Podman backend not implemented yet (see PLAN.md)
  intent/        grouping VMs/containers together, shared per-intent bridge network + IPAM
  intent/dns/    per-intent DNS server on each intent's gateway ("<role>.<intent>.anvil")
  migrate/       SSH-driven cross-host migration
  store/         bbolt-backed registry (instances, intents, images, mirrors, hosts, cloud-init)
  cli/commands/  cobra commands (one file per command/command group)
  tui/           the Bubble Tea app
data/distros/    built-in VM image catalog (embedded in the binary)
packaging/       PKGBUILD/deb/rpm build scripts, systemd unit, sysusers/tmpfiles rules
```

### The `instance.Backend` interface

`internal/instance/backend.go` defines the core `Backend` interface (Create/Start/Stop/
Delete/Status/Logs) that both the QEMU/vz backend and the Docker backend implement.
Optional capabilities (mount, port-forward, snapshot, fork) are separate interfaces
(`Mounter`, `PortForwarder`, `Snapshotter`, `Forker`) that a backend implements only if
it supports that operation — a container backend has no `Snapshotter`, for instance.
`internal/instance/manager.go`'s `Manager` type-asserts against these before calling
them. When adding a capability to one backend, extend this interface set rather than
adding backend-specific branching in the daemon or manager.

### Cross-platform VM backend selection

`newVMBackend` is defined per-platform via build tags (`cmd/anvild/platform_linux.go`,
`platform_darwin.go`): Linux builds compile in `internal/vm` (QEMU), darwin builds
compile in `internal/vm/vz` (Virtualization.framework, currently unimplemented). Despite
this scaffolding, the README describes Anvil as Linux-only today — treat darwin/`vz` as
in-progress, not a supported target.

### Mirrors vs. cloud-init template repos

`anvil mirror` (`internal/store/mirrors.go`) adds to or overrides the built-in VM image
catalog, or rewrites container image references to pull through a different registry —
both are *standing, re-consulted registry entries*. `anvil cloud-init import-repo` is a
one-shot bulk copy of cloud-init YAML templates into the saved cloud-init library; it has
no corresponding `list`/`remove`/`enable`/`disable`, imported templates become
indistinguishable from hand-written ones. Don't conflate the two when touching either
code path. Full mechanics and manifest schemas: `docs/mirrors.md`.

## Conventions

- Go 1.27, module `github.com/anvil-project/anvil`.
- New gRPC surface starts in `api/proto/anvil/v1/anvil.proto`, then `make proto`, then
  implement in the matching `internal/daemon/*_server.go`.
- `internal/cli/commands` is cobra; each command/command group gets its own file.
- no em-dashes in docs, comment or code, use normal punctuation.
- use a simple language style in comments and docs, avoid jargon, avoid "we" or "you" or "I", avoid marketing-speak.
- keep comments of max 2 lines, avoid long paragraphs, avoid repeating the code in the comment.
- each new feature should have a corresponding (if it make sense) TUI and CLI command, and a gRPC API surface.
- each new feature should have a corresponding test, either unit or integration.
