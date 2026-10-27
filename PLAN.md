# Anvil: status and future work

## Current status

Working today: QEMU VMs over QMP, Firecracker microVMs, Docker containers over the real HTTP API, intents with
a shared bridge network and per-intent DNS, SSH-driven migration, export/import bundles,
snapshots, fork, port forwards, virtiofs mounts, cloud-init library and repo import, VM and
container mirrors, man pages, shell completions and the TUI.

## Device passthrough

Implemented (`docs/passthrough.md`), but not yet tested on real hardware.

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
