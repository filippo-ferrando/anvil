# Anvil

Anvil is Multipass, but for people who also want containers, want to group things
together, and don't want to be locked into Ubuntu or a GUI.

It's a daemon (`anvild`) plus a CLI (`anvil`), written in Go. Eventually there's also
going to be a TUI. `anvild` runs your cloud-init VMs by driving QEMU directly (no
libvirt) and your containers through Docker or Podman, both. Everything else, the CLI,
the (future) TUI, talks to the daemon over gRPC on a unix socket. No business logic
lives in the clients.

Linux only. No macOS, no Windows, no login system, no GUI. If you want those, use
Multipass.

## Why this exists

Multipass is a genuinely good tool, but it has some limits that got annoying enough to
build something else:

- it only does VMs, no containers
- the image story is very Ubuntu-centric
- there's no built-in way to say "these five VMs are actually one thing" (an app made of
  a web server, a database and a cache, say)
- moving a VM to a different machine isn't really a feature
- it ships a full GUI and cross-platform support we just don't need

We took some structural ideas (how the API is shaped, how the image catalog works, how
migration avoids cross-host trust issues) from a project called Hyperpass, which is a C++
fork of Multipass with a bunch of LLM and GUI stuff bolted on that we specifically did
NOT want. None of that code is reused here. Anvil is a clean Go rewrite that borrows some
design decisions, not a fork.

## What it can do (or will be able to)

- launch cloud-init VMs from a bunch of distros, not just Ubuntu (Ubuntu, Debian, Arch,
  Fedora, Rocky, AlmaLinux, CentOS Stream, openSUSE, Alpine)
- launch containers from any OCI image, via Docker or Podman (Docker's landing first,
  Podman right after)
- upload your own custom cloud-init config instead of using the built-in ones
- group VMs and containers into "intents", so `myapp` can mean "one VM plus two
  containers" and you manage it as a unit
- add your own image mirrors at runtime, for both VM images and container registries
- cold migrate an instance (or a whole intent) to a different machine, copying it or
  moving it
- package cleanly for Arch Linux, with a proper PKGBUILD and systemd unit

## Current status

This is early. Here's what's actually real right now, versus what's just designed:

**Working (all of M1 and M2), most of it confirmed on real hardware, not just built:**
- `anvil launch --kind vm`, `list`, `info`, `start`, `stop`, `delete`, `purge`
- real QEMU processes get spawned, with real cloud-init seeds and a proper
  prepared-image/overlay-disk setup
- everything talks over an actual gRPC API, generated from `api/proto/anvil/v1/anvil.proto`
- a saved cloud-init config library (`anvil cloud-init list/new/edit/show/rename/delete`),
  reference one at launch with `--cloud-init-name`
- runtime VM image mirrors (`anvil mirror add/list/remove/enable/disable --kind vm`), on
  top of the built-in catalog
- reconciliation on daemon restart, a running VM no longer gets reported as stopped just
  because `anvild` restarted
