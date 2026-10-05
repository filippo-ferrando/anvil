# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

### Recently done

- **Declarative intents.** `anvil apply -f anvil.yaml` (and `A` on the TUI Intents screen)
  plans create, update, recreate, start and prune steps against the current state, then
  applies them in `depends_on` order with a readiness wait. See `docs/apply.md`.
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
- **Consistent fork of a running VM.** A running source is copied with a QEMU backup
  job (`blockdev-backup`, top layer only), which captures the disk at the instant it
  starts while the VM keeps running. With the guest agent, filesystems are frozen just
  for that instant (`guest-fsfreeze`), so the fork is filesystem-consistent.
- **Autostart and restart policy.** `--autostart` starts an instance when anvild starts,
  unless it was stopped on purpose. `--restart on-failure[:N]|always` restarts an
  instance that stopped on its own (exit detection above), with backoff from 1s to 1m;
  on-failure gives up after N restarts that didn't run for 10 minutes.
- **Resize after creation.** `anvil set --cpus/--memory/--disk` (TUI: `u`). The disk
  grows live over QMP `block_resize`, and with the guest agent the root partition and
  filesystem too (cloud-init's growpart/resizefs); CPU/memory of a running VM apply at
  its next start.
- **Live CPU and memory changes.** An x86_64 VM boots with headroom up to the host's CPU
  count (capped at 64) and memory: every vCPU sits in its own socket so it can be plugged or
  unplugged over QMP, and extra memory comes from a `virtio-mem` device. `anvil set
  --cpus/--memory` (TUI: `u`) then applies live; the guest agent onlines new vCPUs and
  memory. Memory can't go below what the VM booted with until it restarts, and a shrink
  settles for what the guest could free. aarch64 VMs still apply these at the next start.
- **Scheduled snapshots.** `anvil snapshot schedule <vm> --every 6h --keep 8` (TUI: `S`
  on the Snapshots screen): live snapshots named `auto-<UTC time>`, only those pruned,
  stopped VMs skipped.
- **TUI coverage.** Everything above is reachable from the TUI (launch form: autostart and
  restart policy; `u` settings; `S` snapshot schedule; detail panel: size, policies, schedule): guest agent/IP/cloud-init
  in the detail panel and list rows, launch toggles for the agent and `--wait`, `w` to
  wait for cloud-init on a running VM (esc stops waiting), live mounts with the current
  mounts listed and suggested by umount, image checksums with `h` on the Images screen,
  and state changes (including outside exits) showing up on their own through `Watch`.

## VM hypervisors

### Firecracker backend

QEMU is the only VM hypervisor on Linux right now. Firecracker microVMs should be a
second one, with the same lifecycle, commands, TUI screens and intent membership as QEMU
VMs. The only visible difference is a `--hypervisor firecracker` flag (default `qemu`).

- **Selection.** Add a `Hypervisor` field to `VMSpec` and a `VmHypervisor` enum in the
  proto, set up the same way as `ContainerEngine`. The Manager still sees one `KindVM`
  backend, which hands each call to QEMU or Firecracker based on the spec.
- **Process control.** Code lives in `internal/vm/firecracker`: start the `firecracker`
  binary per VM (optionally under `jailer`) and drive it over its REST API on a unix
  socket, the same way QMP is used for QEMU.
- **Images.** Firecracker boots an uncompressed kernel plus a raw rootfs, not a qcow2
  cloud image. The catalog needs a kernel per distro, and qcow2 bases have to be converted
  to raw (or ext4) disks on pull -> use a dedicated "cache" to store the firecracker images.
- **cloud-init.** No CD-ROM device exists, so the NoCloud seed goes on a second virtio-blk
  drive, or through MMDS. The cloud-init library and `--wait` should work unchanged.
- **catalog**. create a dedicated Firecracker catalog with the same distros, versions and checksums as the QEMU
  catalog.
- **Networking.** No SLIRP, only tap devices. Intent VMs plug the tap into the intent
  bridge as they do today. Standalone VMs need a small per-host bridge plus DNAT rules
  (or a userspace proxy) for the SSH port and `anvil port` forwards.
- **Gaps to decide.** No virtiofs, so `anvil mount` either returns a clear "not
  supported on firecracker" error or falls back to a block-device share. Snapshots
  use Firecracker's own full-VM snapshot API, and fork, export/import and migration
  must carry the kernel and hypervisor type in the bundle. Unsupported operations
  return a clear error rather than doing nothing, like `--engine podman` does today.
- **Tests.** A spawn integration test like `spawn_integration_test.go` that skips when
  `firecracker` or `/dev/kvm` is missing, plus unit tests for the REST client.

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

## Intents

### GitOps sync

Builds on `anvil apply` (`internal/intent/apply`). The daemon watches a git repo (GitHub or any git remote)
and keeps the host strictly in line with every `anvil.yaml` found in it, recursively.

- **Source.** `anvil gitops add <name> --repo <url> [--branch main] [--path infra/]`,
  saved in bbolt. The daemon shells out to the local `git` binary, as migration does
  with `ssh`, so existing SSH keys or credential helpers are enough for private repos.
- **Watch.** Poll the remote on an interval (default 1m) and apply only when the commit
  changes. The daemon listens on a unix socket only, so push webhooks are out of scope.
- **Reconcile.** On each new commit, load every `anvil.yaml` under the path and run the
  same compare-and-apply as `anvil apply`, in one pass over the whole repo.
- **Strict coherence.** Instances and intents created by a sync are tagged with their
  source. Anything tagged but no longer in the repo is deleted, and manual changes to
  tagged instances (stop, delete, edit) are reverted on the next check. Untagged
  instances are never touched.
- **Safety.** A repo that fails to parse, or a failed apply, leaves the current state as
  is and records the error. `--dry-run` on `anvil gitops sync` shows the plan without
  applying it, and `anvil gitops pause <name>` stops reconciling for manual work.
- **Surface.** gRPC service plus `anvil gitops add/list/remove/status/sync/pause/resume`.
  A TUI screen shows each source with its last synced commit, drift and last error.
- **Tests.** Unit tests for the reconcile diff, including pruning and drift revert,
  against a local bare repo created in the test.

## Migration

- **Live migration.** QMP `migrate` through an SSH tunnel, to move a running VM with a short
  pause instead of a stop, copy and start.

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
