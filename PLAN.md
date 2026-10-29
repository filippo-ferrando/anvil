# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Firecracker microVMs, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

## Container engines

### Podman backend

Docker is the only container engine right now. `--engine podman` returns a clear
"not implemented yet" error instead of silently doing nothing, but there's no actual
Podman code yet.

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

## VMs

### Ephemeral VMs and matrix runs

Status: designed, not started. Each milestone below is one reviewable change with its own
tests; do them in order.

**Commands.**

- `anvil launch --ephemeral <distro> [path] -- <cmd>`: restore a clone from a warm snapshot,
  run `<cmd>`, stream its output, delete the clone, exit with the command's exit code.
- `anvil matrix ubuntu,arch,alpine [path] -- <cmd>`: the same, one clone per distro, all in
  parallel. Exits non-zero if any clone failed.
- `anvil warm create|list|rm`: manage warm snapshots by hand (runs also build them on demand).
- `anvil ephemeral list|shell|rm`: clones kept by `--keep-failed`.
- Shared flags: `--cpus`, `--memory` (part of the warm key), `--logs <dir>`, `--keep-failed`.

`path` is optional, positional, before `--` (after `--` it can't be told apart from the
command's own arguments). `<distro>` resolves through the catalog like `anvil launch` does.

**Hard requirements, checked up front with a clear error, no fallback:** Firecracker
hypervisor, and the jailer in use (`jailerExe()` in `internal/vm/fc_jail.go` not empty). The
packages ship a static firecracker/jailer for this (`scripts/fetch-firecracker.sh`).

#### Design

**Warm snapshot.** A Firecracker full snapshot (vmstate + memory file) plus a reflink copy of
the disk taken at the same moment. The pristine base disk does not match: the memory holds
the guest's page cache.

- Not tied to an instance: own bbolt bucket in `internal/store` (follow `mirrors.go`), files
  under `StateDir/warm/<key>/` (`vmstate`, `mem`, `disk.raw`, `seed.iso`, `guest.iso`).
  Same filesystem as `config.JailerDir()`, so `jailFile()` hard-links them into each clone's
  jail; a copy of the memory file would kill the speed.
- Key: hash of distro + version + arch, CPUs, memory, the generated cloud-init, Firecracker
  version, kernel hash, `anvil-guest` hash. A run with no match builds one first (slow once),
  then reuses it. Never exported or migrated.
- Build: create a normal Firecracker VM in the clone network layout (below), wait for
  cloud-init (`ReadyWaiter`), check `anvil-guest` answers, freeze guest filesystems
  (`guest-fsfreeze-freeze`, `internal/vm/qemu/qga.go`), pause, `PUT /snapshot/create`
  (`snapshot_type: Full`), reflink the disk, stop and delete the build VM.
- No virtio-mem: skip `cfg.MemoryHotplug` (`fcHotplugMiB` in `fc_backend.go`) for warm VMs.
  Default memory small (512 MiB): the memory file is as large as the guest RAM.
- Every drive of the build VM must exist at the same in-jail path in each clone
  (`/disk.raw`, `/seed.iso`, `/guest.iso`), the snapshot records them. Same for
  `vsock.sock` and the tap name.

**Clone restore.** A second spawn path: firecracker started with no `--config-file`
(`Paths.command()` in `internal/vm/firecracker/process.go` always passes it today), then
`PUT /snapshot/load` with `mem_backend: {backend_type: File}` and `resume_vm: true`. The
memory file is mapped private, so clones of one warm snapshot share its pages. After load:
set the guest clock (`guest-set-time` over the agent; a stale clock breaks TLS and apt),
then dial `anvil-guest`.

Clone IDs carry an `eph-` prefix. Clones live only in anvild memory, not in bbolt; on start
anvild removes leftover `eph-` jails and netns. A client disconnect or Ctrl-C deletes the
run's clones (except kept ones).

**Clone network: NAT, one netns per clone.** Every clone has the same MAC and IP, so each
gets its own netns with an identical inside layout: tap `tap0` with `10.200.0.1/30`, guest
`10.200.0.2`. The warm build VM uses the same layout, so the restored guest config is right
as is.

- A veth pair with a unique /30 per clone links the netns to the host; MASQUERADE inside the
  netns (guest to veth) and on the host (veth to uplink, like `fcNftRules` in `fcnet.go`).
  Outbound only, no inbound.
- Creating a netns and configuring links inside it needs `CAP_SYS_ADMIN`, which anvild
  doesn't have (`anvild.service` gives it only `CAP_NET_ADMIN`). Add `netns-create` /
  `netns-delete` requests to the root helper (`internal/hostdev/helper.go`, socket
  `anvil-devhelper.socket`): it creates `/run/netns/anvil-<id>`, the veth pair, `tap0` owned
  by the anvil user, addresses and the in-netns nft rule, then returns. Host-side veth
  address and host NAT stay in anvild.
- Firecracker joins it through the jailer's `--netns /run/netns/anvil-<id>` (add to
  `Jail` in `process.go`).

**Guest exec: `anvil-guest`.** A small static Go binary (`cmd/anvil-guest`,
`CGO_ENABLED=0`), shipped by the anvild package in `/usr/lib/anvil/guest/anvil-guest`. Not
the `anvil` CLI: the anvild package doesn't depend on the anvil one, and anvild itself is
not static.

