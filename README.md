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
- `--publish host:guest[/tcp|udp]` on `anvil launch` now works for a standalone VM too,
  not just containers: same flag, another SLIRP hostfwd entry, so something like nginx
  running directly inside the guest is reachable from the host. Doesn't apply to a
  bridged intent member (it already has its own reachable address), and `anvil launch`
  rejects the combination outright rather than silently dropping the ports

**Working (M3, Docker half), fully confirmed on filippo's own real Docker daemon:**
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
  checking for the image first and pulling it if missing, see `PLAN.md`'s M3 notes
- `anvil mirror add --kind container --registry <host> --mirror-of <upstream>` is now
  actually applied for Docker, not just stored: a matching image ref gets rewritten to
  pull through the mirror before create. This is a client-side rewrite, not a dockerd
  `daemon.json` edit, since Docker's own mirror setting only covers Docker Hub, see
  `PLAN.md` for why and for the `--insecure` caveat (stored, but Docker itself still
  needs its own `insecure-registries` config for that to actually work, unlike Podman)
- Podman is deferred, not built: filippo doesn't have it on this machine to test
  against. `--engine podman` gives a clear "not implemented yet" error rather than
  silently doing nothing

**Working (M4), fully confirmed on filippo's own machine, netlink dependency included:**
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
- **members can resolve each other by name now, not just IP**: containers already got
  this for free from Docker's own embedded DNS, but only under anvil's internal
  `anvil-<name>` container name; they now also get a network alias equal to their plain
  role, so `web` works, not just `anvil-web`. VMs get `/etc/hosts` entries for every peer
  (any kind) via cloud-init, since they have no DNS mechanism of their own; containers
  get `--add-host`-equivalent entries specifically for VM peers (container peers are
  already covered by the alias). Same staleness caveat as the network itself: reflects
  membership as of each member's own launch time, a later-added member isn't
  retroactively added to earlier members' hosts without a restart

**Working (M5), zero real two-machine testing yet, the least-verified thing here:**
- `anvil host add/list/remove/test`: a saved `user@host[:port]` shortcut for
  `anvil migrate --to`, adding one grants no trust by itself. `test` actually SSHes in
  and checks anvil/anvild are on PATH, not just that the alias exists
- `anvil migrate <name> --to <alias|user@host[:port]> [--copy] [--dest-name X]
  [--dry-run]`: moves a single VM or container to a different anvil host over SSH, no
  daemon-to-daemon trust needed, same idea as `anvil shell` shelling out to the real
  `ssh`/`scp` binaries instead of a Go SSH library (no new dependency either). Default
  mode is destructive but implemented as copy-verify-then-delete, never delete-first;
  `--copy` skips the delete
- a migrated VM's disk gets flattened (`qemu-img convert`) and shipped over, then
  relaunched on the target skipping cloud-init entirely, since the disk already has
  everything from its original first boot; a migrated container just gets recreated
  from its spec on the target, which re-pulls the image itself (only works for a
  registry-hosted image, not a local-only build, an accepted limitation)
- mDNS auto-discovery of peer hosts (from the original plan) is deliberately deferred:
  known hosts alone are enough for migration to work, and discovery grants no trust by
  itself either way. See `PLAN.md`'s Migration section for the full reasoning
- **why this needs real testing more than anything else in this repo**: this was written
  and reviewed with no second machine to actually SSH to. No real `scp` transfer, no real
  flattened-disk boot, no real run of the SSH stdout-parsing protocol `anvil migrate` and
  `anvil migrate-import` depend on. Reviewed carefully, executed exactly zero times

**Working (M6, packaging), moved up from dead last in the roadmap specifically so this
could exist before intent migration or the TUI, to make testing on other machines
easier:**
- `packaging/PKGBUILD`: a split package, `anvil` (CLI) and `anvild` (daemon). Builds
  straight from the working tree (no release tarball exists yet). Recommended build is
  a clean chroot, `extra-x86_64-build` (from `devtools`), not bare `makepkg`, so
  dependencies actually get verified instead of assumed