- `anvil launch --ssh-key ...` + `anvil shell <name>` / `anvil exec <name> -- <cmd>` /
  `anvil transfer` all work: a real launch followed by a real interactive SSH login shell
  into a real Ubuntu 24.04 guest, actually tested, not just wired up. Getting here surfaced
  two real bugs (invalid YAML being generated for `ssh_authorized_keys`, and `sudo`
  breaking SSH identity resolution, see `PLAN.md`'s M2 notes for both), now fixed
- `anvil logs <name> [-f]` reads the guest's boot/cloud-init console output, which is also
  literally how the YAML bug above got diagnosed
- `anvil launch` shows real progress while it runs instead of going quiet: download
  percentage for a VM base image, per-layer pull status for a container image, both
  throttled so it's not a wall of spam, but never silent for more than half a second, so
  a slow launch doesn't look like a stuck one. On a real terminal these updates redraw
  in place (carriage return) instead of scrolling the screen with hundreds of near-
  identical lines, found and fixed on filippo's own first real image download
- anvil manages its own SSH keypair (generated once, no passphrase, same idea as Vagrant's
  shared key or Multipass's own managed key) and injects it into every VM by default, so
  `shell`/`exec`/`transfer` work with zero flags, no dependence on whatever personal key you
  happen to have
- `anvil image list` / `anvil image delete <id>` manage the cached base-image tier, refusing
  to delete an image a running instance's disk still depends on unless you `--force` it
- `anvil mount <host-path> <name>:<guest-path>` / `anvil umount <name>:<guest-path>` share a
  host directory into a VM over 9p. Heads up: this restarts the guest OS to attach or
  detach the share (there's no way to hot-plug a 9p share into a live QEMU instance, checked
  against a real QEMU build rather than assumed), it's not a transparent hot-plug. The
  QEMU/cloud-init side of this is tested for real; an actual guest mounting and using the
  share hasn't been, that needs a real downloaded image, which this dev sandbox can't do

**Working (M3, Docker half), confirmed with a real launch on real hardware:**
- `anvil launch --kind container <image>` works end to end against Docker, with
  `--engine`, `--env`/`-e`, `--volume`/`-v`, `--publish`/`-p`, `--entrypoint`, and a
  trailing `-- cmd args` to override the image's own command. It now pulls a missing
  image automatically before creating the container, matching `docker run`'s own
  behavior, that wasn't true on the first real launch, see below
- `anvil shell`/`anvil exec`/`anvil transfer` all work against container instances too
  now, via `docker exec`/`docker cp` shelled out to the real binary, not just VMs over SSH
- `anvil list`/`anvil info` show engine, container ID, published ports, and volumes for
  container instances
- talks to Docker's real HTTP API over `/var/run/docker.sock` through a small hand-rolled
  client, not the official Docker SDK (same call as QMP for VMs: didn't want a dependency
  whose exact current API we couldn't verify without network access here)
- first real bug, found on a real Docker daemon: `anvil launch --kind container
  nginx:alpine` 404ed with "No such image", because `POST /containers/create` doesn't
  auto-pull a missing image the way the `docker` CLI does, it just 404s. Fixed by
  checking for the image first and pulling it if missing, see `PLAN.md`'s M3 notes.
  Everything else is tested for real against a mock unix-socket HTTP server (all
  passing), since this sandbox itself has no root/rootless Docker tooling to run
  `dockerd`
- `anvil mirror add --kind container --registry <host> --mirror-of <upstream>` is now
  actually applied for Docker, not just stored: a matching image ref gets rewritten to
  pull through the mirror before create. This is a client-side rewrite, not a dockerd
  `daemon.json` edit, since Docker's own mirror setting only covers Docker Hub, see
  `PLAN.md` for why and for the `--insecure` caveat (stored, but Docker itself still
  needs its own `insecure-registries` config for that to actually work, unlike Podman)
- Podman is deferred, not built: filippo doesn't have it on this machine to test
  against. `--engine podman` gives a clear "not implemented yet" error rather than
  silently doing nothing

**Working (M4), the least-verified thing in this repo so far:**
- `anvil launch --kind vm|container ... --intent NAME --role ROLE` joins that instance to
  an intent, creating it automatically on first use, no separate "create the group first"
  step required
- `anvil intent create <name> --vm role:image --container role:image` and
  `anvil intent add <name> --vm role:image` are shorthand for the above (just a few
  streaming `Launch` calls in a row); `anvil intent list/info/remove/delete` manage the
  group itself, `delete --purge-members` also tears down every member instance
- `anvil info` shows an instance's intent and role when it's a member of one
- `anvil delete <name>` on a member instance directly (not through any `intent` command)
  now correctly drops it from its intent's member list too, instead of leaving a stale
  entry behind pointing at a deleted instance ID (caught by filippo before it shipped)
- **the shared network is now actually built**: each intent gets its own Docker bridge
  network, created on first use. Container members join it the normal Docker way; VM
  members get a tap device attached to its bridge (new `github.com/vishvananda/netlink`
  dependency) plus a static IP and a generated cloud-init network-config instead of
  SLIRP. `anvil shell`/`exec`/`transfer` connect to a bridged VM's real address directly.
  Podman is deferred, so this is Docker-only for now.
- `anvil intent delete` now removes the intent's Docker network too, not just the group
  record, best-effort: a member still attached to it (not purged, or a VM tap still on
  the bridge) will likely make Docker refuse, which is logged and not fatal to the delete
- **why this is genuinely the shakiest thing here**: the netlink dependency couldn't be
  fetched or exercised against a real bridge in this sandbox at all (no network to
  `go get` it, no root, no bridge to test against), and none of Docker's
  bridge-name option, the reserved-subnet split, real VM-to-container connectivity, or
  cloud-init actually applying a static IP has touched a real machine. See `PLAN.md`'s
  Intents section for the full breakdown of what's unverified before you trust this.

**Not built yet:**
- Podman (the second container backend, deliberately deferred, see above; its own
  `registries.conf.d`-based mirror mechanism, and its own network story, are tied to it
  landing too)
- migration
- the TUI
- Arch packaging

None of the M2 cloud-init/mirror work, the M3 container work, or the M4 intent/network
work above has actually been build tested yet either. The `.proto` file has grown again
since the last confirmed build (`CloudInitService`/`MirrorService` for M2,
`ContainerSpec.engine`/`container_id` for M3, `LaunchRequest.intent_name`/`role` actually
wired up, a new `IntentService`, and `VMSpec.bridge_interface`/`static_ip`/`gateway` for
M4), so you'll need to run `make proto` again before `go build` picks any of it up. `go
mod tidy` also needs to fetch the new netlink dependency for real this time.

See `PLAN.md` for the full design and a milestone-by-milestone roadmap.

## Building it

You need Go, `protoc`, and network access (to fetch Go modules and the protoc plugins).

```
make proto        # regenerates api/gen/anvil/v1 from the .proto file
go mod tidy        # pulls in bbolt, cobra, grpc, etc
go build ./...
go vet ./...
go test ./...
```

If `make proto` complains that `protoc-gen-go` isn't found, it's probably just not on
your `PATH`. `go install` puts binaries under `$(go env GOPATH)/bin`, make sure that's on
your `PATH` before running `make proto` again.

To actually run a VM you'll also need `qemu-system-x86_64`, `qemu-img`, and `xorriso`
installed. Later on we're planning to drop the `xorriso` dependency in favor of a pure Go
ISO9660 writer, but for now it's a real runtime dependency.

## Running it

```
sudo mkdir -p /run/anvil /var/lib/anvil /var/cache/anvil
sudo ./anvild &
./anvil launch --kind vm ubuntu-24.04 --name test1
./anvil list
./anvil stop test1
./anvil delete test1 --purge
```

Right now `anvild` needs to run as root (it needs `/dev/kvm` and eventually raw network
access for bridges). Once packaging lands, this'll be handled by a dedicated `anvil`
system user and a systemd unit instead of you running it by hand.

## A couple of honest warnings

- the built-in image catalog (`data/distros/distribution-info.json`) has real-looking
  URLs for most distros but the checksums are all blank and a few entries (Fedora,
  openSUSE, Alpine) currently point at directory listings instead of an actual file.
  Don't trust it blindly, check it before you rely on it. Run
  `scripts/verify-catalog.sh` somewhere with real internet access to check every URL and
  get the real sha256 for each one.
- restarting `anvild` while a VM is running used to lose track of it completely. That's
  fixed now (see `PLAN.md`'s "VM backend" section for how), tested against real spawned
  processes, not just mocked.
- there's now a real integration test (`internal/vm/qemu/spawn_integration_test.go`) that
  spawns an actual `qemu-system-x86_64`, talks real QMP to it, and stops it, no mocking.
  What it does NOT prove: a real cloud-init VM booting. There's no OS on the disk it uses,
  just an empty qcow2, since that needs a real downloaded base image, which needs real
  network and a working CA cert store, neither of which this dev sandbox has (no root, no
  `/dev/kvm`, no `/var/lib` either, so a full `anvild` run isn't happening here regardless).
  A real end-to-end boot with cloud-init actually running still needs to happen on an
  actual machine.

## Project layout

```
cmd/anvild/      daemon entrypoint
cmd/anvil/       CLI entrypoint
pkg/client/      thin gRPC client, shared by the CLI and (later) the TUI
internal/
  daemon/        gRPC service implementation, converts wire types to domain types
  instance/      domain model, the Manager, and the Backend/Registry interfaces
  vm/            QEMU backend (process management, QMP, cloud-init, image catalog)
  container/     Docker backend (done) and Podman backend (deferred)
  intent/        intent membership, intent-aware launch, shared Docker bridge network per intent
  migrate/       SSH-driven migration, not built yet
  store/         bbolt-backed registry
  cli/commands/  cobra commands
  tui/           tview app, not built yet
api/proto/       the gRPC API definition
data/distros/    the built-in VM image catalog
scripts/         one-off maintenance scripts (verify-catalog.sh)
```

## License

Apache-2.0.
