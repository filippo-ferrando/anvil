# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

### Recently done

- **Man pages.** `anvil man <dir>` generates the page tree through cobra's `GenManTree`.
- **VM disk I/O tuning.** The main disk runs on its own iothread with
  `discard=unmap,detect-zeroes=unmap`, so guest `fstrim` shrinks the qcow2 file again.
  `cache=none` and `aio=io_uring` (or `aio=native`) are used when the host supports them,
  probed at start by `qemu.ProbeDiskTuning`.
- **virtio-rng and virtio-balloon.** First boot no longer waits on guest entropy, and
  free page reporting hands memory freed by the guest back to the host.
- **Parallel start/stop.** `Manager.Start`/`Stop` drive up to 4 instances at once and try
  every instance even if one fails. SLIRP SSH ports handed out in parallel never collide.
- **Resumable image downloads.** A broken download resumes from the `.part` file with an
  HTTP `Range` request (guarded by `If-Range`), up to 4 attempts, also across daemon
  restarts. The SHA256 of each cached base image is kept in a `.sha256` sidecar file.
- **Faster, resumable migration.**
  - When the target already caches the same base image (same SHA256, checked with
    `anvil image checksum` over SSH), only the disk delta is sent (`qemu-img convert -B`)
    and the target rebases it onto its own copy. Otherwise the disk is flattened as before.
  - The disk goes over plain `ssh`, compressed with zstd when both hosts have it, resumes
    from the remote file size after a broken connection, and is checked with `sha256sum`.
  - The target stages the disk in `/var/tmp` instead of `/tmp`, which is often a small tmpfs.
  - `scp` is no longer needed for migration.
- **Instance change stream.** `InstanceService.Watch` streams instance changes. `anvil watch`
  prints them, and the TUI refreshes the Instances, Snapshots and Intents screens on its own,
  so changes made from the CLI or by a migration show up without a manual reload.
- **qemu-guest-agent integration.** Every VM gets a virtio-serial channel for the agent,
  and cloud-init installs and starts `qemu-guest-agent` (`--no-guest-agent` skips it).
  - **Guest IPs**: a per-VM poller asks the agent for addresses; they show in `anvil list`,
    `anvil info`, the TUI detail panel, and `Watch` events.
  - **Clean shutdown**: `anvil stop` asks the agent to power off first, then falls back to
    ACPI and finally a hard stop, all within the same timeout.
  - **`anvil launch --wait` / `anvil wait`**: block until cloud-init is done, read through the
    agent (`cloud-init status`, or `result.json` where guest-exec is blocked), or from the
    serial console when there is no agent. Cloud-init errors make the command fail.
- **virtiofs mounts, live.** 9p is gone: each mount is served by its own `virtiofsd`
  (namespace sandbox, falling back to none) on a `vhost-user-fs-pci` device. Guest RAM is a
  shared memfd and every VM has 8 PCIe root ports, so with a connected guest agent
  `anvil mount`/`umount` hot-plug the device and mount it in the guest, no restart. Without
  the agent (or when guest-exec is blocked) the old path runs: rebuild the seed and restart.
  A generation-guarded bootcmd keeps the guest's fstab in line, and VMs with 9p mounts are
  moved to virtiofs on their next start. `virtiofsd` is now a package dependency.
- **VMs that exit on their own.** A guest poweroff, crash or kill no longer leaves the
  instance shown as running: the backend notices the QEMU exit, frees the tap device,
  virtiofsd processes and runtime state, and records `stopped` (exit status 0) or `error`
  (anything else), which also emits a `Watch` event.
- **Containers that change state outside anvil.** The Docker backend follows dockerd's
  `/events` stream: a container that dies without anvil stopping it is recorded `stopped`
  (exit 0, or SIGINT/SIGTERM from an outside `docker stop`) or `error` (crash, SIGKILL,
  OOM), and one started again from outside goes back to `running`. Exits missed while
  dockerd was unreachable are caught up on reconnect.
