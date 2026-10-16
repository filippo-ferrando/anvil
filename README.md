<div align="center">

# ⚒️ Anvil

**A Multipass-style VM & container manager that doesn't stop at VMs, doesn't stop at Ubuntu, and doesn't stop at one machine.**

[![CI](https://git.fferrando.cc/rdfilippo/anvil/actions/workflows/ci.yml/badge.svg)](https://git.fferrando.cc/rdfilippo/anvil/actions?workflow=ci.yml)
[![Release packages](https://git.fferrando.cc/rdfilippo/anvil/actions/workflows/release.yml/badge.svg)](https://git.fferrando.cc/rdfilippo/anvil/actions?workflow=release.yml)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE.md)
[![Go Version](https://img.shields.io/badge/go-1.27%2B-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS%20(in%20progress)-lightgrey)](#status--limitations)
[![Status](https://img.shields.io/badge/status-early--stage-orange)](#status--limitations)
[![TUI](https://img.shields.io/badge/TUI-Bubble%20Tea-ff69b4)](https://github.com/charmbracelet/bubbletea)
[![gRPC](https://img.shields.io/badge/api-gRPC-4285F4)](api/proto/anvil/v1/anvil.proto)

<img src="docs/demo/gifs/01-launch.gif" alt="anvil launching a VM and a container" width="820">

</div>

---

Anvil spins up cloud-init VMs and Docker containers from the same CLI, the same daemon,
and the same mental model. It's what I wish `multipass` was: not tied to Ubuntu, not VM-only,
and actually able to group stuff together and move it between machines.

No GUI of its own, no login system, no heavy abstraction layer to drag around. Just a
daemon, a CLI, a TUI, and the real primitives underneath (QEMU/QMP directly, no libvirt;
the real Docker API, no SDK; Apple's Virtualization.framework on macOS, in progress).

## Table of contents

- [Why](#why)
- [See it in action](#see-it-in-action)
- [How it compares](#how-it-compares)
- [Features](#features)
- [Install](#install)
- [Quick start](#quick-start)
- [The TUI](#the-tui)
- [How it's built](#how-its-built)
- [Documentation](#documentation)
- [Status & limitations](#status--limitations)
- [Contributing](#contributing)
- [License](#license)

## Why

[Multipass](https://multipass.run) is genuinely solid, and Anvil steals its whole vibe.
But a few things about it kept getting in the way:

- **VMs only.** No containers, so you end up running Multipass *and* Docker side by side
  for no good reason.
- **Ubuntu-first.** Everything about the image story assumes you want Ubuntu.
- **No grouping.** There's no way to say "these three things are actually one app" and
  manage them as a unit.
- **No mobility.** Moving a VM to another machine isn't really a supported idea.
- **A heavy GUI** that adds a lot of surface area when a terminal is where the work
  happens anyway.

Anvil is a from-scratch Go rewrite that fixes exactly those five things and nothing else.
Each platform gets a thin backend on its native hypervisor instead of a lowest-common-
denominator layer: QEMU/KVM driven over QMP on Linux, Apple's Virtualization.framework
on macOS (basic support in progress, see [Status](#status--limitations)).

## See it in action

| | |
| --- | --- |
| **Snapshots**, taken live, restored in seconds<br><img src="docs/demo/gifs/02-snapshot.gif" width="400"> | **Fork** a running VM, sharing its base image<br><img src="docs/demo/gifs/03-fork.gif" width="400"> |
| **Intents**: a VM and a container as one app, with DNS<br><img src="docs/demo/gifs/04-intent.gif" width="400"> | **Export / import** a whole intent as one file<br><img src="docs/demo/gifs/05-export.gif" width="400"> |

The TUI, end to end:

<img src="docs/demo/gifs/tui.gif" alt="anvil tui" width="820">

All recordings are [asciinema](https://asciinema.org) casts rendered to GIF, reproducible
with [`docs/demo/record.sh`](docs/demo/README.md).

## How it compares

| | Anvil | Multipass | Incus / LXD | Vagrant | Lima | Docker Compose |
| --- | :-: | :-: | :-: | :-: | :-: | :-: |
| Full VMs | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ |
| OCI (Docker) containers | ✅ | ❌ | ✅ ¹ | ➖ ² | ➖ ³ | ✅ |
| VMs + containers in one group, shared DNS | ✅ | ❌ | ➖ ⁴ | ❌ | ❌ | ❌ |
| Declarative multi-instance file | ✅ | ❌ | ➖ ⁵ | ✅ | ❌ | ✅ |
| Many distros out of the box | ✅ | ➖ | ✅ | ✅ | ✅ | n/a |
| Snapshots | ✅ | ✅ | ✅ | ✅ | ➖ | ❌ |
| Move to another host | ✅ cold, over SSH | ❌ | ✅ via an Incus remote, live or cold | ❌ | ❌ | ❌ |
| Export to a single file | ✅ | ❌ | ✅ | ✅ (box) | ❌ | ❌ |
| TUI | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Guest GUI apps on the host desktop | ✅ Wayland, app launcher export | ❌ | ➖ by hand | ➖ X11 over SSH | ❌ | ➖ by hand |
| Host OS | Linux, macOS (WIP) | Linux, macOS, Windows | Linux | Linux, macOS, Windows | macOS, Linux | Linux, macOS, Windows |
| Maturity | young, one maintainer | mature | mature | mature | mature | mature |

<sub>¹ Incus 6.3+ runs OCI images as app containers. ² Through the Docker provider, one
provider per machine. ³ Containers run inside the Lima VM (containerd/nerdctl), not
managed as instances. ⁴ Through a shared network and projects, wired by hand. ⁵ Through
profiles, preseeds or the Terraform provider. Based on upstream docs as of late 2026;
corrections welcome.</sub>

Short version: Incus is the closest and far more mature; pick it for clusters and live
migration. Anvil fits when there is already a Docker host and a few Linux boxes reachable
over SSH, and VMs and containers need to live side by side without setting up a cluster.
The longer write-up is in [docs/comparison.md](docs/comparison.md).

## Features

**VMs**, driven straight against `qemu-system-*` over QMP (no libvirt in between):

- 15 base images out of the box: Ubuntu, Debian, Arch, Fedora, Rocky, AlmaLinux, CentOS
  Stream, openSUSE, Alpine (or bring your own via a mirror)
- cross-arch guests: an `aarch64` image runs under emulation on an x86_64 host, see
  [docs/aarch64-on-x86.md](docs/aarch64-on-x86.md)
- real cloud-init under the hood, plus a saved cloud-init config library you can write,
  edit, and reuse (`anvil cloud-init`)
- `anvil shell` / `anvil exec` / `anvil transfer` over real SSH, using a keypair Anvil
  manages for you, *zero* flags, *zero* setup
- `anvil mount` shares a host directory into the guest over virtiofs, live when the
  guest agent is connected
- live progress on downloads instead of a silent terminal
- `anvil snapshot` takes point-in-time QCOW2 checkpoints of a VM's disk and restores back
  to them later, live if the VM is running, and `anvil snapshot schedule` takes them
  every interval, keeping the newest N
- `anvil fork` clones a VM into a new instance by copying its disk, keeping the same
  backing image so the fork only stores its own deltas. Safe to run on a running source
  VM: it stays up, and the copy is point-in-time (filesystem-consistent with the guest agent)
- `anvil find [term]` lists the VM images that `anvil launch` can resolve, built-in and
  mirrored alike
- `anvil set` grows the disk and changes CPUs/memory, all live on a running VM; `--autostart` and
  `--restart no|on-failure[:N]|always` keep long-running VMs and containers up

**Containers**, talking to Docker's real HTTP API directly (Podman backend is on the
way, tracked separately, see [limitations](#status--limitations)):

- `anvil launch --kind container nginx:alpine` -> auto-pulls, just like `docker run`
- the same `shell`/`exec`/`transfer` commands work here too, backed by `docker
  exec`/`docker cp`
- volumes, env vars, published ports, custom entrypoints, the works
- `anvil image containers list` (and the TUI's Images screen) shows every image cached by
  the engine, separate from VM images, and whether it's actually in use by a container

**Intents**: group VMs and containers together and manage them as one thing:

```
anvil intent create myapp --vm db:postgres:16 --container cache:redis:7
```

Every member gets its own bridge network, a real IP, and can resolve every other member
by name -> no manual networking required. Names are served by `anvild` itself, so a
member added later with `anvil intent add` is resolvable right away by the ones already
running, as `db`, `db.myapp.anvil` or `myapp-db`. See [docs/intent-dns.md](docs/intent-dns.md).

An intent can also be described in an `anvil.yaml` file and kept in line with it by
`anvil apply -f anvil.yaml`, with `depends_on` start order. See [docs/apply.md](docs/apply.md).

**Migration**: move an instance, or a whole intent, to a different host over plain SSH:

```
anvil migrate myapp --to user@otherhost
```

No daemon-to-daemon trust to set up. If you can already SSH there, you can migrate there.
Copies the disk, brings the network config with it, and only deletes the source once the
target actually confirms it worked.

Before anything is stopped or copied, the target is checked for the things that would
make the migration fail or the instance unusable: CPU architecture and the matching
`qemu-system-*`, KVM, the container engine, free space, RAM, and a name collision.
Problems that can't be worked around stop the migration there, the rest are printed as
warnings. `anvil migrate <name> --to <host> --dry-run` runs only those checks.

Only a VM's own changes travel, not its base image, and the base image itself is sent
once when the target has no copy of it. Migrating a second VM of the same distro to that
host then sends only its changes again.

Bind-mounted data travels too: a VM's shared folders and a container's bind-mounted
directories are copied to the target, where they land under `/var/lib/anvil` rather than
at their original host paths, and the mounts are recreated over them. A host path that
isn't a readable directory is reported: a container still mounts that same path on the
target, a VM drops the share rather than fail to start. `anvil export` packs the same
data into its bundle.

`anvil host discover` lists the other anvil hosts on the same network: anvild announces
itself over mDNS as `_anvil._tcp`, so `avahi-browse` sees it too. It only reports a name,
an address and an SSH port, nothing is saved and no access is granted, so the output is
just what `anvil host add` would otherwise have to be told. `ANVIL_MDNS=off` in anvild's
environment turns the announcement off.

`anvil --remote <[user@]host>` (or `ANVIL_REMOTE`) drives the anvild on another host over
plain SSH, for every command and for `anvil tui`. It runs `anvil dial-stdio` on that host,
so it needs key or agent SSH auth, the `anvil` CLI there, and membership of its `anvil`
group. `shell`, `exec` and `transfer` reach a VM through that host with `ssh -J`, and
containers through `DOCKER_HOST=ssh://`, so the local `ssh` and `docker` CLIs are enough.
Paths sent to the daemon (`mount`, `export`, `import`) are paths on the remote host and
must be absolute.

`ANVIL_METRICS=127.0.0.1:9469` (or `unix:/run/anvil/metrics.sock`) in anvild's
environment serves Prometheus metrics at `/metrics`: each instance's state plus the live
CPU, memory, disk, network and uptime numbers that `anvil stats` shows. It's off unless set.

Migration SSH trusts a host's key the first time it sees it and records it. To require a
key anvil already knows instead, add the host with `anvil host add <alias> <user@host>
--strict-host-key`; adding the same alias again replaces it, so this can be turned on
once the key is on record.

**GUI apps**: run a graphical app from a VM or container as a native window on the
host's Wayland desktop:

```
anvil gui dev -- gnome-text-editor
anvil gui --x11 re-lab -- ghidraRun
anvil gui export --x11 re-lab -- ghidraRun   # add it to the desktop's app launcher
```

A container talks to the host compositor through a relay (GPU included when the host has
`/dev/dri`), a VM goes through `waypipe` over SSH. X11 apps work through XWayland with
`--x11`. A stopped instance is started first. Only instances created by a version with
GUI support can do this. See [docs/gui.md](docs/gui.md).

`anvil list` and `anvil find` take `-o json` for scripts: the reply is printed as JSON
with the field names from `anvil.proto`, instead of the table.

**A TUI**, because staring at JSON all day gets old:

```
anvil tui
```

Built on [Bubble Tea](https://github.com/charmbracelet/bubbletea). Same daemon, same API,
just easier on the eyes.

**A growing image/template catalogue** you can pull from without waiting for a new
release:

```
anvil mirror add --kind vm --manifest-url <url> extra-images
anvil cloud-init import-repo <url>
```

The official catalogue (extra distro manifests for x86_64 and aarch64, plus cloud-init
templates) is its own repo, checked out here as the `catalog/` git submodule. Its
README has the exact URLs to pass to the two commands above. `git submodule update
--init` fetches it after a clone. See [docs/mirrors.md](docs/mirrors.md) for how
mirrors and template repos work.

## Install

Prebuilt Arch, deb and rpm packages are attached to the
[latest release](https://git.fferrando.cc/rdfilippo/anvil/releases/latest), with the
install commands in its notes. To build the packages instead:

### Arch Linux

Clone the repo, then from its root:

```
makepkg -si -p packaging/archlinux/PKGBUILD
```

This builds two packages, `anvil` (the CLI/TUI) and `anvild` (the daemon), and installs
the daemon as a proper systemd service running under its own unprivileged `anvil` user:
**not root**. The post-install step (`packaging/common/post-install.sh`, shared by all three package
formats) sets up the user, generates a migration keypair, and adds `anvil` to the
`docker` group if it's already installed.

### Debian/Ubuntu and Fedora/RHEL

The same two packages build from `packaging/deb/build.sh` and `packaging/rpm/build.sh`.
Both ship the completions, the man pages, and the same post-install step as the Arch
package.

### From source

You'll need Go 1.27+, `protoc`, and `qemu-system-x86_64`,
`qemu-img`, and `xorriso` on your `PATH`, plus `virtiofsd` (for `anvil mount`; it's
usually installed outside `PATH`, e.g. `/usr/lib/virtiofsd`).

```
make proto          # regenerate api/gen/anvil/v1 from the .proto file
make build          # builds ./anvil and ./anvild
make man            # optional: man pages into man/man1 and man/man8
make gui-tools      # optional: static waypipe/xwayland-satellite for `anvil gui` with VMs (needs docker)
```

The packages install the GUI tools in `/usr/lib/anvil/gui`. For a source build, copy
`build/gui-tools/*` there or point `ANVIL_GUI_TOOLS` at that directory.

## Quick start

```
sudo ./anvild &

./anvil launch --kind vm ubuntu-24.04 --name box
./anvil shell box
./anvil list
./anvil stop box
./anvil delete box --purge
```

(Running as root here is just the fast path for trying it out locally, the packaged
version runs the daemon as an unprivileged system user instead, see [Install](#install).)

## The TUI

`anvil tui` gives you a sidebar-driven view over Instances, Snapshots, Images, Intents,
Cloud-Init, Mirrors, and Migration, over the exact same gRPC API the CLI uses
(`anvil stats <name>` is the CLI's own window into the same data). Shell/exec sessions
hand the real terminal off to `ssh`/`docker exec` and back, logs stream live, and launch progress redraws in place
instead of scrolling your terminal into oblivion.

Select an instance on the Instances screen and its detail panel shows a live CPU/memory
gauge, disk usage (or I/O rate for a container), network throughput, uptime, and address,
sampled straight from the process itself (`/proc` + the tap device for a VM, Docker's own
stats endpoint for a container), refreshed every couple of seconds, no guest agent
required. From the same screen, `f` forks the selected VM, `E`/`i` export/import it, and
`g` runs a GUI app from it (optionally adding it to the desktop's app launcher).

The Migration screen lists the known hosts beside a migrate form: `a` adds a host, `d`
scans the local network over mDNS (`enter` on a result opens the add form prefilled),
`t` tests one, `x` removes one, and `tab` moves over to the migrate form, which
suggests instance and intent names for Name and known host aliases for To.

## How it's built

```
anvil (CLI + TUI, one binary)  ──gRPC over a unix socket──▶  anvild (daemon)
                                                                  │
                                                                  ├─ QEMU processes, driven over QMP
                                                                  ├─ Docker's real HTTP API
                                                                  ├─ bbolt (instances / intents / mirrors / hosts)
                                                                  └─ SSH, for migration to another anvild
```

The daemon owns every bit of business logic. The CLI and TUI are dumb clients: build a
request, call the daemon, print the reply. Access control is just being in the `anvil`
Unix group and being able to reach the socket, *no* accounts, *no* portal, *no* password.

Everything the daemon does across hosts (migration) goes over plain SSH to the target's
own local `anvil` CLI, never daemon-to-daemon gRPC. That means Anvil never has to solve
cross-host trust on its own, whatever SSH access you already have is enough. The same
goes for `anvil --remote`: the CLI runs `anvil dial-stdio` on the other host over SSH and
speaks gRPC through that pipe to the local socket there.

```
cmd/anvild/      daemon entrypoint
cmd/anvil/       CLI entrypoint
pkg/client/      thin gRPC client (unix socket or SSH), shared by the CLI and TUI
internal/
  daemon/        gRPC service implementation
  instance/      domain model + the Manager
  vm/            QEMU backend: process management, QMP, cloud-init, image catalog
  vm/vz/         Apple Virtualization.framework backend (stub, not implemented)
  container/     Docker backend (Podman lands here too, eventually)
  intent/        grouping, shared bridge networking per intent
  intent/dns/    per-intent DNS server ("<role>.<intent>.anvil")
  intent/apply/  the compare-and-apply engine behind `anvil apply`
  migrate/       SSH-driven migration
  discovery/     mDNS announce and browse behind `anvil host discover`
  export/        bundles behind `anvil export` and `anvil import`
  gui/           `anvil gui`: socket relay for containers, app name/icon lookup
  config/        fixed on-disk paths (run, state, cache, GUI tools)
  store/         bbolt-backed registry
  cli/commands/  cobra commands
  tui/           the Bubble Tea app
api/proto/       the gRPC API definition
data/distros/    the built-in VM image catalog
catalog/         the official extra catalogue (git submodule)
packaging/       PKGBUILD, deb/rpm build scripts, systemd unit, sysusers/tmpfiles rules
scripts/         catalog checksum check, release notes and changelog from commit subjects, static GUI tools build
```

## Documentation

| Topic | Doc |
| --- | --- |
| Design, components, trade-offs | [docs/architecture.md](docs/architecture.md) |
| Instance states, restart policies, guest agent | [docs/lifecycle.md](docs/lifecycle.md) |
| Networking, port forwards, intent bridges | [docs/networking.md](docs/networking.md) |
| Intent DNS | [docs/intent-dns.md](docs/intent-dns.md) |
| Declarative intents (`anvil apply`) | [docs/apply.md](docs/apply.md) |
| Snapshots and fork | [docs/snapshots-and-fork.md](docs/snapshots-and-fork.md) |
| Export and import bundles | [docs/export-import.md](docs/export-import.md) |
| Migration between hosts | [docs/migration.md](docs/migration.md) |
| Mirrors and cloud-init repos | [docs/mirrors.md](docs/mirrors.md) |
| aarch64 guests on x86_64 | [docs/aarch64-on-x86.md](docs/aarch64-on-x86.md) |
| The TUI, screen by screen | [docs/tui.md](docs/tui.md) |
| GUI apps from instances (`anvil gui`) | [docs/gui.md](docs/gui.md) |
| Security model | [docs/security.md](docs/security.md) |
| Comparison with similar tools | [docs/comparison.md](docs/comparison.md) |

## Status & limitations

Anvil is young but everything above is real and working, not vaporware: VMs,
containers, intents, migration, the TUI, and Arch/deb/rpm packaging all run today. A few things
aren't there yet:

- **macOS** support (Apple Virtualization.framework, `internal/vm/vz`) is being built.
  The daemon compiles for darwin, but the VM backend is still a stub; Linux is the only
  host that runs VMs today.
- **`anvil gui`** needs a Wayland host session, and with `--remote` it only works for VMs.
- **Podman** isn't wired up. `--engine podman` fails cleanly instead of pretending to
  work; Docker is the only engine right now.
- The built-in image catalog's checksums are a work in progress: run
  `scripts/verify-catalog.sh` if you want to double-check before you trust it blindly, or
  point `anvil mirror add` at your own verified manifest instead.

The CLI, the gRPC API and the on-disk formats can still change between releases until
the planned feature set is settled. What's planned is in [PLAN.md](PLAN.md), what changed
in each release is in [CHANGELOG.md](CHANGELOG.md).

## Contributing

Issues and PRs are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md). If you're touching
daemon logic, keep it in `internal/` behind the gRPC boundary, the CLI and TUI are meant
to stay dumb clients, that's what keeps them in sync with each other for free. Security
issues: [SECURITY.md](SECURITY.md). Everyone taking part follows the
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

[Apache 2.0](LICENSE.md).