- Delivered like the GUI tools: packed into a read-only ISO drive (see `toolsISO()` in
  `internal/vm/fc_gui.go`), installed and started by cloud-init as a service, same pattern
  as `fcAgentUnit` / `fcAgentStartCmd` in `internal/vm/backend.go` (systemd and OpenRC).
  Listens on vsock port 1025 (qemu-ga is on 1024).
- Runs as part of the warm snapshot, so every clone restores with it already listening.
  Each clone has its own vsock socket, so no network is involved.
- Protocol, one connection per command: a JSON header (argv, env, cwd, tty, has_upload),
  then the optional upload as a tar stream, then length-prefixed frames both ways: stdin
  and resize from host, stdout, stderr and exit code from guest. Commands run as root, cwd
  `/work` when a path was uploaded.

**gRPC.** `rpc Run(stream RunInput) returns (stream RunEvent)`, bidirectional because the
code path lives on the client (it may be `--remote`). First input: the request (distros,
command, flags); then tar chunks of the path. anvild resolves warm snapshots, restores
clones, fans the tar out to every clone, merges output. Events: progress (building warm
snapshot, restoring), `{clone, distro, stream, data}`, `{clone, distro, exit_code,
duration}`. `WarmCreate` (streaming progress), `WarmList`, `WarmDelete` for `anvil warm`.

**CLI** (`internal/cli/commands/run.go`, `warm.go`; client only, no logic).

- Tar the path honoring `.gitignore`: `git ls-files --cached --others --exclude-standard`
  inside a git repo (same trick as `packaging/rpm/build.sh`), every file otherwise.
- Output: each line prefixed with a colored `[distro]`, split on newlines so lines never mix;
  then a summary table (distro, exit code, duration). `--logs <dir>` writes one full log
  per distro. `anvil warm list` has `-o table|json` (`addOutputFlag` in `table.go`).

**TUI.** A warm snapshots section (list, create, delete) and a run view: a form for distros,
path and command, then one pane per clone with its output and exit code.

#### Milestones

1. **Firecracker snapshot API.** `CreateSnapshot`, `LoadSnapshot` in
   `internal/vm/firecracker/client.go`; `Paths.command()` without `--config-file`;
   `Jail.NetNS` (`--netns`). Unit tests against the fake API server in `client_test.go`;
   integration test in `fc_integration_test.go` style: boot, pause, snapshot, restore in a
   new firecracker, agent ping.
2. **Clone netns.** Root helper requests, host-side veth and NAT, cleanup. Test the netns
   setup under `unshare -rn` (never on the host network).
3. **`anvil-guest`.** Binary, protocol, ISO delivery, cloud-init service. Unit tests for the
   protocol over a `net.Pipe`, upload extraction, exit codes, tty.
4. **Warm snapshots.** Store bucket, key, build, `WarmCreate/List/Delete`, `anvil warm`,
   TUI section. Unit test for the key; integration test builds one.
5. **Run.** `Run` RPC, ephemeral and matrix, upload, CLI output, `--logs`, cancel cleanup,
   `eph-` sweep on start. Unit tests for the line prefixer and summary; integration test
   runs `uname -a` on two clones of one warm snapshot in parallel.
6. **TUI run view, `--keep-failed`, `anvil ephemeral list|shell|rm`** (shell = tty exec
   over `anvil-guest`), docs (`docs/ephemeral.md`, `docs/firecracker.md`), `make man`.

Real-VM tests need `ANVIL_FC_KERNEL` and `ANVIL_E2E_IMAGE` (`docs/firecracker.md`) and skip
without them.

#### Check before building on it

- Firecracker 1.17 snapshot/load request bodies: `firecracker_spec-v1.17.0.yaml` ships in
  the release tarball `scripts/fetch-firecracker.sh` downloads.
- Snapshot support for the devices a warm VM uses: vsock, entropy, serial console, two
  read-only drives. Memory hotplug is left off on purpose.
- Guest RNG after restore: `CONFIG_VMGENID` is not in `scripts/fc-kernel.config`. Check that
  Firecracker 1.17 exposes VMGenID on x86_64 and aarch64; if so, add it to the kernel config
  and publish a new `fc-kernel-*` release.
- qemu-ga's listener survives a restore (Firecracker resets vsock connections on snapshot);
  if not, restart it through `anvil-guest`.

#### Not in scope

QEMU ephemeral runs, inbound networking to clones, a `--parallel` limit (all clones start
at once; add a limit when a large matrix exhausts host memory), user cloud-init for
ephemeral runs.

## Daemon concurrency

Several clients can call one anvild at once (`docs/remote.md`).

Known limits, left on purpose:

- An `apply` recreate deletes then relaunches the same name; a manual launch of that
  exact name in between wins.

## Migration

- **Live migration.** QMP `migrate` through an SSH tunnel, to move a running VM with a short
  pause instead of a stop, copy and start.

## Platforms

- **Apple Virtualization.framework backend** (`internal/vm/vz`) is scaffolding only.
- **Catalog checksums** are still a work in progress; `scripts/verify-catalog.sh` checks them.
