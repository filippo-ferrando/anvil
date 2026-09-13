# Anvil plan

This is the design doc for anvil, written before most of the code existed. Keep it around
as the reference for "why did we do it this way" questions. If reality drifts from this
doc (it will), update the doc, don't just leave it stale.

## What is this thing

Anvil is a Multipass-like tool, written in Go, that manages cloud-init VMs and containers
(Docker and Podman, both). Think of it as: Multipass, but you can also run containers,
group things together, move them between machines, and it's not tied to Ubuntu.

Daemon is `anvild`, CLI is `anvil`, there will eventually be a TUI too (`anvil tui`).

## Why build this instead of just using Multipass

Multipass is solid but:
- VM only, no containers
- basically an Ubuntu-first tool
- no way to group related VMs together
- no way to move a VM to a different machine
- GUI, Windows and macOS support add a lot of surface area we don't need

We took some structural ideas from a project called Hyperpass (a C++ fork of Multipass
with some Elemento-specific stuff bolted on: LLM runtime, GUI, marketplace). We are not
reusing any of its code. We looked at how it structured its gRPC API, its image catalog,
its migration flow and its cloud-init management screen, and used those as inspiration
for a from-scratch Go implementation. Nothing here is a Multipass or Hyperpass fork.

## Hard constraints, decided up front

- **Linux only.** No macOS, no Windows. One host platform means the VM backend and the
  container backend don't need a platform abstraction layer.
- **No login system.** Access to the daemon is controlled by being in the `anvil` unix
  group and being able to reach its socket. No accounts, no portal, no password.
- **VMs are driven directly with QEMU.** No libvirt. We spawn `qemu-system-x86_64`
  ourselves and talk to it over QMP.
- **Containers support both Docker and Podman.** Docker gets built first (milestone 3,
  done), Podman second (not built yet). Both talk to their engine's REST API directly for
  the daemon-side lifecycle work (create/start/stop/delete/logs), using a hand-rolled client
  for Docker (see the Container backend section, not the official SDK after all),
  `containers/podman/v5/pkg/bindings` planned for Podman. `ContainerSpec` gets an
  `engine` field so a container instance says which one it's running on. The one place
  we *do* shell out to the real `docker`/`podman` CLI binary is `anvil shell`/`exec`
  (interactive TTY handling), same reasoning as using the real `ssh` binary for VMs
  instead of a Go SSH library.
- **The client is a TUI, not a GUI.** Built with `tview`, invoked as `anvil tui`, same
  binary as the CLI.

## Architecture, roughly

```
anvil (CLI + TUI, one binary) --- gRPC over a unix socket ---> anvild (daemon, runs as root)
                                                                    |
                                                                    +-- QEMU processes (VMs)
                                                                    +-- Docker / Podman REST API (containers)
                                                                    +-- bbolt db (instances/intents/images/mirrors)
                                                                    +-- mDNS advertise/browse (finding other anvil hosts)
                                                                    +-- SSH to a remote anvil CLI (migration)
```

The daemon owns every bit of business logic. The CLI and TUI are dumb clients that build
a request, call the daemon, print the response. If you ever find yourself writing actual
logic in `internal/cli` or `internal/tui`, that's a sign it's in the wrong place.

Everything the daemon does over the network for migration goes through SSH to the
target's own local `anvil`/`anvild`, not daemon-to-daemon gRPC. This means we never have
to solve cross-host trust for gRPC: whatever SSH access you already have to a box is
enough to migrate to it.

## Project layout

```
anvil/
  go.mod
  api/proto/anvil/v1/anvil.proto       source of truth for the API
  api/gen/anvil/v1/*.pb.go             generated, committed to the repo
  cmd/anvild/main.go
  cmd/anvil/main.go
  pkg/client/                          thin gRPC client, used by CLI and TUI
  internal/
    daemon/                            gRPC server, converts between wire types and domain types
    instance/                          domain model (Spec, VMSpec, ContainerSpec), Manager, Backend/Registry interfaces
    vm/                                QEMU backend: qemu/, cloudinit/, image/, network/ (tap/bridge attach)
    container/docker/                  Docker backend (built)
    container/podman/                  Podman backend (deferred, filippo doesn't have Podman to test against)
    intent/                            intent membership, launch routing, shared network orchestration; ipam/ (subnet/address math)
    migrate/                           SSH-driven migration (not built yet)
    discovery/                         mDNS host discovery (not built yet)
    store/                             bbolt-backed registry
    config/                            fixed on-disk paths (/run/anvil, /var/lib/anvil, etc.)
    cli/commands/                      cobra commands
    tui/                               tview app (not built yet)
  data/distros/                        embedded default VM image catalog
  packaging/                           PKGBUILD, systemd unit (not built yet)
```

