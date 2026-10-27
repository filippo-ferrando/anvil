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
make man     # regenerate man/man1 from the live command tree, copy hand-written docs/man/anvild.8 to man/man8
make gui-tools  # static waypipe/xwayland-satellite for `anvil gui` into build/gui-tools (needs docker)
make fc-kernel  # anvil's Firecracker guest kernel into build/fc-kernel (needs docker, ARCH=aarch64 for arm64)
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
`qemu-img`, `xorriso` on `PATH`, plus `virtiofsd` (looked up on `PATH` and in
`/usr/lib`, `/usr/libexec`). `anvil gui` with VMs also needs the GUI tools in
`/usr/lib/anvil/gui` (or `ANVIL_GUI_TOOLS`). Full feature notes: `docs/gui.md`.
Firecracker VMs also need `firecracker`/`jailer` and `nft`; their real-VM tests need
`ANVIL_FC_KERNEL` and `ANVIL_E2E_IMAGE` (see `docs/firecracker.md`).

## Architecture

```
anvil (CLI + TUI, one binary)  ──gRPC over a unix socket──▶  anvild (daemon)
                                                                  │
                                                                  ├─ QEMU processes, driven over QMP
                                                                  ├─ Docker's real HTTP API
                                                                  ├─ bbolt (instances / intents / mirrors / hosts / events)
                                                                  └─ SSH, for migration to another anvild
```

**The daemon owns all business logic. The CLI and TUI are dumb clients**: build a
request, call the daemon over the unix socket, print/render the reply. Keep it that way:
logic added to `internal/cli/commands` or `internal/tui` instead of behind the gRPC
boundary in `internal/` will drift between the two clients. Access control is just unix
group membership (`anvil` group) plus socket reachability; there are no accounts, no
portal, no password.

`anvil --remote <[user@]host>` (or `ANVIL_REMOTE`) points the CLI/TUI at another host's
anvild: `pkg/client/ssh.go` runs `ssh <host> anvil dial-stdio` and speaks gRPC over that
pipe, so the remote daemon still only listens on its unix socket.

Migration/host-to-host work never uses daemon-to-daemon gRPC. It shells out over plain
SSH to the target's own local `anvil` CLI (`internal/migrate`). This means Anvil never
solves cross-host trust itself: whatever SSH access already exists is sufficient.

### Layout

```
cmd/anvild/      daemon entrypoint (platform_linux.go / platform_darwin.go pick the VM backend)
cmd/anvil/       CLI entrypoint
pkg/client/      thin gRPC client (unix socket, or SSH for --remote), shared by the CLI and TUI
api/proto/       gRPC API definition (anvil.proto), the source of truth; api/gen is generated, don't hand-edit
internal/
  daemon/        gRPC service implementation (one *_server.go per resource area), plus
                 the optional Prometheus /metrics endpoint (metrics.go, ANVIL_METRICS)
  instance/      domain model + Backend interface + the Manager that dispatches to it
  vm/            VM backends (Linux): QEMU (backend.go) and Firecracker (fc_*.go) behind
                 Hypervisors (hypervisors.go), cloud-init, image catalog
  vm/firecracker/  Firecracker process, REST API client, vsock, jailer
  vm/qemu/       QEMU process, args, QMP and guest agent clients, qemu-img, virtiofsd
  vm/image/      image catalog, mirrors, downloads and the prepared-image vault (plus Firecracker raw cache)
  vm/cloudinit/  NoCloud seed builder
  vm/network/    tap devices on a Linux bridge (netlink)
  vm/vz/         Apple Virtualization.framework backend (darwin), a stub, not implemented yet
  container/docker/   Docker backend
  container/      the Backend that picks an engine; Podman is rejected, not implemented (see PLAN.md)
  intent/        grouping VMs/containers together, shared per-intent bridge network + IPAM
  intent/dns/    per-intent DNS server on each intent's gateway ("<role>.<intent>.anvil")
  intent/ipam/   subnet and address allocation for intent networks
  migrate/       SSH-driven cross-host migration
  migrate/payload/  JSON payload migrate sends to the target's anvil CLI
  discovery/     mDNS announce and browse behind `anvil host discover`
  export/        bundle export/import behind `anvil export` and `anvil import`
  hostdev/       USB/PCI device listing, boot-device denylist, and the root helper
                 (`anvild devhelper`, anvil-devhelper.socket) behind `launch --device`
  gui/           `anvil gui`: fd-passing socket relay + xauth (gui/relay), app .desktop/icon lookup (gui/appinfo)
  config/        fixed on-disk paths (run/state/cache dirs, GUI relay and tools dirs)
  intent/apply/  the compare-and-apply engine behind `anvil apply`
  store/         bbolt-backed registry (instances, intents, mirrors, hosts, cloud-init, events)
  cloudinitrepo/ fetches a cloud-init template repo manifest and its templates
  hostpath/      ACL grants and path checks for host dirs the anvil user must reach (`anvil create-dir`)
  progress/      progress line formatting shared by the CLI and TUI
  sshkey/        anvil's default guest-access SSH keypair
  version/       build version stamped from git
  cli/commands/  cobra commands (one file per command/command group)
  tui/           the Bubble Tea app
data/distros/    built-in VM image catalog (embedded in the binary)
catalog/         official extra catalogue (distro manifests + cloud-init templates), a git
                 submodule consumed via `anvil mirror add` / `cloud-init import-repo`, not embedded
scripts/         verify-catalog.sh (checksums), release-notes.sh (used by the release workflow),
                 changelog.sh (CHANGELOG.md from commit subjects),
                 build-gui-tools.sh (static GUI tools, used by `make gui-tools` and releases),
                 build-fc-kernel.sh + fc-kernel.config (the Firecracker guest kernel, `make fc-kernel`)
packaging/       PKGBUILD/deb/rpm build scripts, systemd unit, sysusers/tmpfiles rules
```