- **`anvild` runs as a dedicated unprivileged "anvil" system user, not root**, per
  filippo's explicit ask. Three separate mechanisms get it everything it needs without
  being root: `SupplementaryGroups=kvm` for `/dev/kvm` (safe since `qemu-base` is a hard
  dependency), `AmbientCapabilities=CAP_NET_ADMIN` for intent tap/bridge management, and
  Docker group membership granted by the install script conditionally (not a static
  group in the unit, since Docker might not be installed yet, which would otherwise fail
  the whole service to start)
- `anvild.install` (a real pacman post_install/post_upgrade hook) creates the "anvil"
  user immediately, generates a **passwordless SSH key for `anvil migrate`** at
  `/var/lib/anvil/.ssh/id_ed25519` (printed on screen so you can copy the public half to
  a target host's `authorized_keys`), and adds "anvil" to the "docker" group if Docker
  happens to already be installed
- **a real, load-bearing consequence, not just a permissions checkbox**: `anvil mount`
  sharing an arbitrary host directory into a VM now depends on the "anvil" user actually
  having access to that directory (QEMU's 9p backend does that file I/O as whatever user
  spawned it). Under the old root-anvild design this always worked regardless of
  permissions; now it doesn't. Not solved here, just made real instead of staying an
  aspiration nobody had checked yet
- shell completions (bash/zsh/fish) generated at package time from cobra's own built-in
  `completion` subcommand, no extra code needed
- **not included**: man pages (`cobra/doc`'s generator needs
  `github.com/cpuguy83/go-md2man/v2`, a new dependency that couldn't be fetched in this
  sandbox, same limitation as every other new dependency this project has needed) and a
  real `makepkg`/`namcap`/`extra-x86_64-build` run (this sandbox has the `makepkg`
  binary but no actual Arch build environment, `/etc/makepkg.conf`/`/etc/pacman.conf`
  don't exist here, and no `devtools` for a chroot build either). `bash -n PKGBUILD` and
  `sh -n anvild.install` both pass and everything was reviewed carefully, but an actual
  chroot build needs to happen on your end

**Not built yet:**
- Podman (the second container backend, deliberately deferred, see above; its own
  `registries.conf.d`-based mirror mechanism, and its own network story, are tied to it
  landing too)
- migrating a whole intent as one unit (M7, moved down from M6 so packaging could move
  up); single-instance migration is done (see above)
- the TUI (M8, moved down from M7 for the same reason)

M2, M3, and M4 (including the name-resolution and `--publish`-for-VMs work) have all now
been build tested and confirmed working by filippo on his own real hardware, not just
reviewed in a sandbox. M5 (`anvil host`/`anvil migrate`) and M6 (packaging) above are
brand new and haven't been build tested at all yet: `make proto` again (`HostService`,
`MigrateService`, `VMSpec.source_disk_path`), then the usual `go build`, or, once M6
is confirmed working, just `makepkg -si`.

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

Running it this way (a plain `go build` + manual `sudo ./anvild`, not the package) is the
quickest path for local dev iteration, and running as root here just sidesteps every
permission question below for a quick manual test. The packaged version
(`packaging/PKGBUILD`, see below) is the real target: `anvild.service` runs as a
dedicated, unprivileged "anvil" system user instead, with narrowly-scoped
KVM/CAP_NET_ADMIN/Docker access rather than root.

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
  migrate/       SSH-driven single-instance migration (payload/ is the JSON wire shape)
  store/         bbolt-backed registry
  cli/commands/  cobra commands
  tui/           tview app, not built yet
api/proto/       the gRPC API definition
data/distros/    the built-in VM image catalog
packaging/       PKGBUILD, anvild.service, sysusers/tmpfiles rules
scripts/         one-off maintenance scripts (verify-catalog.sh)
```

## License

Apache-2.0.