One thing worth calling out: the plan originally put the `Backend` interface in its own
`internal/backend` package. That creates an import cycle once the instance manager needs
to reference it back (`backend` imports `instance` for the spec types, `instance`'s
manager would need to import `backend` for the interface, that's a cycle). We folded
`Backend` and a `Registry` interface straight into the `instance` package instead. Same
idea, one less package.

## The pieces, one by one

### API layer

gRPC over a unix domain socket at `/run/anvil/anvild.sock`, not a hand rolled REST API.
Reason: Launch needs a server-streaming RPC for progress updates, and gRPC gives you that
for free. The `.proto` file is the contract, generated Go code is committed to the repo so
building the project doesn't require `protoc` unless you're actually changing the API.

No TLS. The socket's file permissions (owned by an `anvil` group, mode 0660) are the
whole access control story, same idea as `podman.sock` or `docker.sock`.

**Provisioning progress**: `anvil launch` used to just print "provisioning" and then go
silent until it either finished or errored, which for a slow image download or container
pull looked exactly like it had hung. `Backend.Create` now takes a `progress func(status
string)` callback, threaded through `Manager.Launch`'s existing `LaunchEvent.Status`
plumbing (so no proto/API change, `LaunchProgress` already had a `Status` case, it just
wasn't being fed much). Both backends actually use it now: the VM backend reports
download percentage (throttled to twice a second, not a flood of one line per chunk),
then "creating disk overlay", then "building cloud-init seed"; the Docker backend
forwards Docker's own per-layer pull status (`Pulling fs layer` -> `Downloading` ->
`Verifying Checksum` -> `Pull complete`, each tagged with its layer ID), throttled the
same way so a long download doesn't spam identical lines but also never goes quiet for
more than half a second.

First real usage (filippo actually downloading a real Ubuntu image) showed the "print
each line as it arrives" approach was worse than the silence it replaced: a ~500ms-per-
tick download prints a genuinely new line every tick, so a big image scrolled the
terminal with hundreds of near-identical percentage lines. Fixed in
`internal/cli/commands/launch.go`'s `streamLaunchProgress`: on a real terminal (checked
via `isTerminalWriter`, skipped for piped/redirected output), consecutive updates that
share a "key" (everything before the first `": "` in the status text, so
`"downloading ubuntu-24.04: 43%..."` groups by image name and `"abc123: Downloading..."`
groups by Docker layer ID, see `progressKey`) redraw the same line in place with a
carriage return instead of each becoming a new one. Not a full multi-line progress
renderer: a multi-layer Docker pull still prints more than one line when different
layers' updates interleave, just far fewer than one-per-tick.

### VM backend

We spawn `qemu-system-x86_64` and talk to it over QMP (a small JSON-over-unix-socket
protocol QEMU exposes). We wrote our own minimal QMP client instead of pulling in a
library, since the bit of the protocol we need is small and we didn't want to depend on
something of uncertain maintenance status.

Cloud-init seed images (the little ISO that carries `user-data`/`meta-data` into the
VM) are built with `xorriso` for now. The plan calls for a pure Go ISO9660 writer later
(`kdomanski/iso9660`) so we don't need `xorriso` installed at all, but that needs network
access to fetch the dependency, which wasn't available while this was first written.
Swapping it in later is a one-line change since we already built it behind a `Builder`
interface.

Base images: there's a two-tier setup. "Prepared" images are the full downloaded qcow2
files for a given distro, cached once. Every instance gets its own qcow2 that's just a
thin overlay on top of the prepared image (`qemu-img create -b`), so launching a new VM
of a distro you already have cached is basically instant and barely uses disk.

Networking: SLIRP (user mode networking) by default for a standalone VM, which needs zero
host privileges. VMs that are part of an intent get bridged instead, see the intent
section below.

Restarting `anvild` while VMs are running used to just lose track of them (they'd read as
stopped even though QEMU was still alive). Fixed now: `Start` writes a small `runtime.json`
per instance (pid + QMP socket path, deliberately outside bbolt, see "Instance registry"
below), and at startup `Manager.Reconcile` walks every instance the registry thinks is
running, re-verifies the pid is actually alive and its `/proc/<pid>/cmdline` really looks
like our qemu process for that instance's disk (not some unrelated process that happens to
have reused the pid), then reattaches QMP. If any of that fails, the instance gets marked
stopped instead of trusted blindly.

### Container backend

Docker half is done (milestone 3, part one). Podman is still not built.

Both engines implement the exact same `instance.Backend` interface the VM code already
uses, so `list/start/stop/delete/info` keep working uniformly no matter what's actually
running the container. `ContainerSpec` gets an `engine` field (`docker` or `podman`) so
an instance record says which one owns it. Right now the default (when `--engine` is
left blank) is Docker, since that's the only one that exists; the plan is still to flip
the default to Podman once it's built, matching the original "Podman is the default"
idea, Docker was just what got built first.

