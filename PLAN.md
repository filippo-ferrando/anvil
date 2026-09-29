# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, 9p mounts, cloud-init library and repo import, VM and
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

### qemu-guest-agent integration

Attach a `virtio-serial` channel for `qemu-guest-agent` and install the agent through
cloud-init. This enables several features at once:

- `guest-fsfreeze-freeze`/`thaw` around live snapshots and fork. Today a live snapshot is
  only as safe as a sudden power loss.
- Reliable guest IP reporting, also for bridge-mode VMs.
- A clean shutdown path that doesn't depend on ACPI, with the current escalation as fallback.
- `anvil launch --wait`, blocking until `cloud-init status` reports done.

### Detect VMs that exit on their own

A guest `poweroff` or a QEMU crash doesn't update the registry: the instance still shows as
running until something re-reads its state. The `exited` channel in `qemu.Process` could
mark the instance stopped (or errored) right away, which also emits a `Watch` event.

### Autostart and restart policy

After a host reboot, `Reconcile` works out each instance's state again but doesn't start
anything. A per-instance or per-intent `--autostart` flag and a `restart=on-failure`
policy would cover long-running services. Needs the exit detection above.

### Resize after creation

`anvil set <name> --cpus/--memory/--disk`. The disk is only sized at create time today
(`Vault.OverlayFor`). CPU and memory can change while stopped. The disk can grow online
with QMP `block_resize`, with cloud-init `growpart` handling the guest side.

### virtiofs mounts without a reboot

`anvil mount` currently restarts the guest to attach a 9p share. `virtiofsd` with
`vhost-user-fs-pci` is much faster than 9p, and on q35 with a PCIe root port it may be
hot-plugged with `device_add`. Needs a check of guest kernel support across the catalog.

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
