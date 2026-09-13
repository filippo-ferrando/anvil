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
anvil (CLI + TUI, one binary) --- gRPC over a unix socket ---> anvild (daemon, dedicated "anvil" system user)
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
    migrate/                           SSH-driven single-instance migration; payload/ (JSON wire shape)
    discovery/                         mDNS host discovery (deferred, not built)
    store/                             bbolt-backed registry
    config/                            fixed on-disk paths (/run/anvil, /var/lib/anvil, etc.)
    cli/commands/                      cobra commands
    tui/                               tview app (not built yet)
  data/distros/                        embedded default VM image catalog
  packaging/                           PKGBUILD, systemd unit, sysusers/tmpfiles rules
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

`--publish host:guest[/tcp|udp]` on `anvil launch` now works for a standalone VM the same
way it already did for containers (same flag, same `PortMapping` message, `VMSpec.ports`
on the wire): each one becomes another SLIRP hostfwd entry alongside the SSH one, so
something like nginx installed directly inside the guest (not in a container) is reachable
from the host. Doesn't apply to a bridged intent member, since that VM already has its own
directly-reachable address on the shared network, nothing to forward. `anvil launch`
rejects `--publish` together with `--intent` on a VM for exactly that reason instead of
silently ignoring it.

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

**Confirmed for real**: filippo has run all of this on his own machine, networking piece
included (the `vishvananda/netlink` dependency and all). This was written and reviewed
in a sandbox that can't run `anvild` at all (no root, no `/dev/kvm` reliably, no Docker
daemon), so at the time it was the least-verified code in the whole project; that's no
longer true.

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
- **Confirmed for real, on filippo's own machine**: the `github.com/vishvananda/netlink`
  dependency (couldn't even be fetched in the sandbox this was written in), Docker
  honoring `com.docker.network.bridge.name`, the IPAM `IPRange` split, a VM on the bridge
  actually reaching a Docker container on the same network, and cloud-init's
  network-config applying a static IP on a real guest boot. This was the one piece of M4
  that most needed a real smoke test before trusting it, and it's had one.
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
- **Name resolution**: filippo asked whether members can resolve each other by name, like
  a Docker internal network. Container-to-container already worked for free (Docker's own
  embedded DNS on the shared network), the only gap was that it only resolved anvil's
  internal container name (`anvil-<instance-name>`), not the plain role. VMs couldn't
  resolve anything by name at all, they're not Docker-managed, invisible to that DNS.
  Went with the static-hosts option (there was also a bigger "run our own per-intent DNS
  server" option on the table, deliberately not chosen: a real per-intent process anvil
  would have to manage, unnecessary complexity for now):
  - Every container member now also gets a Docker network alias equal to its role
    (`CreateContainerParams.NetworkAlias`, via Docker's own
    `NetworkingConfig.EndpointsConfig[network].Aliases`), so peers resolve it as `web`,
    not `anvil-web`. Free for other containers (Docker's DNS already does this), doesn't
    help a VM peer at all.
  - Every VM member gets `/etc/hosts` entries (`VMSpec.ExtraHosts`, a role -> IP map)
    injected via cloud-init `bootcmd` (idempotent, grep-guarded lines, since cloud-init
    has no first-class "static hosts" module in the minimal config surface this project
    generates) for every other already-known member, VM or container, since a VM has no
    DNS mechanism of its own to fall back on.
  - Every container member also gets `HostConfig.ExtraHosts` (`--add-host` equivalent)
    entries (`ContainerSpec.ExtraHosts`), but only for VM peers, container peers are
    already covered by Docker's own DNS via the alias above, so adding them again would
    just be redundant.
  - A container's own address on the network isn't known until after Docker creates it
    (Docker's IPAM assigns it), so `internal/intent.Manager.Launch` reads it back right
    after via a new `Networker.ContainerAddress` method (Docker's
    `GET /containers/{id}/json`, `NetworkSettings.Networks[name].IPAddress`) and records
    it in the new `store.IntentMember.IP` field. A VM's address is already known ahead of
    time (the same static assignment used for its own network-config), no lookup needed.
  - **Same staleness caveat as everything else in this design**: a member's hosts/alias
    setup reflects the intent's membership as it existed at that member's own launch
    time. A VM launched before a later member joins won't resolve it until restarted; a
    container's `ExtraHosts` has the same limitation (Docker doesn't support changing
    `--add-host` on a live container without recreating it). Not solved here, consistent
    with the tradeoff already flagged before picking this design over a real DNS server.

### Migration

Single-instance migration is done (milestone 5). A whole intent (milestone 7, moved down
from 6 so packaging could move up, see the roadmap's M6) is not.
`anvil migrate <name> --to <alias|user@host[:port]> [--copy] [--dest-name NAME]
[--dry-run]`.

The trick, exactly as originally planned: the source daemon doesn't talk to the target
daemon directly over gRPC. It SSHes into the target host and drives the target's own
local `anvil` CLI, which talks to the target's own local `anvild` over its own unix
socket. This sidesteps the whole "how do two daemons trust each other over the network"
problem, we just reuse whatever SSH access you already have. Concretely: the source
daemon shells out to the real `ssh`/`scp` binaries (`internal/migrate/ssh.go`), same
reasoning as the CLI's own shell/exec/transfer using real `ssh` instead of a Go SSH
library, no new runtime dependency either.

**Known hosts** (`anvil host add/list/remove/test`, `internal/store/hosts.go`, a plain
`HostService`): a saved alias for a `user@host[:port]` plus an optional identity file.
Adding one grants no trust by itself, it's just a shortcut so `--to` doesn't need a
literal address every time. `anvil host test <alias>` actually SSHes in and checks
`anvil`/`anvild` are on PATH, not just that the alias is saved. `anvil migrate --to` also
accepts a literal `user@host[:port]` directly, no saved host required.

**mDNS auto-discovery of peer anvil hosts, from the original plan, is deliberately
deferred, not built.** It's a real, hand-rollable-with-stdlib feature (Go's `net` package
supports multicast UDP directly), but a correct mDNS/DNS-SD implementation is
non-trivial, and the plan itself says discovery "doesn't grant any trust by itself": it's
a convenience for finding a host's address, not something migration actually depends on.
Known hosts alone are enough for `anvil migrate` to work. Revisit if it's actually missed
in practice, same call as Podman and the "real DNS server per intent" option earlier.

**The actual migration mechanism** (`internal/migrate`, a new `MigrateService`):
1. Stop the source instance first, unconditionally (a VM's disk can't be safely
   flattened while a live QEMU process might still be writing to it).
2. Build a plain JSON payload of everything needed to relaunch it
   (`internal/migrate/payload`, deliberately independent of the proto/domain types).
   This is what actually crosses the wire to the target, over SSH's stdin, never as
   command-line arguments (arbitrary spec content, image refs, env values, would need
   careful shell-quoting that a fixed, argument-free remote command sidesteps entirely).
3. **VM**: flatten the disk (`qemu-img convert`, a new `internal/vm.Backend.ExportDisk`)
   into a standalone qcow2, `scp` it to the target's `/tmp`, and reference that path in
   the payload.
4. **Container**: no disk/image transfer at all. The payload just carries the
   `ContainerSpec` (image ref, env, volumes, ports, engine), and the target re-pulls the
   image itself via the exact same M3 pull-if-missing logic. This only really works for a
   registry-hosted image, not one that only ever existed as a local build on the source,
   an accepted v1 limitation, matches what the plan already flagged for container export.
5. SSH to the target and run a fixed command, `anvil migrate-import` (a new, hidden CLI
   command, plumbing, not meant to be run by hand), piping the JSON payload via stdin.
   It relaunches the instance through a completely normal `Launch` call against its own
   local anvild, and prints a final `MIGRATE_OK <id>`/`MIGRATE_FAIL <message>` line the
   source parses to know whether it actually worked; every other line in between is just
   forwarded progress.
6. Only once the target confirms success does the source get deleted (skipped
   entirely with `--copy`): copy, verify, then delete, never delete-first, exactly as
   planned. Any failure before that point leaves the source stopped but otherwise
   untouched, nothing is lost.

**A migrated VM skips cloud-init entirely** (new `VMSpec.source_disk_path` /
`instance.VMSpec.SourceDiskPath`, and a new `internal/vm.Backend.adoptMigratedDisk` path
in `Create`): the transferred disk already has everything from its original first boot
baked in (users, SSH keys, packages), and re-running cloud-init against a new
instance-id risks re-applying modules that aren't all idempotent. Simpler and safer to
just boot the disk as-is with no seed at all, rather than try to reason correctly about
which cloud-init modules are safe to rerun.

**Nothing about this has touched two real machines yet.** This was written and reviewed
in a sandbox with no second host to actually SSH to, no way to test a real `scp` transfer
landing correctly, no way to confirm a flattened qcow2 actually boots on another machine,
and no way to verify `anvil migrate-import`'s stdout-parsing protocol survives a real SSH
session (buffering, escape sequences, etc.). This needs a real two-machine smoke test
before trusting it: more than anything else built so far, this is code that looked right
on review but has had zero real execution.

### Cloud-init config library

Done (milestone 2). `anvil cloud-init new/edit/rename/delete/list`, saved server side
(`cloud_init_configs` bbolt bucket) so the CLI and TUI both see the same set of saved
configs, then referenced by name at launch time (`--cloud-init-name <name>`).
`--cloud-init <file|->` remains as a lighter-weight ad hoc escape hatch, not saved to the
library.

### TUI

Not built yet (milestone 8, moved down from 7 so packaging could move up, see M6 in the
roadmap). Using `tview` (built on `tcell`). Rough shape: a nav list on the left, a table
of instances, a launch form, a cloud-init list-plus-editor view, a mirrors table, a
migration flow. SSH/shell access from the TUI works by suspending the TUI and handing
the real terminal over to an actual `ssh`/`docker exec`/`podman exec` session, then
resuming the TUI when you exit, same trick tools like k9s and lazygit use. Way simpler
than trying to build a terminal emulator widget.

### Packaging

Done, milestone 6, moved up from 8 (originally the very last milestone) specifically so
filippo could test the project on other machines via a real package instead of manually
copying a Go build around every time; see the roadmap's M6 for what actually shipped.
Two packages, `anvil` (CLI) and `anvild` (daemon), `packaging/PKGBUILD` +
`anvild.service` + `anvil.sysusers`/`anvil.tmpfiles`. A few things worth calling out that
changed from the original plan along the way:
- **No pure-Go SSH client, no `avahi`/mDNS dependency either**: both were originally
  planned dependencies for this milestone, and both ended up not needed at all: SSH
  duties throughout the project shell out to the real `ssh`/`scp` binaries instead (see
  the Migration section), and mDNS host discovery was deliberately deferred rather than
  built (also see Migration). `anvil`'s package still depends on `openssh` for exactly
  that reason, just as a runtime dependency instead of a Go one.
- **`anvild` does run as the dedicated unprivileged "anvil" system user, matching this
  section's original plan**, via `SupplementaryGroups=kvm` plus `AmbientCapabilities=
  CAP_NET_ADMIN` in `anvild.service`, and `anvild.install` conditionally granting Docker
  group membership. Getting there took more than the plan originally sketched, though,
  and changed real runtime behavior (`anvil mount` now needs the "anvil" user to actually
  have access to whatever host directory you share, and Podman's eventual rootful
  assumption no longer holds); see the M6 roadmap entry for the full story.
- License is Apache-2.0, unaffected by any of the above: that decision was about not
  needing to port GPLv3 Hyperpass GUI code once the client became a clean-room TUI, and
  stands regardless of the SSH/mDNS/root changes.

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
      "anvil" group to widen the socket to, which M6 packaging now actually provides (a
      real user still needs `usermod -aG anvil <user>` to use that group, though, this
      doesn't happen automatically for anyone but the daemon's own service account)
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
- [x] **confirmed for real**: filippo has run all of M3 against his own real Docker
      daemon. Everything above is no longer just "as verified as it can be in a sandbox
      with no dockerd." It's actually been launched, pulled, shelled into, and mirrored
      for real.

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
- [x] **confirmed for real**: filippo has run all of M4 on his own machine, netlink
      dependency included. The shared network, tap-attach, static VM addressing, and the
      three real bugs above all held up outside this sandbox, not just in review. That
      confirmation predates M6's non-root daemon, though: M6 introduced a real
      regression here (tap creation cgroup-denied, see M6's `DeviceAllow=/dev/net/tun`
      note), now fixed.
- [x] **name resolution**: container members now also get a Docker network alias equal
      to their role (peers resolve `web`, not `anvil-web`, via Docker's own embedded DNS,
      which already worked, this just fixes the name). VM members get `/etc/hosts`
      entries for every other already-known member (any kind, since a VM has no DNS
      mechanism of its own), injected via cloud-init `bootcmd`; container members get
      `--add-host`-equivalent entries for VM peers specifically (container peers are
      already covered by the DNS alias). New `store.IntentMember.IP` and
      `Networker.ContainerAddress` (Docker: reads back the engine-assigned IP right after
      create) to make this possible. Same staleness caveat as the rest of M4's network
      design: reflects membership as of each member's own launch time, not retroactively
      updated when a later member joins. See the Intents section's "Name resolution" note
      for the full design and why a real per-intent DNS server wasn't picked instead
- [ ] not build tested against a real daemon yet (this is brand new, added after the rest
      of M4 was already confirmed). Proto validated with real `protoc`, the Docker
      client changes have real tests (`TestCreateContainerNetworkAliasAndExtraHosts`,
      `TestContainerNetworkAddress`, 4 new tests total, all passing), the cloud-init
      `bootcmd` generation has real tests too (`internal/vm/backend_test.go`,
      untestable in this sandbox specifically but written the same way the already-
      confirmed `mergeSSHKeys`/`mergeMounts` tests were)
- [x] `--publish host:guest[/tcp|udp]` now also works for a standalone VM, not just
      containers (same `PortMapping` message, reused for `VMSpec.ports`): an nginx
      install running directly inside a cloud-init guest is now reachable the same way a
      container's published port is. Doesn't apply to a bridged intent member (it already
      has a directly-reachable address), `anvil launch` rejects that combination outright

### M5: migration, single instance
- [x] known hosts: `anvil host add/list/remove/test` (`internal/store/hosts.go`, a plain
      `HostService`, `test` does a real SSH connectivity + anvil/anvild-on-PATH check)
- [ ] mDNS discovery: deliberately deferred, not built. Known hosts alone are enough for
      migration to work, and discovery grants no trust by itself either way, see the
      Migration section above for the reasoning
- [x] `anvil migrate <name> --to <alias|user@host[:port]> --copy` (non-destructive) and
      the destructive default (copy, verify the target has it, then delete the source,
      never delete-first) both work the same way, one `MigrateService.Migrate` RPC with
      a `copy` flag, not two separate code paths
- [x] `--dry-run`: checks the target is reachable and has anvil installed, reports the
      plan, transfers nothing
- [x] `--dest-name`: optional rename on the target
- [x] VM migration: flatten disk (`qemu-img convert`, new `Backend.ExportDisk`), `scp` it
      over, relaunch via a new `--from-disk`/`--default-user` pair on `anvil launch`
      (`VMSpec.source_disk_path`) that adopts the disk directly and skips cloud-init
      entirely, since the disk already has everything from its original first boot. New
      `internal/migrate/payload` package carries the relaunch spec over SSH as JSON via
      stdin (never as shell arguments, to sidestep quoting arbitrary content), to a new
      hidden `anvil migrate-import` command that's the actual "target's own local anvil
      CLI" the plan describes
- [x] container migration: no disk/image transfer at all, just the `ContainerSpec` (image
      ref, env, volumes, ports, engine) relaunched via the same `migrate-import` path; the
      target re-pulls the image itself through the existing M3 pull-if-missing logic.
      Only actually works for a registry-hosted image, matching the plan's own
      already-flagged limitation for container export
- [ ] **zero real two-machine testing**: everything above was written and reviewed with
      no second host to actually SSH to in this sandbox. Proto validated with real
      `protoc`, code is gofmt-clean and manually reviewed, that's the limit of what's
      verifiable here. This is the least-verified thing built so far and needs a real
      smoke test across two real machines before it's trusted, more than M3's Docker work
      or M4's networking did before they were each confirmed

### M6: packaging
Moved up from M8, ahead of intent migration and the TUI: filippo wants a real Arch
package now specifically so testing this on other machines is a `makepkg -si` instead of
a manual `go build`/copy-around every time. See the "Arch Linux packaging" section above
for the details behind each item below.
- [x] LICENSE (Apache-2.0), already added (`LICENSE.md`), before this milestone even
      started
- [x] two-package PKGBUILD (`packaging/PKGBUILD`): `anvil` (CLI, depends on `openssh`
      for shell/exec/transfer/migrate) and `anvild` (daemon, depends on `qemu-base`
      + `xorriso` + `openssh`, `docker`/`podman` as optdepends). Builds straight from
      the working tree (`packaging/` is one directory below the repo root, no release
      tarball exists yet, this is for testing an in-progress build on a second machine,
      not distributing a pinned version). Recommended build is a clean chroot
      (`extra-x86_64-build`, from `devtools`), not bare `makepkg`, per filippo's own
      request, so `depends`/`makedepends` actually get verified instead of assumed
- [x] **`anvild` runs as the dedicated unprivileged "anvil" system user, not root**: see
      below for the full permission story, this took real engineering, not just flipping
      `User=` in the service file, and it changes real behavior (see the `anvil mount`
      caveat below)
- [x] `anvild.service`, `anvil.sysusers`, `anvil.tmpfiles`, `anvild.install` (a real
      pacman post_install/post_upgrade hook, not just static config files)
- [x] shell completions (bash/zsh/fish), generated at package time from cobra's own
      built-in `completion` subcommand, no extra code needed
- [ ] man pages: deferred. `cobra/doc`'s `GenManTree` needs
      `github.com/cpuguy83/go-md2man/v2`, a genuinely new dependency that couldn't be
      fetched in this sandbox (same limitation as every other new dependency this
      project has added, see `github.com/vishvananda/netlink` in M4); add it and wire
      up a small `gendoc`-style generator once there's network access to do that for real
- [x] real bug, caught the first time filippo actually ran `extra-x86_64-build`:
      `source=()` was empty and `build()` just did `cd "$startdir/.."`, which only ever
      worked for bare `makepkg` running directly on the host filesystem. A real isolated
      chroot build only copies the invocation directory (`packaging/`) into the chroot,
      never its parent, so the whole Go module tree (go.mod, cmd/, internal/, ...) simply
      wasn't there, `go build` failed with "go.mod file not found". Fixed by declaring
      `source=("anvil::git+file://${startdir}/..")`: makepkg's own source-fetch
      machinery clones the parent repo (its last commit, not uncommitted changes, see the
      PKGBUILD's own comment on that) into `$srcdir/anvil` before chroot isolation even
      starts, which both bare `makepkg` and a real chroot build handle identically
- [x] `anvil mount`'s permission-denied error now includes real, copy-pasteable
      `setfacl` commands (see the mount-permission note above)
- [x] `anvil create-dir <path>`: makes a host directory and runs those same `setfacl`
      grants itself, instead of leaving them to copy-paste (see the note below)
- [x] `anvil migrate-key`: prints anvild's own migration SSH public key, generating one
      on the spot if it doesn't exist yet (see the migration-key note below)
- [x] fixed a real, unrelated CLI bug noticed along the way: every error was printed
      twice, once by cobra itself (`Error: ...`) and once by `cmd/anvil/main.go`
      (`anvil: ...`), since the root command had `SilenceErrors: false`. Set to `true`;
      `main.go` was already the single source of truth for printing a returned error
- [x] **a real, second consequence of the non-root daemon, caught on filippo's own first
      `anvil launch` into an intent post-change**: tap device creation failed with a
      plain `operation not permitted`, nothing to do with `CAP_NET_ADMIN` at all. Adding
      `DeviceAllow=/dev/kvm rw` to the unit silently flipped systemd's
      `DevicePolicy=auto` from "allow every device" to "deny everything except what's
      explicitly listed" (any `DeviceAllow=` entry does this, see
      systemd.resource-control(5)), so `/dev/net/tun` was cgroup-denied underneath the
      capability check, which never even got reached. Fixed by adding
      `DeviceAllow=/dev/net/tun rw` alongside the kvm one. Also fixed a now-stale doc
      comment on `internal/vm/network.CreateTap` that still said "anvild already runs
      as root," left over from before this milestone
- [x] **`anvil migrate`'s disk upload used to look hung**: `scpUpload` shells out to a
      real `scp`, capturing its stderr into a buffer so a real error message survives,
      but `scp`'s own progress meter only ever prints to a real terminal (checks
      stderr's isatty), so a multi-hundred-MB/GB qcow2 transfer produced zero output
      for however long it took. Fixed by running the transfer in the background and
      having `scpUpload` itself push a heartbeat line through the existing `progress`
      callback every 5 seconds (elapsed time, plus the file's size up front for scale),
      not real byte-level progress, `scp`'s own meter isn't there to parse in a non-tty,
      but enough to show it's still alive instead of looking stuck
- [ ] a real `makepkg`/`namcap`/`extra-x86_64-build` run: this sandbox has the `makepkg`
      binary present but no actual Arch build environment (`/etc/makepkg.conf` and
      `/etc/pacman.conf` don't exist here) and no `devtools` for a chroot build either,
      so the only verification possible here was `bash -n PKGBUILD`/`sh -n
      anvild.install` (real syntax checks, both pass) and careful manual review. The
      actual chroot build is squarely what filippo needs to do on a real machine, which
      is the whole point of this milestone

**Running as a dedicated unprivileged user, not root**: filippo explicitly didn't want
anvild running as root, so this got done properly instead of shipped as an aspiration
(the previous pass's `anvild.service` claimed root "for now," this pass replaces that).
Three separate mechanisms, not one, get anvild everything it actually needs without
being root:
- **`/dev/kvm`**: `SupplementaryGroups=kvm` in the unit, same as any desktop QEMU/libvirt
  setup on Arch (udev owns the device as `root:kvm 0660` already), safe to declare
  statically since `qemu-base` is a hard dependency, so the "kvm" group is guaranteed to
  exist by the time the unit starts.
- **tap/bridge creation** for an intent's shared network (`internal/vm/network`, real
  netlink calls): `AmbientCapabilities=CAP_NET_ADMIN` / `CapabilityBoundingSet=CAP_NET_ADMIN`
  in the unit, so the capability survives across exec despite the process not being
  root.
- **the Docker socket**: deliberately *not* a static `SupplementaryGroups=docker` in the
  unit, since Docker is an optional dependency and might not be installed (and its group
  might not exist) when the unit loads, which would fail the whole service to start
  outright. Instead, `anvild.install`'s `post_install`/`post_upgrade` conditionally runs
  `gpasswd -a anvil docker` if the group exists at that moment, with an on-screen note
  telling the user to do it manually (plus `systemctl restart anvild`) if they install
  Docker later. Being in the "docker" group is well-known to be root-equivalent trust in
  practice (a container can bind-mount the host filesystem), an accepted, documented
  tradeoff of talking to Docker directly at all, not something specific to this change.

**A real consequence, not just a permissions checkbox**: `anvil mount` sharing an
arbitrary host directory into a VM now actually depends on the "anvil" user having
access to that directory. QEMU's 9p backend does that file I/O as whatever user spawned
it (anvild's own child process, no privilege escalation happens for VM spawning), so
unlike the old root-anvild design, a mount into `/home/someone/private-project` will
fail unless that directory is actually readable (and, for a non-read-only mount,
writable) by "anvil": group permissions or an ACL, not automatic anymore. This is a
real, load-bearing behavior change from the security improvement, not a hypothetical
one, and it's not solved here (there's no code fix that makes an unprivileged user able
to read arbitrary other users' files, that's the whole point of the permission model).

Caught in the wild the very first time filippo tried a real mount post-change: a plain
`stat: permission denied`, no indication of why or what to do about it. Fixed in
`internal/vm.Backend.Mount`: a permission-denied stat now gets a hint appended, real
`setfacl` commands for every ancestor directory in the path (traverse only) plus the
target itself (full rwx, and a default ACL so new files created inside later inherit it
too), since `stat(2)` failing with `EACCES` specifically means some ancestor lacks search
permission, not the target itself, this covers the whole chain rather than guessing
which directory is the actual blocker.

That ancestor-walking logic now lives in its own package, `internal/hostpath`, pure
stdlib (`os/user`, `os/exec`, `path/filepath`), pulled out of `internal/vm` on purpose so
the CLI can call it directly without dragging in `internal/vm`'s whole dependency tree.
Two entry points share it: `hostpath.Hint` just builds the message text (what
`Backend.Mount` appends to its error), `hostpath.Grant` actually runs the commands. That
second one is what powers the new `anvil create-dir <path>` command: `mkdir -p` plus the
same grants, so filippo doesn't have to copy-paste `setfacl` lines by hand before every
mount of a fresh directory. `create-dir` runs entirely client-side, no daemon RPC
involved, since `mkdir`/`setfacl` are ordinary filesystem operations the invoking user
can already do without anvild's help. `hostpath.Grant` no-ops (not an error) when the
"anvil" system user doesn't exist on the machine at all, which is the normal shape of the
manual `sudo ./anvild` dev workflow that predates packaging. Real tests, and unlike the
rest of `internal/vm` this package has zero non-stdlib imports, so it's actually run in
this sandbox, not just hand-traced: `TestHintCoversEveryAncestor`,
`TestHintTrailingSlash`, `TestAncestorsExcludesRootAndTarget`,
`TestGrantSkipsWhenAnvilUserMissing` (the last one overrides the package's user-lookup
var rather than trusting this machine to actually lack an "anvil" user, since it turns
out this particular dev machine has one already from earlier packaging tests), all four
pass.

**A real open question this raises for the Podman backend (still deferred, not built)**:
the original design assumed "anvild already needs to run privileged for KVM access, so
running Podman rootful too isn't a new requirement": that assumption no longer holds
now that anvild isn't privileged. Podman's rootful REST socket doesn't have a
Docker-group-style shared-access convention the same way `/var/run/docker.sock` does.
When Podman actually gets built, this needs revisiting: either rootless Podman (a real
architecture difference, per-user instead of system-wide), or whatever access model
Podman itself supports for a non-root client. Not resolved now, just flagged honestly
since it's a direct consequence of this change.

**anvild's own passwordless migration SSH key**: since anvild no longer runs as root (or
as whatever user invoked `sudo anvil`), it needs its own identity for `anvil migrate`'s
outbound SSH connections, generated once by `anvild.install` at
`/var/lib/anvil/.ssh/id_ed25519` (StateDir doubles as the "anvil" user's actual home
directory now, see `anvil.sysusers`), passwordless, printed on-screen so filippo can
copy the public half into a target host's `~/.ssh/authorized_keys`. `anvil host add`'s
own `--identity`/`-i` flag still overrides this per-host when given; this is just the
default. `internal/migrate/ssh.go` passes this explicitly via `-i` rather than relying
on ssh's own default `$HOME`-based identity resolution, the exact same fix already
applied once for a near-identical bug on the CLI side (`internal/cli/commands/ssh.go`,
M2's `sudo`/root `$HOME` bug), not something worth risking twice.

Generation isn't purely `anvild.install`'s job, though: `internal/migrate.Manager` also
lazily generates the same key itself (`EnsurePublicKey`, called at the start of both
`Migrate` and `CheckHost`) if it doesn't already exist, mirroring
`ensureDefaultAnvilKey` on the CLI side exactly. This matters because this project has a
whole documented workflow for running `anvild` manually during dev/testing (`sudo
./anvild`, see the README) that never goes through `anvild.install` at all: without
this, `anvil migrate` would just fail confusingly the first time in that setup, instead
of working the same way regardless of how `anvild` got started. `anvil migrate-key`
(new CLI command, plain `MigrateService.Key` RPC) prints whatever key this resolves to,
generating one on the spot if needed, so there's a real command for "give me the key to
paste into a target's `authorized_keys`" instead of finding it manually on disk (which,
being owned by the unprivileged "anvil" user, you likely can't even read directly
without `sudo cat` anyway).

### M7: migration, intents
- [x] migrate a whole intent as one unit: `anvil migrate <name|intent> --to ...`
      resolves `name` as a single instance first, a whole intent second (same as
      before, just extended: neither namespace is new). No new network-recreation
      logic needed on the migration side at all: each member is transferred through
      the exact same `migrateSpec` a single-instance migration already used, just
      carrying the intent's own name and that member's role along in the payload,
      since `anvil migrate-import`'s existing Launch call already knows what to do with an
      intent name (that's M4's `intent.Manager.Launch`, joining/creating the
      same-named intent on the target and standing up its shared network there, on
      whichever member arrives first). One real interface change this needed:
      `Instances.Info` (used to decide "is this name an instance") errors out
      instead of returning an empty slice when nothing matches, and the dispatcher
      was initially written to bail out on that error, which silently broke the
      "fall back to an intent lookup" path entirely; fixed by ignoring that
      particular error and treating it as the signal to try the intent lookup
- [x] abort and roll back the target if any member fails: default mode is
      all-or-nothing: the first member that fails to migrate stops the loop there,
      whatever already landed on the target gets deleted via a new hidden command,
      `anvil migrate-rollback` (reads a JSON array of names from stdin, same
      argument-free pattern as `migrate-import`, force-deletes them through the
      target's own local anvild), and every source member is left exactly as
      stopping it left it, never deleted, since a source member only ever gets deleted
      after its own migration is independently confirmed successful, so "roll back"
      here just means "don't touch the source," there's nothing to undo there
- [x] `--best-effort` mode that keeps whatever succeeded: it skips the abort/rollback
      above, keeps migrating every member regardless of earlier failures, deletes
      only the ones that actually succeeded from the source, and reports exactly
      which is which. Rejected outright for a single instance (`--best-effort` only
      makes sense for a group)
- [x] proto additions: `MigrateRequest.best_effort`; `MigrateProgress` gained two new
      oneof cases, `member_done` (one `MigrateMemberResult` per intent member) and
      `intent_done` (the final `IntentMigrateDone` summary: intent name, every
      member's result, whether it was rolled back), both sent together right after
      the whole group has been attempted, not literally as each member finishes
      (`Manager.migrateIntent` only streams plain status lines mid-flight, not
      structured per-member events)
- [x] **a real gap caught mid-implementation, not in the original plan**: a migrated
      VM's disk skips cloud-init entirely on relaunch (see the Migration section
      above), which means the *destination* host's own default anvil SSH key (the
      one its own `anvil launch` would normally bake in, see
      `resolveSSHKeys`/`ensureDefaultAnvilKey`) never gets into the guest at all,
      only whichever key the *source* host originally launched it with is in there.
      Without a fix, the destination's own `anvil shell`/`exec`/`transfer` would have
      no way into a VM migrated from elsewhere. Fixed, and deliberately fixed
      client-side, not in the daemon: `anvild` itself has no guest-access identity of
      its own at all (`VMSpec.SSHPublicKeys` is purely client-supplied, see
      `internal/daemon/convert.go`), so the daemon can't inject anything into a guest
      even if it wanted to; only the CLI, running as whatever OS user launched (or is
      migrating) the VM, has that. New flow, all in `internal/cli/commands/migrate.go`,
      before the streaming `Migrate` RPC is even called (skipped entirely on
      `--dry-run`):
      1. Resolve `name` to its VM(s) the same way the daemon does (single instance,
         or every VM member of an intent (a container is simply skipped, no
         SSH/cloud-init concept applies).
      2. Ask the daemon for the destination's own default guest-access key via a new
         RPC, `MigrateService.GuestKey`. The daemon SSHes to the target over the
         *already-established* host-to-host channel (no new trust) and runs a new
         hidden command there, `anvil migrate-guest-key`, which just prints (and
         generates on first use) the invoking identity's own
         `ensureDefaultAnvilKey()` public half, the exact key a normal `anvil
         launch` run as that same identity on that host would already bake in.
      3. SSH directly into each still-running source VM (client-side, using
         whatever identity is already authorized there (the same connection
         resolution `anvil shell`/`exec` already use) and append the fetched key to
         `~/.ssh/authorized_keys`, idempotently (checked via `grep -qxF` first, so
         migrating the same instance twice doesn't pile up duplicate lines). The key
         travels over SSH stdin, never interpolated into the remote command line,
         same reasoning as `migrate-import`/`migrate-rollback`'s own stdin-JSON
         payloads.
      A VM that's already stopped when `anvil migrate` runs gets a warning instead of
      a hard failure (there's genuinely nothing to SSH into), everything else is a
      hard error before any actual transfer starts: cheap to check upfront, and far
      better than discovering it only after an expensive disk copy finishes.
- [x] **a second real gap, also caught mid-implementation**: migrating a whole intent
      one member at a time, each relaunching independently on the target, means each
      one lands on whatever fresh subnet/address `intent.Manager.ensureNetwork`
      happens to auto-allocate there, almost certainly *not* the same addresses the
      source intent had. That's a real problem, not a cosmetic one: a migrated VM's
      disk skips cloud-init entirely on relaunch (see above), so there's no way to
      refresh its already-baked-in static network config or `/etc/hosts` entries to
      match a new subnet after the fact. Rather than trying to actively rewrite
      either of those inside every guest, the fix preserves the network exactly
      instead, so nothing needs rewriting at all:
      - `internal/migrate.Manager.migrateIntent` reads the source intent's own
        `store.IntentNetwork` (subnet/gateway/docker IP range) once, and each VM
        member's own recorded `store.IntentMember.IP`, and carries all of it in every
        member's migration payload (`payload.IntentNetwork`, `payload.StaticIP`),
        attached to every member regardless of kind, since whichever one's `anvil
        migrate-import`/Launch call reaches the target *first* is the one that
        actually creates the network there.
      - New plumbing to carry this from the payload through to where the network
        actually gets created: `LaunchRequest` gained four internal-only fields
        (`pinned_subnet`/`pinned_gateway`/`pinned_docker_ip_range`/`pinned_static_ip`,
        not meant to be set by a normal `anvil launch`), `instance.LaunchParams`
        gained matching `PinnedNetwork`/`PinnedStaticIP` fields, and
        `intent.Manager.ensureNetwork` now takes an optional pinned network that,
        when given, creates the network with that *exact* subnet instead of the
        usual `ipam.AllocateSubnet` hashed-attempt loop (no retry-with-a-different-
        subnet fallback for a pinned network either: if the exact subnet collides
        with something already on the target, that's a real migration failure to
        surface, not something to silently paper over with a subnet the guest's
        already-baked-in config knows nothing about). A VM member's static IP
        assignment got the same treatment: reuse `PinnedStaticIP` when set, instead
        of `ipam.VMAddress`'s normal next-in-sequence allocation.
      - Container members deliberately don't get a pinned address: unlike a VM, a
        container's networking is re-established fresh at every launch, nothing
        baked into an image to preserve, so it's simply left to the target's own
        engine to assign one as usual; only the subnet/gateway need to match, so a
        VM member migrating alongside it lands in the same network.
      - Net effect: since every VM member keeps the *exact* address it had on the
        source, whatever `/etc/hosts` entries were already correct there (baked in
        at each member's own original launch, per M4's existing "reflects membership
        as of launch time, not retroactively updated" design) stay correct on the
        target too, with nothing to actively rewrite inside any guest. This is
        deliberately not solved by having the source SSH into each guest and rewrite
        `/etc/hosts` after the fact; preserving the addresses makes that
        unnecessary, and avoids re-introducing exactly the kind of live guest
        mutation this whole design otherwise avoids.
      - **Known, accepted limitation, not solved here**: a member left behind on the
        source during a `--best-effort` partial migration still has the migrated
        peers' *old* addresses baked into its own `/etc/hosts`, now pointing at a
        bridge those peers are no longer attached to. Fixing that live would need
        actively rewriting a running guest's `/etc/hosts`, the exact kind of thing
        this design avoids elsewhere: flagged, not fixed.

### M8: TUI
- [x] **rewritten on Bubble Tea after a real run of the first pass (`rivo/tview`)
      showed broken-looking text boxes and confusing navigation.** tview's model is
      a tree of imperative widgets you wire up and mutate by hand; get the wiring
      slightly wrong (as the first pass evidently did, untested against a real
      terminal) and it just looks broken, with no framework-level structure forcing
      it back into a consistent shape. Bubble Tea's Elm architecture (one `Model`,
      a single `Update(msg) (Model, Cmd)` entry point, a single `View() string`) is
      a much smaller surface to get wrong, and it's what most of the terminal UIs
      people actually call "modern" today are built on (`gh`, `soft-serve`, and
      most of the rest of the Charm ecosystem's own showcase). Same package,
      `internal/tui`, same `anvil tui` command, same `pkg/client`-only dependency:
      this is a framework swap, not a scope change
- [x] app shell: `internal/tui/app.go`'s `model` holds one screen enum
      (menu/instances/launch/cloud-init/mirrors/migration) and one sub-model per
      screen; `Update`/`View` just dispatch to whichever is active. A hand-rolled
      main menu (arrow keys, enter, `q`) replaces the nav-list-plus-pages shell,
      simpler than a widget tree for five items, and the header/status line
      (OS username, socket, a one-shot daemon-reachability check) is common to
      every screen regardless
- [x] instances screen: a `bubbles/list.Model` (title/kind • state • image, one
      instance per row) with `n` launch, `s` start/stop, `d` delete (confirm
      overlay), `x` shell, `r` refresh, same verbs as before, same RPCs the CLI
      already calls
- [x] launch form: `internal/tui/form.go`'s `simpleForm`, a small shared component
      (Tab/Shift+Tab between fields, Space to toggle a checkbox-shaped field, Enter
      on the last field or Ctrl+S to submit, Esc to cancel) used by the launch
      form, mirror-add, host-add, and the cloud-init new/import/rename prompts,
      one implementation instead of five ad hoc ones. **Same deliberate v1
      simplification as before**: env/volumes/ports are each one comma-separated
      text field, not a dynamic add/remove row list
- [x] cloud-init screen: a `bubbles/list.Model` (configs) beside a
      `bubbles/textarea.Model` (editor), `n`/`m`/`r`/`d` for new/import/rename/
      delete (via `simpleForm` prompts), `e` to focus the editor, Ctrl+S to save,
      same `CloudInitService` CRUD RPCs
- [x] mirrors screen: a `bubbles/list.Model` of mirrors, `a` add (a `simpleForm`
      covering both VM-manifest and container-registry shapes, Ctrl+K swaps
      which), `e` enable/disable, `x` remove
- [x] migration screen: a known-hosts `bubbles/list.Model` beside a `simpleForm`
      (name, target, Copy/Best-effort/Dry-run toggles), Tab switches focus between
      them, submitting streams `MigrateService.Migrate`'s progress as plain lines
      below
- [x] shell/SSH handoff: `tea.ExecProcess`, Bubble Tea's own supported mechanism
      for exactly this (suspend the program, hand the real terminal to an external
      `*exec.Cmd`, resume automatically when it exits and report the result back
      as a message), simpler than tview's manual `Suspend`/resume pairing since
      the framework itself owns restoring the terminal afterward
- [x] every streaming RPC (`Launch`, `Migrate`) is consumed via the standard Bubble
      Tea pattern for a server-streaming call: a `tea.Cmd` receives one message off
      the stream and returns it; the message's own handler in `Update` re-issues
      "receive the next one" as its returned `tea.Cmd`, chaining through to the
      terminal event without ever blocking the UI loop
- [x] **a real bug caught while writing this, before it ever shipped**: the shared
      `simpleForm`'s Enter handling originally submitted the whole form immediately
      on *any* toggle field, not just the last one; harmless for the launch form
      (its toggles don't exist), but would have badly misfired the migration form,
      where Copy/Best-effort/Dry-run sit in the middle before Migrate. Fixed: Enter
      only submits on the actual last field, regardless of its kind; a toggle
      field just advances like any other
- [x] **a real UX bug caught while writing this**: `bubbles/list.Model` has
      filtering (`/`) enabled by default, which consumes every typed character
      (including the single-letter shortcuts above) as filter text, but each
      screen's own key handling intercepts `n`/`s`/`d`/etc. *before* forwarding
      anything to the list, so filtering would have silently never worked, eating
      keystrokes meant for the list instead. Fixed by disabling filtering on these
      (small, rarely-huge) lists entirely, rather than threading a filter-state
      check through every screen's key handling
- [x] **`internal/sshkey`, pulled out of `internal/cli/commands` for this
      milestone**: anvil's own default guest-access SSH keypair (generated once
      per invoking OS user, injected into every VM at launch so `shell`/`exec`/
      `transfer` work with zero flags) was only ever `internal/cli/commands`-
      internal before now. The launch form needs it too, so it's now its own small
      package both import, instead of the TUI growing a second copy to drift out
      of sync with the CLI's, same reasoning as `internal/hostpath`'s extraction
      earlier this session
- [x] **actually build tested this round, not just reviewed**: filippo built the
      real package on his own machine (`make tui-deps` resolved real
      `bubbletea`/`bubbles`/`lipgloss` versions this sandbox never could) and ran
      `anvil tui` for real, which is exactly what caught the bugs documented below.
      This sandbox still can't build (no network access to fetch the same three
      dependencies) but found a way to test anyway: a `pty`-backed Python harness
      (`os.openpty` plus feeding keystrokes to the already-installed `/usr/bin/anvil`
      binary from filippo's last build) let this session drive the real TUI headless
      and capture its actual rendered frames, which is how the layout bugs below got
      confirmed and root-caused precisely instead of guessed at. It could only
      exercise the binary as it existed *before* this round's fixes, though; the
      fixes themselves are reviewed and gofmt-clean but not re-verified the same
      way, since rebuilding still needs real network access this sandbox doesn't
      have. A real rebuild-and-test cycle on your end is still what actually confirms them.
- [x] **real bug, confirmed with the pty harness**: the Cloud-Init and Migration
      screens' side-by-side panels were built with plain string concatenation
      (`left + "  " + right`) instead of `lipgloss.JoinHorizontal`, which doesn't
      lay out multi-line blocks side by side at all, it just appends one block's
      text after the other's. Exactly the two screens reported broken were the only
      two using a side-by-side layout at all. Fixed everywhere this pattern was used.
- [x] **a second, related real bug**: even with `JoinHorizontal` fixed, the two
      panels ended up wildly different heights: `bubbles/list.Model` doesn't pad
      its own output to fill the height passed to `SetSize` the way `textarea.Model`
      does, so an empty or short list rendered as a much shorter box than the
      taller panel next to it. Fixed by pinning both panels to an explicit height via
      `lipgloss.NewStyle().Height(n).Render(...)` before adding the border, in both
      Cloud-Init and Migration.
- [x] **a third real bug, also pty-confirmed**: the launch form's `Name`/`Image`/
      `CPUs`/`Memory` fields were themselves in the captured frame, at the very top,
      just already scrolled off the terminal's visible area: eleven fields, each
      wrapped in its own bordered box (three lines minimum per field just for the
      border), produced more total lines than a normal terminal has rows, and the
      earliest-printed lines (the title and first fields) get pushed out as the
      terminal's own scrolling keeps up with everything printed below. Fixed with
      two changes to the shared `simpleForm`: compact rendering (no per-field box,
      a colored `┃` bar instead of a border to show focus, a hint line only under
      the focused field, not every field) and a `SetHeight`-driven scroll window
      that keeps whichever field is focused visible, with "↑ more above"/"↓ more
      below" indicators when the form doesn't fit. Wired in wherever a form is
      constructed (the launch form, mirror-add, host-add), sized from the model's
      own last-known terminal height.
- [x] **layout, requested explicitly**: rebuilt around a persistent left sidebar
      with the active page centered after it (Hyperpass's own shape), replacing
      the earlier full-screen menu you navigated away from and back to. The
      sidebar (`Instances`/`Images`/`Cloud-Init`/`Mirrors`/`Migration`) is always
      visible; `up`/`down` on it instantly switches (and reloads) whichever page is
      showing next to it, `enter`/`right` moves keyboard focus into that page,
      `esc` hands focus back to the sidebar rather than to a separate destination
      (there isn't one anymore). `anvil launch` (reached via Instances' `n`) is the
      one exception, a full-width takeover with no sidebar, matching how a modal
      form is usually presented rather than being its own nav destination.
- [x] **feature gap, flagged explicitly ("TUI and CLI miss the option to list the
      available cloud images")**: `ImageService` only ever covered the downloaded/
      cached tier (`internal/vm/image.Vault`); nothing exposed the *catalog* of
      distro images that can actually be downloaded and launched, not in the CLI,
      not in the TUI, even though `internal/vm.Backend` already had the merge logic
      (built-in catalog plus enabled mirrors) via its own (renamed, now exported)
      `EffectiveCatalog`. Fixed at every layer: a new `ImageService.Catalog` RPC
      (backed by `Backend.ListCatalog`, the exact same merge a real launch
      resolves against, not a separate copy), a new CLI command, `anvil find
      [term]` (was in the original plan's CLI surface, never actually built until
      now), and the TUI's new Images screen shows cached images and the catalog
      side by side.
- [x] Images screen (new, M8 was missing it from the original checklist too): cached
      images (`ImageService.List`/`Delete`) next to the catalog
      (`ImageService.Catalog`), `tab` to switch which panel has focus, `x` to
      delete a cached image (confirm overlay, cached only; a catalog entry isn't
      anything to delete), `r` to refresh both.

## Things we already know are unresolved

- Cross-kind networking (a VM and a container in the same intent sharing a bridge) is
  done and confirmed for Docker (M4); still entirely undesigned for Podman/netavark,
  since that backend doesn't exist yet.
- Whether an intent can mix Docker containers and Podman containers together is punted on
  for now, see the Intents section, current plan is "don't allow it" until there's a real
  reason to support it.
- The final default engine (once both exist) is meant to be Podman, matching the original
  spec, but that's not enforced by any code yet since only Docker exists as of M3's start.
- Now that `anvild` runs unprivileged (M6), Podman's own eventual access model is a real
  open question, not just "inherits root the way Docker socket access did"; see M6's
  notes on this.
- Migrating a container's arbitrary host bind mounts doesn't really work, only anvil
  managed volumes get copied. `--dry-run` is supposed to warn about this, not silently
  drop data.
- No bootstrap-over-SSH for migration: the target machine needs `anvil` already installed.
- The distro manifest schema has a `schema_version` field so we can change it later
  without silently breaking old manifests, but we haven't needed to bump it yet.
- `anvil mount` sharing an arbitrary host directory into a VM now depends on the "anvil"
  system user actually having access to that directory (M6); not solved, just flagged.