Why two backends instead of pointing Docker's client at Podman's Docker-compatible
socket and calling it done: Podman's compatibility layer doesn't cover every native
feature (and we specifically want Podman's own pod/network primitives for intents later),
so a real native Podman backend is worth having, we just don't want to block on it before
Docker works.

- **Docker** (built): not the official `github.com/docker/docker/client` SDK after all.
  Same call as QMP for the VM side: hand-rolled a small REST client
  (`internal/container/docker/client.go`) against `/var/run/docker.sock`'s plain HTTP
  API, pinned to API version `v1.41`, instead of taking on a big SDK dependency whose
  current API surface we couldn't verify against without network access in this
  environment. Covers create/start/stop/remove/inspect/logs, including demuxing Docker's
  multiplexed log stream format (an 8-byte frame header per chunk). Tested against a real
  mock unix-socket HTTP server (no real dockerd available in this sandbox, no root/
  rootless tooling here), all passing. `container.DockerBackend` (in package
  `internal/container`, not `internal/container/docker`, see the package-split note
  below) wraps this client as an `instance.Backend`.
- **Podman** (not built yet): `containers/podman/v5/pkg/bindings` against `podman.sock`,
  `ContainerSpec` maps onto Podman's own spec generator shape.

Users point `--image` at literally any OCI image either way, we're not curating a
container catalog the way we do for VM distros.

**Dispatch**: `instance.Manager`'s backend map is keyed only by `Kind` (vm/container),
not by engine, so there's a small multiplexing `container.Backend` in front of the real
per-engine backends (`internal/container/backend.go`) that picks Docker or Podman per
instance based on `ContainerSpec.Engine` and forwards the call. `container.Backend.Podman`
just stays `nil` until Podman lands.

**Package split**: `internal/container/docker` only touches the standard library (plus
`net/http` against a unix socket), no import of `internal/instance`, so it stays testable
without network access in this environment (`internal/instance` pulls in `oklog/ulid`,
which isn't vendored and needs a real module fetch). The `instance.Backend` wrapper
(`docker_backend.go`) and the multiplexing `Backend` live one level up, in plain
`internal/container`, mirroring the existing `internal/vm/qemu` (pure) vs `internal/vm`
(wired to `instance`) split.

**Shell/exec/transfer**: `anvil shell`/`anvil exec`/`anvil transfer` now work against
container instances too, not just VMs. Same reasoning as using the real `ssh`/`scp`
binaries instead of a Go SSH library for the VM side: shell out to the real `docker`/
`podman` CLI binary (`docker exec -it <id> ...`, `docker cp`) so real TTY/resize handling
comes for free, rather than reimplementing Docker's attach/exec streaming protocol just
for this one interactive case. `anvil shell` defaults to `/bin/sh` (not `bash`, since an
arbitrary image isn't guaranteed to have it). Not yet tested against a real daemon in
this environment, same caveat as the rest of the Docker backend.

### Image catalog and mirrors

The embedded `data/distros/distribution-info.json` ships with entries for the big
cloud-init distros: Ubuntu, Debian, Arch, Rocky, AlmaLinux, CentOS Stream, plus some
placeholder entries for Fedora, openSUSE and Alpine that still need real URLs filled in.
Heads up: none of the checksums are filled in yet either, and I have not been able to
verify the URLs against a live download. Treat this file as a solid starting point, not
gospel, until someone actually runs a download against every entry.

On top of the built-in catalog, you can register your own mirrors at runtime
(`anvil mirror add --kind vm <name> --manifest-url <url>`, where the manifest is just a
JSON file in the same schema as the built-in one). This part is done: the manifest gets
fetched and checked once at add time, cached in a `mirrors` bbolt bucket, then merged
into the catalog by priority whenever a VM launches, no repeat network call.

Container registry mirrors (`--kind container`) are applied now too, for Docker at
least, and it turned out Docker and Podman really don't handle this the same way, same
heads up as before, just resolved now instead of deferred. Podman lets you drop an
arbitrary per-registry mirror config under `/etc/containers/registries.conf.d/` and it
just works, daemon-side, for anything talking to that Podman install, anvil-managed or
not. Docker's `registry-mirrors` setting in `daemon.json` is only for mirroring Docker
Hub specifically, and editing dockerd's own config file (plus reloading the daemon) for
an anvil-specific concern felt like a much bigger blast radius than this needed.

So for Docker, mirror resolution happens client-side instead, entirely inside
`internal/container/docker` (`mirror.go`): before pulling or creating a container,
`DockerBackend` looks up enabled `--kind container` mirrors, and if one's `--mirror-of`
matches the image ref's upstream registry (bare refs like `nginx:alpine` or
`bitnami/postgres` are implicitly `docker.io`, same as Docker itself treats them), the
ref gets rewritten to pull through that mirror's `--registry` instead, and the container
actually runs on the rewritten image. Nothing about dockerd itself is touched. One real
limitation worth calling out: the `--insecure` flag is stored but Docker itself still
won't talk to an insecure/self-signed registry without also adding it to dockerd's own
`insecure-registries` list yourself, unlike Podman where `--insecure` on a
`registries.conf.d` entry is enough on its own. Podman's own version of this (the real
`registries.conf.d` drop-in) is still M3's second half, once that backend exists.

### Instance registry

bbolt, not flat JSON files. Reason: transactional writes mean we don't have to worry
about a half-written file if the daemon dies mid-write, which matters more here because
the reconciliation logic (see "VM backend" above) starts from what's in the registry at
startup and corrects it, rather than trusting it outright.

### Intents

Membership and the shared network are both done now (milestone 4). An intent is a named
group of instances, VMs and/or containers, each tagged with a role (`web`, `db`,
whatever). The important decision here: **every member of an intent shares one
network**, no matter what kind it is, and **intents don't talk to each other** by
default.

There's no separate "create an intent" step in the data model or the API. A member is
created exactly the way a standalone instance is, through the same
`InstanceService.Launch` RPC everything else already goes through, just with
`intent_name`/`role` set on the request. The first launch under a given `intent_name`
that doesn't exist yet brings that intent into existence (`internal/intent.Manager.Launch`
auto-vivifies it); every launch after that just appends another member. This means the
full normal provisioning path applies to intent members too, unchanged: image resolution,
cloud-init, container image pulls, the progress reporting built for M3, all of it, since
it really is the same `instance.Manager.Launch` underneath.

`internal/intent` sits between `internal/daemon` and `internal/instance`, not inside
either one: `internal/store` already imports `internal/instance` (for `Spec`/`Kind` in
the instances bucket), so `internal/instance` can't import `internal/store` back to know
what an `Intent` is without a cycle. `intent.Manager` depends on both instead, and
`internal/daemon`'s `Launch` handler decides which one to call: straight to
`instance.Manager.Launch` for a plain launch, or through `intent.Manager.Launch` when
`intent_name` is set (see `internal/daemon/server.go`). Membership itself
(`store.Intent`/`store.IntentMember`, a `PutIntent`/`GetIntentByName`/... bucket) lives
in `internal/store` the same way `Mirror`/`CloudInitConfig` do, referencing member
instances by ID, not embedding their specs, so the instances bucket stays the single
source of truth.

`anvil intent create <name> --vm role:image --container role:image` and
`anvil intent add <name> --vm role:image` are CLI-side sugar, not their own RPCs: they
just call the same streaming `Launch` once per member with `--intent`/`--role` set,
`create` erroring if the intent already exists and `add` erroring if it doesn't. Neither
exposes per-member resource flags (cpus, env, volumes, ports); for anything beyond "just
an image", use plain `anvil launch --kind vm|container ... --intent NAME --role ROLE`
directly, since that's the exact same underlying path with the full flag surface anyway.
`anvil intent list/info/remove/delete` are real RPCs (`IntentService`, no Create/Add
method, since creation isn't its own operation). `remove` ungroups a member (by role or
instance ID) without deleting it; `delete --purge-members` also deletes every member
instance outright, resolving each member's current name via
`instance.Manager.GetByID` (new, small addition) before calling the normal
`instance.Manager.Delete`.

