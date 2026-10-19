# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

### Recently done

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
- **JSON output.** `anvil list` and `anvil find` take `-o json` (default `table`), printed
  with `protojson` and the field names from `anvil.proto`.
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

### Firecracker backend (in progress)

QEMU is the only VM hypervisor on Linux right now. Firecracker microVMs are being added as
a second one, with the same lifecycle, commands, TUI screens and intent membership as QEMU
VMs. The only visible difference is `--hypervisor firecracker` (default `qemu`).

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
  virtio-mem, SELinux/AppArmor. Built by `scripts/build-fc-kernel.sh` (docker, like
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
  Packaging adds a sysctl.d file for `net.ipv4.ip_forward`.
- **Mounts.** A userspace NFSv3 server in anvild (`github.com/willscott/go-nfs`), one
  export per mount, bound to the gateway and answering only that VM's IP. Mount/umount go
  live through the guest agent; cloud-init installs the NFS client.
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

#### Phases

Each phase ends buildable, with its tests, and is ticked here when done.

1. [x] **Selection plumbing.** Proto enum + `VMSpec.hypervisor`, `instance.VMHypervisor`,
   convert.go, `anvil launch --hypervisor`, `anvil.yaml` `hypervisor:`, TUI launch form
   field, `vm.Hypervisors` dispatcher (Firecracker returns "not implemented yet"), list/info
   show the hypervisor.
2. [x] **`internal/vm/firecracker`.** REST client (machine-config, boot-source, drives,
   network-interfaces, vsock, actions, vm state, PATCH drives, memory hotplug), process
   spawn/attach/stop, vsock dialer. Unit tests against an httptest unix socket; spawn
   integration test that skips without `firecracker`, `/dev/kvm` or `ANVIL_FC_KERNEL`.
3. [x] **Kernel and images.** `scripts/build-fc-kernel.sh` + config fragment, `make fc-kernel`,
   `firecracker.json` catalog, raw conversion cache with GPT root lookup, reflink disk copy.
   CI caches the built kernel keyed on kernel version + config fragment + build script hash
   (same pattern as the build-gui cache from a03c738), so it only rebuilds when one changes.
4. [ ] **Lifecycle.** `FCBackend` Create/Start/Stop/Delete/Status/Logs/Reconcile, exit
   watch, seed drive, console log, guest agent over vsock, WaitReady, GuestInfo, intent
   bridge networking, Stats.
5. [ ] **Standalone networking.** `anvil-fc0` bridge, nftables masquerade, IPAM, DNS zone,
   port proxy, AddPort/RemovePort, packaging sysctl.
6. [ ] **Disk and resources.** Snapshots, scheduled snapshots, fork, disk resize, live
   memory, CPUs at next start.
7. [ ] **Mounts and GUI.** NFS server and live mount/umount, GUI tools drive, PrepareGUI.
8. [ ] **Export, import, migration.** Hypervisor-aware disk conversion on both ends.
9. [ ] **Jailer and packaging.** Jailer chroot, unit capabilities, firecracker optdepend in
   all three package formats, docs (`docs/firecracker.md`), man pages.

#### Status notes

(Updated at the end of each phase: what landed, what's left, anything surprising.)

- **Phase 1 (done).** `VmHypervisor` enum + `VMSpec.hypervisor` (field 24), `instance.VMHypervisor`
  with `VMSpec.HypervisorOf()` (empty = qemu, so stored VMs need no migration), converters,
  `anvil launch --hypervisor`, `hypervisor:` in `anvil.yaml` (a change means recreate), TUI
  launch form field and "VM (firecracker)" in the detail panel, list ENGINE column and
  `anvil info` show the hypervisor. `vm.Hypervisors` (`internal/vm/hypervisors.go`) is now
  the KindVM backend; its `Firecracker` field is nil, so firecracker launches fail with "isn't
  implemented yet". A missing capability fails with "<op> isn't supported on firecracker".
  Left for phase 8: export/migration calls are QEMU-only in the dispatcher,
  `PrepareImportedDisk` has no spec so it can't pick a hypervisor, and the migration target's
  `anvil launch --from-disk` call must pass `--hypervisor`. Fork keeps the source's
  hypervisor through `cloneVMSpecForFork`, with `NetworkMode` "slirp" meaning standalone.
- **Phase 2 (done, real-VM test pending a kernel).** `internal/vm/firecracker`: `Config`
  (`--config-file` JSON: boot source, machine config, drives with `partuuid`, network
  interfaces, vsock, memory-hotplug, entropy), so the VM boots on spawn with no pre-boot API
  calls. `Client` covers the runtime calls only: `Info`, `SendCtrlAltDel`, `Pause`/`Resume`,
  `UpdateDrive` (PATCH, picks up a grown file), `MemoryHotplug`/`SetMemoryHotplug`.
  `Process` mirrors `qemu.Process` (Spawn, Attach by `/proc/<pid>/cmdline`, WaitAPI, Exited,
  Stop = Ctrl+Alt+Del then SIGKILL). Serial console is firecracker's stdout (`console.log`),
  its own log plus stderr go to `firecracker.log`. `DialVsock` does the `CONNECT <port>`
  handshake; `qemu.GuestAgent.Dial` lets the QGA client run over it.
  To check in phase 4 with a real guest: whether a guest `poweroff` (agent `guest-shutdown`
  mode powerdown) makes firecracker exit, or the agent has to use mode `reboot`.
- **Phase 3 (done, kernel not built yet).** `scripts/build-fc-kernel.sh [out] [x86_64|aarch64]`
  (`make fc-kernel`, `ARCH=aarch64` to cross-build) builds Linux 6.18.57 from Firecracker
  v1.17.0's `microvm-kernel-ci-<arch>-6.18.config` merged with `scripts/fc-kernel.config`
  (partition tables, btrfs/xfs/vfat/iso9660, NFSv3, vsock, virtio-mem, i8042 for
  Ctrl+Alt+Del, overlayfs, wireguard). It fails if `olddefconfig` drops a fragment option,
  and fixes the build timestamp/user/host so rebuilds are byte-identical. Output:
  `anvil-fc-kernel-<version>-<arch>` (bzImage on x86_64, Image on aarch64) plus its `.config`.
  CI: `.github/workflows/fc-kernel.yml`, separate from package releases, runs on tags
  `fc-kernel-*` (and by hand), caches `build/fc-kernel` keyed on the script + fragment hash,
  and publishes the kernels plus `SHA256SUMS` as that tag's release.
  `data/distros/firecracker.json` (embedded as `distros.FirecrackerRaw`): kernel per arch
  (URL assumes the Forgejo release `fc-kernel-6.18.57`, **sha256 still empty until the first
  build is published**) and every main-catalog distro id, Fedora with
  `rootflags=subvol=root`. Distros other than the ones tested in phase 4 are unverified.
  `internal/vm/image/firecracker.go`: `FCCatalog`, `FCCache` (`config.FirecrackerImageDir()`,
  `/var/cache/anvil/images/firecracker`) with `Kernel` (downloaded on first use through the
  Vault's downloader; `ANVIL_FC_KERNEL` overrides) and `RawBase` (qcow2 base converted once,
  again when the base is newer), `CloneDisk` (`cp --reflink=auto --sparse=always`, then
  grow). `partition.go`: `RootPartUUID` reads GPT (arch root type, else largest Linux data,
  else largest) or MBR (`<signature>-<NN>`) from the instance disk itself.
  Mirrors: a VM mirror manifest takes an optional `firecracker` section (kernels + distro
  entries, merged by priority like images, ignored by older anvil). The embedded
  `firecracker.json` uses the same shape, so `Catalog.FirecrackerKernel`/`FirecrackerDistro`
  cover built-in and mirrors alike. The `catalog/` submodule has `firecracker_x86_64.json`
  and `firecracker_aarch64.json` (the `-arm` ids), documented in its README and
  `docs/mirrors.md`.
  Open: `anvil image list/delete` don't show the raw copies and kernels yet.

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

## Platforms

- **Apple Virtualization.framework backend** (`internal/vm/vz`) is scaffolding only.
- **Catalog checksums** are still a work in progress; `scripts/verify-catalog.sh` checks them.
