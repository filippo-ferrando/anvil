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

**Not built yet:**
- containers (`--kind container` gives you a clear "not implemented" error for now, and
  `--kind container` mirrors are stored but not applied anywhere yet either)
- intents
- migration
- the TUI
- Arch packaging

None of the M2 cloud-init/mirror work above has actually been build tested yet either,
the `.proto` file grew two new services (`CloudInitService`, `MirrorService`) so you'll
need to run `make proto` again before `go build` picks it up.

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
  container/     Docker and Podman backends, not built yet (Docker first)
  intent/        intent groups, not built yet
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