Not build tested against a real daemon yet, same caveat as M2/M3: this sandbox can't run
`anvild` for real (no root, no `/dev/kvm` reliably, no Docker daemon). Proto syntax
validated with a real `protoc` run; everything else is gofmt-clean and manually reviewed
end to end, same verification level as M3's container work before filippo's own hardware
confirmed it. The networking piece below is the least-verified code in the whole project
so far, see its own callouts.

**The shared network, since Podman is deferred (filippo doesn't have it to test
against): a Docker bridge network per intent, with VM members' tap devices attached to
its underlying Linux bridge.** This is the design filippo picked over an anvil-owned
bridge, on the reasoning that it reuses Docker's own IPAM/network object instead of
anvil managing a second bridge implementation. The original plan called for Podman here
(`podman network inspect --format '{{.NetworkInterface}}'` hands you the bridge name
directly); Docker has no equivalent inspect field, so instead anvil requests an explicit
bridge interface name at creation time via the driver's own
`com.docker.network.bridge.name` option (`internal/container/docker/network.go`'s
`CreateNetwork`) rather than guessing at Docker's undocumented default naming.

- **Lazily created, once per intent**, the first time any member (VM or container) is
  launched under that `intent_name` (see `internal/intent.Manager.ensureNetwork`), gated
  behind a `Networker` interface so `internal/intent` itself stays engine-agnostic (a
  Podman `Networker` would slot in the same way once that backend exists).
  `container.DockerNetworker` is the one implementation that exists.
- **Subnet allocation** (`internal/intent/ipam`, pure and actually tested, unlike the
  parts touching real netlink/Docker below): a private `/24` deterministically hashed
  from the intent's ID, split in two: `.128/25` reserved for Docker's own IPAM
  (container members get an address from there automatically, same as any Docker
  network), `.2`-`.127` reserved for anvil's own static VM address assignments, so a VM's
  manually-picked address can never collide with one Docker hands to a container later.
  If the hashed subnet collides with something else already on the host, Docker's
  `/networks/create` call fails and `ensureNetwork` retries with a different hash (up to
  5 attempts), a real retry path that has never actually seen a real collision to
  actually retry against.
- **Container members**: just get `ContainerSpec.NetworkMode` set to the Docker network's
  name before `Create` runs. Docker handles everything else about their address/DNS
  itself, this part barely differs from any other Docker network attachment.
- **VM members**: get a statically assigned address from the `.2`-`.127` range (the Nth
  VM member gets `.2+N`, tracked by counting existing VM members already in
  `store.Intent.Members`, no separate counter needed) and `VMSpec.NetworkMode` set to
  `"bridge"`. `internal/vm.Backend.Start` then creates a tap device
  (`internal/vm/network.CreateTap`, using `github.com/vishvananda/netlink`, a new
  dependency) and attaches it to the intent's bridge interface instead of the usual SLIRP
  setup; `internal/vm/qemu`'s `-netdev tap,...` argument-building already existed from
  earlier groundwork (`Config.BridgeTapDevice`), this just wires something into it.
  `internal/vm.Backend`'s `buildSeed` also now generates a cloud-init NoCloud
  network-config (v2/netplan-shaped, matching interfaces by `name: "en*"` rather than a
  hardcoded `eth0`, since a virtio NIC's predictable name isn't `eth0` on most modern
  cloud images) with the static address instead of relying on DHCP, since Docker's bridge
  networking doesn't run an actual DHCP server VMs could use the normal way.
- **`anvil shell`/`exec`/`transfer` against a bridged VM** connect directly to its static
  address on port 22 instead of a SLIRP-forwarded `localhost` port, see
  `resolveSSHTarget` in `internal/cli/commands/ssh.go`.
- **What's genuinely unverified here, more than anything else in this project so far**:
  the `github.com/vishvananda/netlink` dependency couldn't be fetched or exercised
  against a real bridge in this sandbox at all (no network access to `go get` it, no root
  to create a real tap device, no existing bridge to attach one to even if it could).
  Whether Docker actually honors `com.docker.network.bridge.name` the way documented,
  whether the IPAM `IPRange` split really keeps Docker's own address assignment out of
  the reserved range, whether a VM on that bridge can actually reach a Docker container
  on the same network, and whether cloud-init's network-config actually applies a static
  IP correctly on a real guest boot: none of that has touched real hardware yet. This is
  the one piece of M4 that most needs a real, deliberate smoke test, not just "does it
  build," before trusting it.
- **Tap device cleanup**: `internal/vm.Backend.Stop` deletes the tap device after QEMU
  exits; a leftover one after an unclean daemon crash isn't cleaned up automatically
  (`Reconcile` doesn't currently know about tap devices at all, only about the QEMU
  process itself), worth revisiting once this is confirmed working at all.
- **Mixed engines**: an intent that mixes VMs with both Docker and Podman containers at
  once is still punted on (see "Things we already know are unresolved" below), moot for
  now since Podman doesn't exist yet, but worth remembering once it does.
- **Tearing the network down**: `anvil intent delete` now also removes the intent's
  Docker network (`intent.Manager.Delete`, via the same `Networker` interface used to
  create it) once the intent record itself is gone. Attempted regardless of
  `--purge-members`, but best-effort either way: Docker refuses to remove a network with
  active endpoints, so a member still attached to it (a container that wasn't purged, or
  a VM whose tap is still on the bridge) will likely make this a no-op, logged but not
  fatal to the delete itself, since the intent record going away is what actually
  matters. `--purge-members` is what makes removal reliably succeed, by clearing out
  every attached member first.

