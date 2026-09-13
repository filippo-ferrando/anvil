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
- **Containers support both Docker and Podman.** Docker gets built first (milestone 3),
  Podman second. Both talk to their engine's REST API directly (Docker's official Go SDK,
  Podman's `containers/podman/v5/pkg/bindings`), no shelling out to `docker`/`podman` CLI
  binaries. `ContainerSpec` gets an `engine` field so a container instance says which one
  it's running on.
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
    vm/                                QEMU backend: qemu/, cloudinit/, image/, network/
    container/docker/                  Docker backend (not built yet, built first)
    container/podman/                  Podman backend (not built yet, built second)
    intent/                            intent group registry (not built yet)
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

Not built yet (milestone 3). We support both Docker and Podman, Docker first.

Both engines implement the exact same `instance.Backend` interface the VM code already
uses, so `list/start/stop/delete/info` keep working uniformly no matter what's actually
running the container. `ContainerSpec` gets an `engine` field (`docker` or `podman`) so
an instance record says which one owns it, plus a daemon-level default (falls back to
whichever engine is actually configured if only one is, defaults to Podman once both
are around, matching the original "Podman is the default" idea, docker is just what
gets built first).

Why two backends instead of pointing Docker's client at Podman's Docker-compatible
socket and calling it done: Podman's compatibility layer doesn't cover every native
feature (and we specifically want Podman's own pod/network primitives for intents later),
so a real native Podman backend is worth having, we just don't want to block on it before
Docker works.

- **Docker** (built first): `github.com/docker/docker/client`, the official SDK, against
  `/var/run/docker.sock`. `ContainerSpec` maps pretty directly onto
  `container.Config`/`container.HostConfig`.
- **Podman** (built second): `containers/podman/v5/pkg/bindings` against `podman.sock`,
  `ContainerSpec` maps onto Podman's own spec generator shape.

Users point `--image` at literally any OCI image either way, we're not curating a
container catalog the way we do for VM distros.

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

Container registry mirrors are stored the same way (`--kind container`) but not applied
anywhere yet, that's milestone 3 alongside the container backends themselves. And it's
worth flagging now: Docker and Podman don't handle this the same way. Podman lets you
drop an arbitrary per-registry mirror config under `/etc/containers/registries.conf.d/`
and it just works. Docker's `registry-mirrors` setting in `daemon.json` is really built
for mirroring Docker Hub specifically, it's not as flexible about mirroring an arbitrary
registry. We'll figure out the actual mechanism for Docker mirrors when we get there,
don't assume it'll look symmetric to the Podman side.

### Instance registry

bbolt, not flat JSON files. Reason: transactional writes mean we don't have to worry
about a half-written file if the daemon dies mid-write, which matters more here because
the reconciliation logic (see "VM backend" above) starts from what's in the registry at
startup and corrects it, rather than trusting it outright.

### Intents

Not built yet (milestone 4). An intent is a named group of instances, VMs and/or
containers, each tagged with a role (`web`, `db`, whatever). The important decision here:
**every member of an intent shares one network**, no matter what kind it is, and
**intents don't talk to each other** by default.

How that's supposed to work: if an intent has at least one container member, we create a
real Podman network for it (bridge driver), and attach any VM members' QEMU tap devices
to that same underlying Linux bridge. If it's VM only, we make our own bridge and run our
own dnsmasq for DHCP, no Podman involved. Checked this against Podman's docs: rootful
Podman with netavark does create a plain Linux bridge you can inspect the name of
(`podman network inspect <name> --format '{{.NetworkInterface}}'`), and it's just a
normal kernel bridge under the hood, nothing exotic, so attaching an external tap device
to it should just work. This only holds for rootful Podman though, rootless uses
something called Pasta instead of a real bridge. Since `anvild` already needs to run
privileged for KVM access, running Podman rootful too isn't a new requirement.

Docker does the same basic thing for its own user-defined bridge networks (a real Linux
bridge you could in theory attach a tap device to), so the same trick should carry over,
but that's not verified yet either. And now that containers can be Docker or Podman, an
intent that mixes VMs with both engines at once is a genuinely harder case, two container
engines each wanting to own "the" bridge. Simplest way out for now: an intent's container
members all use the same engine. Don't build anything that assumes otherwise until
there's an actual reason to.

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
- [ ] `ContainerSpec` gets an `engine` field (docker/podman), wired through the proto,
      daemon and CLI (`--engine`, or a sensible default if only one engine is configured)
- [ ] **Docker backend**: `github.com/docker/docker/client` against `/var/run/docker.sock`,
      `--kind container` actually works end to end
- [ ] **Podman backend**: `containers/podman/v5/pkg/bindings` against `podman.sock`
- [ ] figure out container registry mirrors for real, Podman's `registries.conf.d`
      drop-ins are straightforward, Docker's `daemon.json` `registry-mirrors` is a
      narrower mechanism, don't assume they end up looking the same

### M4: intents
- [ ] intent data model, own bbolt bucket, references instance IDs not embedded specs
- [ ] `anvil intent create/add/list/info/remove/delete`
- [ ] shared per-intent network: container-engine network + tap attach for mixed
      VM/container intents (Docker or Podman, not both in the same intent, see the
      Intents section above), anvil-owned bridge + dnsmasq for VM-only intents
- [ ] confirm intents are actually isolated from each other by default

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