- **TUI coverage.** Everything above is reachable from the TUI: guest agent/IP/cloud-init
  in the detail panel and list rows, launch toggles for the agent and `--wait`, `w` to
  wait for cloud-init on a running VM (esc stops waiting), live mounts with the current
  mounts listed and suggested by umount, image checksums with `h` on the Images screen,
  and state changes (including outside exits) showing up on their own through `Watch`.

## Container engines

### Podman backend

Docker is the only container engine right now. `--engine podman` returns a clear
"not implemented yet" error instead of silently doing nothing, but there's no actual
Podman code (`internal/container/podman/` is an empty package).

A few things are blocked on this landing:

- **Podman engine support.** The `anvil` CLI needs to be able to talk to Podman
  instead of Docker, and `anvild` needs to be able to run Podman containers.
- **Cross-kind networking for Podman.** A VM and a container sharing an entire
  network is done and working for Docker; there's no equivalent design for Podman/
  netavark yet.
- **Mixing engines in one intent.** Whether an intent can contain both a Docker
  container and a Podman container at once is undecided, current plan is "don't
  allow it" until there's a real reason to.

## VM features

### Consistent fork of a running VM

`anvil fork` copies the disk of a running VM with `qemu-img convert -U`, which can catch
writes halfway. Now that the guest agent exists, `guest-fsfreeze-freeze`/`thaw` around the
copy (or a short-lived QMP blockdev snapshot) would make it consistent. Live snapshots use
`savevm`, which pauses the VM and saves its memory, so they don't need this.

### Autostart and restart policy

After a host reboot, `Reconcile` works out each instance's state again but doesn't start
anything. A per-instance or per-intent `--autostart` flag and a `restart=on-failure`
policy would cover long-running services. Exit detection already reports a crash as
`error` and a guest poweroff as `stopped`, which is what a restart policy would act on.

### Resize after creation

`anvil set <name> --cpus/--memory/--disk`. The disk is only sized at create time today
(`Vault.OverlayFor`). CPU and memory can change while stopped. The disk can grow online
with QMP `block_resize`, with cloud-init `growpart` handling the guest side.

### Scheduled snapshots

`anvil snapshot schedule <name> --every 6h --keep 8`, run by the daemon on top of the
existing snapshot primitives, with retention.

## Intents

### Declarative intents

`anvil apply -f anvil.yaml`: a compose-like file that the daemon compares against the
current state and applies. Includes `depends_on` start order with a readiness check, so a
`db` member is up before `app` starts. Parallel start currently starts all members at once.

## Migration

- **Arbitrary bind mounts aren't migrated.** Moving a container only carries over
  anvil-managed volumes; a host bind mount's actual data doesn't travel with it.
  `--dry-run` should warn about this instead of staying silent.
- **Base image transfer from the source.** When the target lacks the base image, the
  whole flattened disk is sent. Sending the base image once and then only deltas would
  help hosts that migrate many VMs of the same distro, and hosts without internet access.
- **Live migration.** QMP `migrate` through an SSH tunnel, to move a running VM with a short
  pause instead of a stop, copy and start.
- **Delta export bundles.** `anvil export` could use the same base-image delta as migration
  when the importing host has the base image.
- **Host key trust.** Migration SSH uses `StrictHostKeyChecking=accept-new`, which is
  trust on first use. This should be documented, with an option to require a known key.

## Discovery and observability

### mDNS discovery of other Anvil hosts

Migration currently relies entirely on `anvil host add` (a saved `user@host` alias).
There's no automatic discovery of other machines running `anvild` on the same
network (`internal/discovery/` is an empty package). Discovery wouldn't grant any trust by
itself either way, so it's a pure convenience feature, not a blocker for anything.

### Metrics and event history

- An optional Prometheus `/metrics` endpoint (opt-in, on its own socket or port) exposing
  the per-instance stats the daemon already collects.
- A small persisted event log (`anvil events`) for create/start/stop/migrate/failure, on
  top of the live `Watch` stream.
- Streaming stats over `Watch` or a sibling RPC, so the TUI stops polling `Stats` every 2s.

## Platforms

- **Apple Virtualization.framework backend** (`internal/vm/vz`) is scaffolding only.
- **Catalog checksums** are still a work in progress; `scripts/verify-catalog.sh` checks them.