### The `instance.Backend` interface

`internal/instance/backend.go` defines the core `Backend` interface (Create/Start/Stop/
Delete/Status/Logs) that both the QEMU/vz backend and the Docker backend implement.
Optional capabilities are separate interfaces (`Mounter`, `PortForwarder`, `Snapshotter`,
`Forker`, `Reconciler`, `StatsProvider`, `GuestInspector`, `ReadyWaiter`, `ExitNotifier`,
`GUIPreparer`, `GuestFileReader`, `DeviceHolder`, and `Resizer`/`LiveResizer` in `update.go`) that a backend implements only if
it supports that operation, so a container backend has no `Snapshotter`.
`internal/instance/manager.go`'s `Manager` type-asserts against these before calling
them. When adding a capability to one backend, extend this interface set rather than
adding backend-specific branching in the daemon or manager.

### Cross-platform VM backend selection

`newVMBackend` is defined per-platform via build tags (`cmd/anvild/platform_linux.go`,
`platform_darwin.go`): Linux builds compile in `internal/vm` (QEMU + Firecracker), darwin builds
compile in `internal/vm/vz` (Virtualization.framework, currently unimplemented). Despite
this scaffolding, the vz backend is still a stub: basic macOS support is in progress, so treat darwin/`vz` as
in-progress, not a supported target.

### Mirrors vs. cloud-init template repos

`anvil mirror` (`internal/store/mirrors.go`) adds to or overrides the built-in VM image
catalog, or rewrites container image references to pull through a different registry.
Both are *standing, re-consulted registry entries*. `anvil cloud-init import-repo` is a
one-shot bulk copy of cloud-init YAML templates into the saved cloud-init library; it has
no corresponding `list`/`remove`/`enable`/`disable`, imported templates become
indistinguishable from hand-written ones. Don't conflate the two when touching either
code path. Full mechanics and manifest schemas: `docs/mirrors.md`.

## Conventions

- Go 1.27, module `git.fferrando.cc/rdfilippo/anvil`.
- New gRPC surface starts in `api/proto/anvil/v1/anvil.proto`, then `make proto`, then
  implement in the matching `internal/daemon/*_server.go`.
- `internal/cli/commands` is cobra; each command/command group gets its own file.
- list-style commands offer `-o table|json` through `addOutputFlag`/`checkOutput`/`printJSON`
  in `internal/cli/commands/table.go`; JSON is the gRPC reply itself, not a CLI-side shape.
- no em-dashes in docs, comment or code, use normal punctuation.
- use a simple language style in comments and docs, avoid jargon, avoid "we" or "you" or "I", avoid marketing-speak.
- keep comments of max 2 lines, avoid long paragraphs, avoid repeating the code in the comment.
- each new feature should have a corresponding (if it make sense) TUI and CLI command, and a gRPC API surface.
- each new feature should have a corresponding test, either unit or integration.
