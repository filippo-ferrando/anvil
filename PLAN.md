# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

### Recently done

- **Gap review fixes (2026-10).** Export and import paths, staged migration data and
  container volume binds are checked against the caller; instance names are validated.
  Failed creates, rollbacks after a cancelled request, exits during launch, failed port
  changes, failed restores and stops, and VMs left running across a daemon crash leave
  a consistent record. QEMU taps and unreachable QEMUs are cleaned up at start, a cached
  base is never replaced under existing disks, intent networks stay while an instance
  uses them and come back after a prune, and roles are unique per intent. The CLI and
  TUI share SSH, guest-key and launch parsing code in `pkg/client`, every list command
  takes `-o json`, and every service maps missing records to NotFound.
- **Event log.** `anvil events [name] [-f] [-n N] [-o table|json]` shows what happened to
  instances: launch, fork, start, stop, delete, settings, mounts, ports and snapshots run
  through anvil (with the caller's user name and any error), exits and starts outside anvil
  (with the reason), and the daemon's own autostarts, policy restarts and scheduled
  snapshots. anvild keeps the newest 10000 events in bbolt. TUI: an Events screen and the
  selected instance's recent events in the Instances detail panel.
- **Streaming stats.** `InstanceService.StreamStats` sends stats on an interval and ends
  once the instance stops. The TUI detail panel and `anvil stats --watch` use it instead of
  polling `Stats`.
- **GUI apps from instances.** `anvil gui <name> [--x11] -- <app>` shows an app running in
  a VM (waypipe over SSH, tools shared over virtiofs) or a container (fd-passing socket
  relay, `/dev/dri` passed through) as a native Wayland window. `anvil gui
  export|unexport|exports` manage desktop entries with the app's own name and icon. TUI:
  `g` on the Instances screen. Packages ship static `waypipe`/`xwayland-satellite` in
  `/usr/lib/anvil/gui` (`make gui-tools`). See `docs/gui.md`.
- **TUI migrate form suggestions.** Name suggests instances and intents, To suggests known
  host aliases; the add-host form completes the identity path.
- **Migration preflight fixes** and a fix for `anvil tui` over `--remote`.
- **Remote anvild over SSH.** `anvil --remote <[user@]host>` (or `ANVIL_REMOTE`), CLI and
  TUI alike, tunnels gRPC through `ssh <host> anvil dial-stdio`. Guest SSH hops through the
  host with `ssh -J`, container exec/cp go through `DOCKER_HOST=ssh://`, and paths sent to
  the daemon (`mount`, `export`, `import`) are remote paths and must be absolute.
- **JSON output.** Every list-style command (`list`, `find`, `events`, `host list`,
  `snapshot list`, `image list`, ...) takes `-o json` (default `table`), printed with
  `protojson` and the field names from `anvil.proto`.
- **Official catalogue as a submodule.** Extra distro manifests and cloud-init templates
  live in their own repo, checked out at `catalog/`, for `anvil mirror add` and
  `anvil cloud-init import-repo`.
- **Neovim integration removed.** `integrations/nvim` is gone; the CLI and TUI are the
  only clients.
- **Prometheus metrics.** `ANVIL_METRICS=<host:port>` or `unix:<path>` in anvild's
  environment serves `/metrics`: per-instance state plus the live `anvil stats` numbers.

- **Host discovery over mDNS.** anvild announces itself as `_anvil._tcp`
  (`internal/discovery`), so `avahi-browse` sees it too. `anvil host discover` (TUI
  Migration screen: `d`) lists the name, address and SSH port of the other anvil hosts
  on the network. Nothing is saved and no access is granted. `ANVIL_MDNS=off` turns the
  announcement off.

- **Declarative intents.** `anvil apply -f anvil.yaml` (and `A` on the TUI Intents screen)
  plans create, update, recreate, start, keep and delete steps against the current state, then
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
    Output from `cloud-init status` with no `status:` line (the command itself broke, e.g.
    python upgraded mid-boot) counts as unknown, so `result.json` decides, not a false error.
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

### Firecracker backend (done)

Firecracker microVMs are the second VM hypervisor on Linux, next to QEMU, shipped and
tested on real hosts. They have the same lifecycle, commands, TUI screens and intent
membership as QEMU VMs. The only visible difference is `--hypervisor firecracker`
(default `qemu`). The notes below record the design decisions.

Host requirements: `firecracker` and `jailer` (Arch: `extra/firecracker`, 1.17+ for
bzImage and virtio-mem), `/dev/kvm`, `qemu-img` (qcow2 to raw), `nft`. Dev and tests also
need a guest kernel: `ANVIL_FC_KERNEL=<path>` overrides the catalog kernel.

#### Decisions

- **Dispatch.** `instance.VMSpec.Hypervisor` (`qemu` when empty) and a `VmHypervisor` proto
  enum, set up like `ContainerEngine`. The Manager still sees one `KindVM` backend:
  `vm.Hypervisors` hands each call to the QEMU `vm.Backend` or the Firecracker
  `vm.FCBackend` and returns a clear error for an operation one of them lacks. Both live in
  `internal/vm` and share the cloud-init merge, guest agent polling and log code.
  `internal/vm/firecracker` only holds process control and the REST client, like `qemu/`.
- **Kernel.** One anvil-built kernel per arch, everything built in, no modules: Firecracker's
  6.18 guest config plus btrfs, xfs, iso9660, overlayfs, netfilter, NFS client, vsock,
  virtio-mem, SELinux (netfilter, bridge, veth and SELinux come from the base config; no AppArmor). Built by `scripts/build-fc-kernel.sh` (docker, like
  `build-gui-tools.sh`), published as a compressed bzImage release asset, not packaged:
  anvild downloads it on the first Firecracker launch into the Firecracker cache, like a
  distro image (catalog URL + sha256, mirrors apply, `ANVIL_FC_KERNEL` overrides it).
  Kernel updates inside the guest have no effect.
- **Catalog.** A `firecracker` section in the manifest format: the kernel per arch (url,
  sha256, version) plus one entry per supported distro id (same ids as the QEMU catalog, so
  the same image URL, checksum and mirrors apply) with Firecracker-only fields such as extra
  kernel args (`rootflags=subvol=root` for Fedora's btrfs). Built in as
  `data/distros/firecracker.json`, and in the `catalog/` submodule as
  `firecracker_<arch>.json` for `anvil mirror add`.
- **Disks.** Raw only. The qcow2 base is converted once into a Firecracker cache
  (`/var/cache/anvil/images/firecracker/<id>-<arch>.raw`, plus the kernel). An instance
  disk is a reflink copy (btrfs/xfs, falls back to a sparse copy), grown with truncate.
  The root partition is found from the GPT at conversion time and passed as
  `root=PARTUUID=...`, no initrd.
- **cloud-init.** The existing NoCloud seed ISO is attached as a second, read-only
  virtio-blk drive. Library, `--wait` and mounts logic stay unchanged.
- **Guest agent.** qemu-guest-agent over vsock: cloud-init installs it with a drop-in that
  runs `qemu-ga -m vsock-listen -p 3:1024` (OpenRC: conf.d), and the host side dials
  Firecracker's vsock unix socket with `CONNECT 1024`. Same QGA client otherwise, so IPs,
  clean shutdown, `--wait`, guest-exec and fsfreeze all work.
- **Networking.** Intent VMs plug their tap into the intent bridge as QEMU does.
  Standalone VMs get a tap on an anvil-owned `anvil-fc0` bridge (a /24), masquerade in an
  anvil-owned nftables table, a static IP through the seed's network-config, and DNS through
  the intent DNS forwarder on the gateway. SSH and `anvil port` forwards are a small TCP/UDP
  proxy in anvild listening on all interfaces (like SLIRP hostfwd), re-created on Reconcile.
  anvild turns on `net.ipv4.ip_forward` at runtime when it sets up `anvil-fc0`
  (`internal/vm/fcnet.go`), so hosts without Firecracker VMs don't route.
- **Mounts.** A userspace NFSv3 server in anvild (`github.com/willscott/go-nfs`) on a fixed
  port of each gateway, one server per VM picked by source address. Mount/umount go live
  through the guest agent; the fstab line needs no NFS client package in the guest.
- **GUI.** The tools dir goes in as a read-only ISO drive instead of virtiofs.
- **Snapshots.** Disk-only, as for QEMU (restore never resumes RAM): a reflink copy of the
  disk under `snapshots/<name>.raw`. Live: pause over the API, fsfreeze through the agent,
  copy, thaw, resume. Firecracker's own memory snapshot API isn't needed.
- **Resize.** Disk grows live (truncate + `PATCH /drives`, then growpart through the agent).
  Memory grows/shrinks live through virtio-mem up to a boot-time maximum. vCPUs apply at the
  next start (Firecracker has no vCPU hotplug).
- **Export/migration.** Bundles stay qcow2: raw is converted on export and back on import;
  the delta path uses `qemu-img convert -B` against the raw base. The hypervisor travels
  in the spec; the kernel is resolved from the target's catalog.
- **Jailer.** Used when present. The unit gains the capabilities jailer needs
  (`CAP_SYS_ADMIN CAP_SYS_CHROOT CAP_MKNOD CAP_SETUID CAP_SETGID CAP_CHOWN
  CAP_DAC_OVERRIDE`); disks and kernel are hard-linked into the chroot.

## Device passthrough (implemented, not yet tested on real hardware)

Pass host USB and PCI devices to a VM at `anvil launch`. QEMU backend only: Firecracker
VMs and containers reject `--device` with a clear error. Code in `internal/hostdev` and
`internal/vm/devices.go`, user docs in `docs/passthrough.md`. The notes below record the
design decisions.

#### Decisions

- **Spec.** `instance.VMSpec.Devices`, set at create and fixed afterwards (no edit while
  stopped, no live hotplug). A repeatable `--device` flag on `anvil launch`:
  `usb:046d:c52b` (vendor:product), `usb:3-2.1` (bus-port path) or
  `pci:0000:01:00.0`. A vendor:product that matches more than one connected device is
  rejected and the bus-port form is required.
- **QEMU args.** `usb-host` devices on a `qemu-xhci` controller added only when USB
  devices are present, `vfio-pci` for PCI. Plain PCI devices only: no GPU display
  options (x-vga, romfile) for now.
- **IOMMU groups.** A PCI device takes its whole IOMMU group: every endpoint device in
  the group is bound and passed too, and the device list shows the group.
- **Root helper.** anvild stays unprivileged. A socket-activated root service
  (`anvil-devhelper.socket`/`.service`), reachable only by anvild, accepts
  claim/release requests: on claim it unbinds the host driver, binds `vfio-pci` and
  chowns the `/dev/vfio/<group>` or `/dev/bus/usb/...` node to anvil; on release it gives
  the node back and rebinds the original driver. The anvild unit gains the matching
  `DeviceAllow=` lines.
- **Safety.** Any device can be passed, except a built-in denylist the helper enforces:
  the device holding the root filesystem, the NIC of the default route and the GPU
  driving the host display (and anything sharing an IOMMU group with them).
- **Start/stop.** Claim at VM start, release at stop and exit. Start fails with a clear
  error if a device is missing or held by another running VM. Two stopped VMs may list
  the same device.
- **Blocked operations.** Migration and fork refuse an instance with devices. Snapshots
  work as usual. Export keeps the device list, and the imported VM fails at start if a
  device isn't on the new host (same missing-device rule). `anvil apply` and intents
  accept devices (`devices:` in `anvil.yaml`).
- **Listing.** Read `/sys/bus/pci` and `/sys/bus/usb` directly, names from `pci.ids`/
  `usb.ids` when installed; no lspci/lsusb dependency. Each entry shows id, name,
  current driver, IOMMU group (PCI) and status: free, in use by `<instance>`, or blocked
  (denylist).
- **Surface.** gRPC `ListHostDevices` plus `anvil host devices [-o json]` (works with
  `--remote`). TUI: a multi-select device picker in the launch form (qemu VMs only),
  the assigned devices on the instance detail page, and a host devices screen with the
  status of every device.
- **Tests.** Unit tests for id parsing and ambiguity, sysfs listing against a fake sysfs
  tree, IOMMU group expansion, denylist, QEMU args and the migrate/fork refusal; the
  helper claim/release against a fake sysfs root.

- **Locked memory.** VFIO pins all guest RAM against QEMU's `RLIMIT_MEMLOCK`, so the
  anvild unit sets `LimitMEMLOCK=infinity` and QEMU inherits it. virtio-mem works with
  VFIO (QEMU 6.1+, only plugged blocks are mapped). The balloon stays: QEMU turns off its
  discard on its own, so free page reporting returns nothing for these VMs.
- **Claim state.** Kept in the VM's runtime state file next to the QEMU pid: devices held
  and the original driver of each. The helper stays stateless. Reconcile rebuilds
  "in use" from it after an anvild restart; if the QEMU is gone, `removeLeftovers` asks
  the helper to release its devices (chown back, rebind the original driver).

## Container engines

### Podman backend

Docker is the only container engine right now. `--engine podman` returns a clear
"not implemented yet" error instead of silently doing nothing, but there's no actual
Podman code (`internal/container/podman/` is an empty placeholder directory).

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

## Daemon concurrency

Done. Several clients (local CLI, TUI, `--remote` sessions) can call one anvild at once;
writes to the same instance are now serialized. Context: `docs/remote.md`.

What is in place:

- **Per-instance lock** (`internal/instance/lock.go`), taken by every mutating
  `instance.Manager` operation, fork (on the source) and the snapshot scheduler. A second
  caller fails fast with "instance busy"; the restart supervisor and exit handling wait.
- **Hold** for multi-step work: export, migration and `anvil apply` lock their instances
  for the whole run, and their own Stop/Start/Delete calls pass through via the ctx.
- **Name reservation** in Launch/Fork, and `ReserveNames` for import and apply, so data
  dirs and roles can't be claimed twice. `store.PutInstance` refuses a taken name.
- **Patch, not upsert.** `store.UpdateInstance` re-reads and writes in one transaction and
  never recreates a deleted record; the Manager saves only the fields it owns.
- **Image leases.** A create marks the base image, raw copy and kernel it uses in the
  `image.Vault` until its instance is saved. Image delete and the purge prune check those
  leases, the instance list and remove the file under one lock, so only creates of that
  same image block a delete, and none can slip in between.
- **Per-intent lock.** `intent.Manager` locks one intent at a time, so a slow member
  launch only blocks its own intent.
- **Smaller fixes:** stale exit reports are checked against the backend's live state,
  shared image downloads outlive the caller that started them, migration progress is sent from one goroutine at a time,
  the migration base is staged per instance, the migration key uses the `sshkey.Ensure`
  link pattern, `SetMirrorEnabled` is one transaction, and the guest poller has its own
  `VMSpec` copy.

Known limits, left on purpose:

- An `apply` recreate deletes then relaunches the same name; a manual launch of that
  exact name in between wins.

## Migration

- **Live migration.** QMP `migrate` through an SSH tunnel, to move a running VM with a short
  pause instead of a stop, copy and start.

## Platforms

- **Apple Virtualization.framework backend** (`internal/vm/vz`) is scaffolding only.
- **Catalog checksums** are still a work in progress; `scripts/verify-catalog.sh` checks them.