### Migration

Not built yet (milestones 5 and 6). `anvil migrate <name> --to <host> [--copy]`. Works for
a single instance or a whole intent.

The trick: the source daemon doesn't talk to the target daemon directly over gRPC. It
SSHes into the target host and drives the target's own local `anvil` CLI, which talks to
the target's own local `anvild` over its own unix socket. This sidesteps the whole
"how do two daemons trust each other over the network" problem, we just reuse whatever
SSH access you already have.

Default mode is destructive (deletes the source), but it's implemented as copy, verify,
then delete, never delete first. `--copy` skips the delete. Migrating a VM means
flattening its disk into a standalone qcow2 and shipping that over, then rebuilding the
cloud-init seed on the target from the spec (not shipping the seed ISO itself).

Host discovery: anvil daemons advertise themselves over mDNS and can find each other on
the local network automatically, so you don't have to type an IP address every time you
want to migrate something. This is on top of a manual "known hosts" list you can add to
by hand. Discovery only helps you find a candidate host, it doesn't grant any trust by
itself, you still need real SSH access to actually migrate to it.

### Cloud-init config library

Not built yet (milestone 2). Right now `--cloud-init <file>` on `anvil launch` is a one
shot thing: you point at a file, its contents get shipped as-is. The plan is to make this
a proper managed library instead, closer to what Hyperpass's GUI does: `anvil cloud-init
new/edit/rename/delete/list`, saved server side so the CLI and TUI both see the same set
of saved configs, then reference one by name at launch time
(`--cloud-init-name <name>`).

### TUI

Not built yet (milestone 7). Using `tview` (built on `tcell`). Rough shape: a nav list on
the left, a table of instances, a launch form, a cloud-init list-plus-editor view, a
mirrors table, a migration flow. SSH/shell access from the TUI works by suspending the
TUI and handing the real terminal over to an actual `ssh`/`docker exec`/`podman exec`
session, then
resuming the TUI when you exit, same trick tools like k9s and lazygit use. Way simpler
than trying to build a terminal emulator widget.

### Packaging

Not built yet (milestone 8). Two packages: `anvil` (CLI + TUI) and `anvild` (daemon,
systemd unit, sysusers/tmpfiles rules for a dedicated `anvil` system user and group). No
`avahi` dependency (we use a pure Go mDNS library), no `openssh` dependency (pure Go SSH
client for migration). License is going to be Apache-2.0, not picked purely on vibes:
originally we thought we might need to port some GPLv3 Hyperpass GUI code, which would've
forced our hand, but since the client is a from-scratch TUI now, that's moot and we're
free to pick whatever.

Once M3 lands, `anvild`'s runtime deps grow to include both `docker` and `podman` (or
we make them optional deps and let the daemon work with whichever's actually installed,
decide that when we get there).

## Roadmap

Rough order, each milestone should leave you with something you can actually run.

### M1: VMs, single machine (mostly done)
- [x] go module, project layout
- [x] `anvil.proto` and generated stubs
- [x] domain model (`internal/instance/spec.go`)
- [x] QEMU args builder, QMP client, process lifecycle (spawn, stop, escalate to SIGKILL)
- [x] cloud-init seed builder via `xorriso`
- [x] image catalog, downloader, two-tier vault
- [x] bbolt store
- [x] daemon and CLI wired together: `launch/list/info/start/stop/delete/purge` for VMs
- [x] `go build`/`go vet`/`go test` confirmed working on a real machine (network access,
      protoc plugins on PATH)
- [~] partially verified: `TestSpawnRealProcessLifecycle` (`internal/vm/qemu/spawn_integration_test.go`)
      actually spawns a real `qemu-system-x86_64` against a blank disk, dials real QMP,
      calls `query-status` for real, and drives `Stop`'s full escalation path, all for
      real, no mocking. That proves the process/QMP control plane genuinely works. It does
      **not** prove a real cloud-init VM boots, there's no OS on that disk, just an empty
      qcow2, since downloading a real base image needs working network + a working CA
      cert bundle, and this sandbox has neither (checked: `/etc/ssl/certs/ca-certificates.crt`
      is a broken symlink here, plus no root, no `/dev/kvm`, no `/var/lib` at all, so a
      real `anvild` run isn't happening in this environment regardless). Booting a real
      guest with cloud-init actually running inside it still needs your machine
- [x] reconciliation on daemon restart: `runtime.json` per instance (pid, QMP socket)
      written on `Start`, `qemu.Attach` re-verifies pid liveness + `/proc/<pid>/cmdline`
      match before trusting it, `Manager.Reconcile` runs once at daemon startup. Actually
      tested this time (spawned fake processes, checked pid death detection, checked the
      pid-reuse rejection), not just written and hoped for
- [~] real checksums: done for ubuntu-24.04, ubuntu-22.04, debian-12, archlinux, rocky-9,
      almalinux-9, centos-stream-9 (filippo ran `scripts/verify-catalog.sh` for real and
      pasted back real sha256s). Fedora, openSUSE and Alpine still broken: fedora's listing
      URL 404'd outright; openSUSE's and Alpine's actually returned HTTP 200 but for the
      directory-listing HTML page itself, not a real image. The script only checked for
      "200 OK", which isn't enough, so it printed a checksum for the wrong thing. Fixed the
      script (now also rejects non-image Content-Type and anything under 50MB) so this
      can't happen silently again. Real per-file URLs found for openSUSE and Alpine (a
      research pass actually fetched their `.sha256`/`.sha512` sidecar files to confirm),
      but no real sha256 pasted in for either yet since our schema is sha256-specific and
      Alpine only publishes sha512. Rerun `verify-catalog.sh` once network's available to
      get real sha256s computed locally, that always works regardless of what the origin
      publishes. Fedora 40 is EOL and its own download infra is behind anti-bot protection
      that blocks non-browser fetches entirely (checked ~15 mirrors, all purged it as EOL);
      the URL now in the manifest is a best-effort guess corroborated by an official Fedora
      QA wiki page citing the exact filename, but genuinely **not verified**. Could be
      wrong, try it or just bump to a currently-supported Fedora release instead

### M2: making VMs actually usable (done, pending your build/test round)
- [x] `anvil shell <name>` and `anvil exec <name> -- <cmd>`: real SSH, shelling out to the
      actual `ssh` binary (not a Go SSH library, real TTY/agent/known_hosts handling for
      free). Connection info (`ssh_port`, `default_user`) is now populated server side by
      `internal/vm.Backend` at Create/Start time and surfaced through the proto, instead
      of the user having to `ps aux | grep qemu-system-x86_64` to find the forwarded port
      by hand (which is genuinely how this was being tested before this landed)
- [x] `--ssh-key` on `launch`: repeatable, takes a literal key or a `.pub` file path, falls
      back to `~/.ssh/id_ed25519.pub`/`id_rsa.pub` automatically if you pass nothing. VMSpec
      already had `ssh_public_keys` wired server-side since M1, this was just the missing
      CLI flag to actually set it
- [x] **real bug, caught on an actual VM boot**: the server-side code that builds cloud-init
      user-data was injecting `ssh_authorized_keys` by string concatenation onto the default
      `"#cloud-config\n{}\n"` document. That's not valid YAML (you can't follow a flow-style
      `{}` with more block-style keys), so cloud-init silently parsed only the `{}` and
      dropped every key, every time, no matter how the key was supplied. `anvil launch` +
      `anvil shell` kept saying "Permission denied (publickey)" for exactly this reason.
      Rewrote it (`internal/vm.mergeSSHKeys`) to actually parse the existing user-data as
      YAML, merge into its `ssh_authorized_keys` list, and re-serialize, instead of splicing
      text, which would've broken just as badly on a real `--cloud-init` file that doesn't
      end in a newline. New dependency: `gopkg.in/yaml.v3`. Added real tests
      (`internal/vm/backend_test.go`) that parse the actual output back as YAML rather than
      just checking the raw string, that's what would have caught this the first time
- [x] `anvil transfer <src> <dst>`: shells out to `scp`, same connection-info resolution as
      shell/exec. Exactly one side must be `<name>:<path>`, transferring instance-to-instance
      isn't supported
- [x] `--identity`/`-i` on `shell`/`exec`/`transfer`: found for real, running `anvil` under
      `sudo` means `ssh`/`scp`'s own default identity lookup uses root's `$HOME`, not the
      real invoking user's, so it tries the wrong private key even when the right public key
      was correctly authorized. This is really a symptom of running the whole CLI as root
      pre-packaging, not something `--identity` truly fixes, just works around: the actual
      fix is to stop running `anvil` (not `anvild`) under sudo at all once there's a real
      "anvil" group to widen the socket to, which is M8 packaging work, not done yet
- [x] **anvil now manages its own SSH keypair**, instead of depending on the user's personal
      `~/.ssh/id_ed25519`. Generated once (passphrase-less `ssh-keygen -t ed25519`, on first
      use, under `$XDG_CONFIG_HOME/anvil/ssh/` or platform equivalent) and always injected
      into every launched VM by default, on top of whatever `--ssh-key` adds. `shell`/
      `exec`/`transfer` default `--identity` to this same key. Same idea as Vagrant's shared
      "insecure" keypair or Multipass's own managed key: this key only ever needs to gate
      "not a random unrelated local process", since SLIRP forwarding is host-only to begin
      with, so a passphrase would be pure friction with no real security upside. This is
      what actually makes `anvil shell <name>` work with zero flags once packaged (see the
      `--identity` entry above): once the CLI runs as the real user instead of root, its own
      `$HOME`/config dir resolves correctly and this key is just always there and always
      matches what got authorized at launch time
- [x] `anvil logs <name> [-f] [--tail N]`: new `Logs` streaming RPC. For a VM this is the
      guest's serial console output (boot messages, cloud-init's own output), not
      application logs. There's no way to see those without actually SSHing in. This only
      works going forward: `-serial null` used to just discard the console entirely, now
      it's `-serial file:<instance-dir>/console.log`; anything launched before this fix has
      no console.log to read
- [x] per-instance SSH known_hosts, kept under `~/.cache/anvil/known_hosts/<instance-id>`
      rather than the daemon's state dir or the user's own `~/.ssh/known_hosts`, since a SLIRP
      host-forwarded port is ephemeral and gets reused across unrelated instances, so
      tracking host identity by "localhost:port" the normal ssh way produces false "REMOTE
      HOST IDENTIFICATION HAS CHANGED" warnings; keying by instance ID instead means the
      same instance's real host key (tied to its persisted disk) is still checked across a
      stop/start cycle, just not confused with a different instance that happens to reuse
      the same port
- [x] `anvil mount <host-path> <name>:<guest-path>` / `anvil umount <name>:<guest-path>`,
      via 9p, not virtiofs. Checked empirically against a real QEMU 11.1.1 build rather
      than assumed: `qom-list-types` shows no user-creatable fsdev-backend object at all,
      so there's genuinely no QMP hot-plug path for a 9p share (and virtiofs would've
      needed a `virtiofsd` process this project doesn't depend on yet, plus a shared
      memory backend fixed at boot, for a capability that turns out not to be truly
      hot-pluggable either). So mounting/unmounting on an already-running instance means a
      real reboot of the guest (rebuild the cloud-init seed, stop, start) to attach or
      detach the 9p device, not a transparent hot-plug, documented plainly in the CLI's
      own help text, not hidden. Guest-side, cloud-init's `mounts` + `bootcmd` modules set
      up the fstab entry and mkdir the mountpoint; each mount-triggered reboot also bumps a
      new `Generation` field baked into the seed's instance-id, forcing cloud-init to treat
      it as a fresh instance and fully re-apply every module, since there wasn't enough
      confidence about the `mounts` module's actual default rerun-frequency to rely on
      instead. Real tests: `qemu.BuildArgs` renders the right `-fsdev`/`-device` pair, a
      real QEMU process actually stays alive with a mount attached
      (`TestSpawnWithMountRealProcess`), and `mergeMounts` round-trips through a real YAML
      parse. Not independently verified: an actual guest mounting and using the share (needs
      a real network-downloaded image plus guest 9p kernel support, both outside this
      sandbox), and whether arbitrary host-directory permissions actually work once anvild
      runs as its own unprivileged "anvil" system user post-packaging rather than root
- [x] `anvil image list` / `anvil image delete <id>`: manage the cached base-image tier
      (`internal/vm/image.Vault`'s "prepared" dir), not per-instance disks. `delete` refuses
      to remove an image still used as some instance's disk backing file (checked for real
      via `qemu-img info`'s `backing-filename`, not just guessed) unless `--force` is given,
      since deleting a backing file out from under a live overlay would corrupt that
      instance's disk. New `ImageService` RPC, new `internal/daemon/image_server.go`
- [x] cloud-init library: `anvil cloud-init list/new/edit/show/rename/delete`, backed by
      the daemon (new `cloud_init_configs` bbolt bucket), plus `--cloud-init-name` on
      launch, resolved server side in `internal/vm.Backend.Create`
- [x] runtime VM mirrors: `anvil mirror add/list/remove/enable/disable --kind vm`,
      manifest fetched and validated once at add time, cached in the `mirrors` bbolt
      bucket, merged into the catalog by priority at launch time (no network call needed
      per launch)
- [ ] mirror storage for `--kind container` exists (same CRUD, same bucket) but isn't
      applied anywhere yet, that's M3's job once a container backend actually exists
- [x] confirmed for real: `anvil launch --ssh-key ...` then `anvil shell test1 -i
      ~/.ssh/id_ed25519` gets a real interactive login shell in a real Ubuntu 24.04 guest.
      Found and fixed two real bugs to get here (the invalid-YAML cloud-init bug above, and
      the sudo/root `$HOME` identity mismatch that `-i`/`--identity` works around), both
      caught by actually testing on real hardware, not by reasoning about the code

### M3: containers, Docker first, then Podman
- [x] `ContainerSpec` gets an `engine` field (docker/podman), wired through the proto,
      daemon and CLI (`--engine`, defaults to docker, the only one implemented so far)
- [x] **Docker backend**: `--kind container` works end to end against a hand-rolled REST
      client (`internal/container/docker`), not the official SDK, see the Container
      backend section above for why. `anvil launch --kind container` supports
      `--engine`, `--env`/`-e`, `--volume`/`-v`, `--publish`/`-p`, `--entrypoint`, and a
      trailing `-- cmd args` overriding the image's own command
- [x] `anvil list`/`anvil info` show an engine column/field and container-specific
      details (image, container ID, published ports, volumes)
- [x] `anvil shell`/`anvil exec`/`anvil transfer` work against container instances too,
      via `docker exec`/`docker cp` (shelled out to the real binary, see above), not
      just VMs over SSH
- [x] **Docker registry mirrors**: `anvil mirror add --kind container --registry
      <host> --mirror-of <upstream>` now actually gets applied, not just stored. Client-
      side ref rewriting in `internal/container/docker/mirror.go`
      (`ResolveMirror`/`splitRegistryHost`), not a dockerd `daemon.json` edit, see the
      "Image catalog and mirrors" section above for why. `DockerBackend.Create` resolves
      the ref before checking/pulling/creating, and reports the rewrite through the same
      progress callback ("using mirror: nginx:alpine -> mirror.corp/nginx:alpine").
      `--insecure` is stored but not enough on its own for Docker, unlike Podman, see
      above. Real tests: `TestSplitRegistryHost`, `TestResolveMirror*` (Docker Hub
      default, explicit upstream, no-match passthrough, priority ordering)
- [ ] **Podman backend, deferred**: filippo doesn't have Podman on this machine to test
      against, so this is postponed rather than built blind. `container.Backend.Podman`
      stays `nil`, `--engine podman` already returns a clear "not implemented yet" error
      (see `internal/container/backend.go`'s `engineFor`), nothing else depends on it
      existing before M4
- [x] first real bug, found on a real Docker daemon: `anvil launch --kind container
      nginx:alpine` 404ed with "No such image", even with real network access and a
      correct image name. Root cause: `POST /containers/create` doesn't auto-pull a
      missing image the way `docker run` appears to from the CLI, it just 404s. Fixed by
      having `DockerBackend.Create` check `ImageExists` first and `PullImage` if it's
      missing (both new on the Docker client), same pull-then-create sequence the real
      `docker` CLI does under the hood. Added `TestImageExists`,
      `TestPullImageStreamsProgressAndSucceeds`, `TestPullImagePropagatesDaemonError`, and
      `TestSplitImageRef` (the repo/tag parsing has to handle a registry host with its own
      port, like `myregistry:5000/nginx:alpine`, without mistaking that colon for the tag
      separator), all real tests against a mock server, not just reasoned about
- [x] `anvil launch` shows real provisioning progress instead of going silent: image
      download percentage for VMs, per-layer pull status for Docker, both throttled to
      avoid spamming the terminal but never silent for more than half a second, see the
      API layer section above for how this is wired
- [x] second real bug, caught on filippo's first real image download: printing every
      progress tick as its own line scrolled the terminal with hundreds of near-identical
      lines. Fixed by redrawing in place on a real terminal instead (carriage return,
      grouped by a status "key"), see the API layer section's Provisioning progress notes
- [ ] still not fully verified against a real daemon beyond the one launch above: this
      sandbox itself has no root/rootless docker tooling to run `dockerd`, so everything
      here is as verified as it can be in here (real unit tests, real `go build`/`gofmt`
      checks) except for whatever filippo runs for real on his end

### M4: intents
- [x] intent data model, own bbolt bucket (`internal/store/intents.go`), references
      instance IDs not embedded specs
- [x] intent-aware launch: `InstanceService.Launch`'s existing `intent_name`/`role`
      fields (previously unused) now actually route through
      `internal/intent.Manager.Launch`, auto-creating the intent on first use, tagging
      the instance's `Labels["intent"]`/`Labels["role"]`
- [x] `anvil launch --intent NAME --role ROLE` (works for both `--kind vm` and
      `--kind container`)
- [x] `anvil intent create/add/list/info/remove/delete`, create/add are CLI-side sugar
      over `Launch`, list/info/remove/delete are a real `IntentService` (no separate
      Create/Add RPC, see the Intents section above for why)
- [x] `anvil info` shows an instance's intent/role when it has one
- [x] **shared per-intent network**: a Docker bridge network per intent
      (`internal/container/docker/network.go`, `internal/container.DockerNetworker`,
      `internal/intent.Manager.ensureNetwork`), created lazily on the first member's
      launch. Container members get `NetworkMode` set to it; VM members get a tap device
      attached to its bridge interface (`internal/vm/network`, new
      `github.com/vishvananda/netlink` dependency) plus a statically-assigned address
      (`internal/intent/ipam`) and a generated cloud-init network-config, instead of
      SLIRP. `anvil shell`/`exec`/`transfer` connect to a bridged VM's real address
      instead of a SLIRP-forwarded port. Podman deferred, so this is Docker-only for
      now, see the Intents section above for the full design and, importantly, exactly
      what's unverified about it
- [x] `internal/intent/ipam`'s subnet/address math is real, tested code (6 tests,
      deterministic hashing, collision-avoiding container/VM address split, out-of-range
      rejection); everything downstream of it (the actual netlink calls, Docker's
      bridge-name option, real guest connectivity) is not, see the Intents section
- [ ] confirm intents are actually isolated from each other by default (each intent gets
      its own bridge/subnet by construction, so this should already hold, but it's not
      been checked against two real intents at once)
- [x] `anvil intent delete` now removes the intent's Docker network too, not just the
      group record, best-effort (Docker refuses to remove a network with members still
      attached, see the Intents section's "Tearing the network down" for the details)
- [x] third real bug, spotted by filippo before it even shipped: deleting a member
      instance directly (`anvil delete <name>`, not through any `intent` command) left a
      stale entry behind in its intent's `Members` list, pointing at an instance ID that
      no longer existed. Fixed in `internal/daemon/server.go`'s `Delete` handler: it now
      resolves each instance's `Labels["intent"]` before deleting (labels aren't
      retrievable afterward), then best-effort removes it from that intent via
      `intent.Manager.Remove`, logged but not fatal to the delete itself if that fails
- [ ] none of this has touched a real daemon yet, same as M3's Docker work before
      filippo's own hardware confirmed it. Proto validated with real `protoc`, code is
      gofmt-clean and manually reviewed, that's the limit of what's verifiable here. The
      networking piece specifically also couldn't fetch or exercise its new netlink
      dependency at all in this sandbox, more unverified than anything built so far

### M5: migration, single instance
- [ ] known hosts: `anvil host add/list/remove/test`
- [ ] mDNS discovery, advertise and browse, in-memory peer table separate from known hosts
- [ ] `anvil migrate <name> --to <host> --copy` (non-destructive first, it's simpler)
- [ ] `anvil migrate <name> --to <host>` (destructive, copy-verify-delete)

### M6: migration, intents
- [ ] migrate a whole intent as one unit
- [ ] abort and roll back the target if any member fails
- [ ] `--best-effort` mode that keeps whatever succeeded

### M7: TUI
- [ ] `tview` app shell, nav list plus pages
- [ ] instances table
- [ ] launch form
- [ ] cloud-init view (list panel plus editor panel)
- [ ] mirrors view
- [ ] migration view
- [ ] shell/SSH handoff (suspend TUI, exec real session, resume TUI)

### M8: packaging
- [ ] pick and add a LICENSE file (Apache-2.0)
- [ ] two-package PKGBUILD: `anvil`, `anvild`
- [ ] `anvild.service` systemd unit, sysusers/tmpfiles rules
- [ ] shell completions, man pages
- [ ] a real `makepkg`/`namcap` run, not just a paper design

## Things we already know are unresolved

- Cross-kind networking (a VM and a container in the same intent sharing a bridge) is
  designed but not yet tested against a real Podman/netavark setup, and not even designed
  yet for Docker's bridge networks.
- Whether an intent can mix Docker containers and Podman containers together is punted on
  for now, see the Intents section, current plan is "don't allow it" until there's a real
  reason to support it.
- The final default engine (once both exist) is meant to be Podman, matching the original
  spec, but that's not enforced by any code yet since only Docker exists as of M3's start.
- Migrating a container's arbitrary host bind mounts doesn't really work, only anvil
  managed volumes get copied. `--dry-run` is supposed to warn about this, not silently
  drop data.
- No bootstrap-over-SSH for migration: the target machine needs `anvil` already installed.
- The distro manifest schema has a `schema_version` field so we can change it later
  without silently breaking old manifests, but we haven't needed to bump it yet.
