# Anvil: status and future work

## Done

QEMU VMs over QMP, Firecracker microVMs, Docker containers over the real HTTP API, intents
with a shared bridge network and per-intent DNS, `anvil apply`, SSH-driven migration,
export/import bundles, snapshots, fork, port forwards, virtiofs mounts, cloud-init library
and repo import, VM and container mirrors, man pages, shell completions and the TUI.

Ephemeral VMs and matrix runs (`launch --ephemeral`, `anvil matrix`, `anvil warm`,
`anvil ephemeral`): warm Firecracker snapshots, per-clone netns with NAT, `anvil-guest` over
vsock, clone pool, netns pool, dm-snapshot disks, admission control, free page reporting,
VMGenID and the opt-in UFFD memory backend. Docs: `docs/ephemeral.md`, `docs/firecracker.md`.
Not planned: QEMU ephemeral runs, user cloud-init for ephemeral runs.

CI/CD on ephemeral clones: `--collect`, warm snapshots with a setup script, `anvil warm save`,
`anvil runner` for Forgejo and GitHub Actions jobs, and `anvil pipeline` with history,
branch watches and apply on success. Docs: `docs/runner.md`, `docs/pipelines.md`.

## Open

### Podman backend

Docker is the only engine; `--engine podman` returns a "not implemented yet" error.

- Podman engine support in anvild.
- Cross-kind networking (VM and container on one network) for Podman/netavark; Docker has it.
- Mixing engines in one intent: not allowed until there's a real reason.

### GitOps sync

Builds on `anvil apply` (`internal/intent/apply`): anvild keeps the host in line with every
`anvil.yaml` in a git repo, recursively.

- **Source.** `anvil gitops add <name> --repo <url> [--branch main] [--path infra/]`, stored
  in bbolt. Shells out to local `git`, so existing SSH keys or credential helpers work.
- **Watch.** Poll the remote (default 1m), apply only when the commit changes. No webhooks.
- **Reconcile.** Same compare-and-apply as `anvil apply`, one pass over the whole repo.
- **Strict coherence.** Synced instances and intents are tagged with their source; tagged ones
  gone from the repo are deleted, manual changes to them are reverted. Untagged are untouched.
- **Safety.** A parse or apply failure keeps the current state and records the error.
  `anvil gitops sync --dry-run` shows the plan; `anvil gitops pause <name>` stops reconciling.
- **Surface.** gRPC plus `anvil gitops add/list/remove/status/sync/pause/resume`, and a TUI
  screen with last synced commit, drift and last error.
- **Tests.** Reconcile diff, pruning and drift revert against a local bare repo.

### CI/CD

Built: `--collect`, setup-script warm snapshots, `warm save`, `@<key>` runs, `anvil runner`
(Forgejo 15+, GitHub) and `anvil pipeline` (run, history, watches, apply on success).
Docs: `docs/ephemeral.md`, `docs/runner.md`, `docs/pipelines.md`. Still open:

- **Real-run checks.** Only fakes cover these: `warm save` on a jailed clone (a dm-snapshot
  disk is read from its device in the jail), the forge runners against a real Forgejo 15 and
  GitHub, and the runner install scripts on every catalog distro.
- **Runner.** GitHub org scopes (no queued-job listing per org), runner groups other than
  the default, container jobs (`docker://` labels) inside clones.
- **Pipelines.** Passing collected files from a job to the jobs that need it, secrets for
  jobs, webhooks instead of polling, reporting commit status back to the forge.
- **Saved clones with UFFD memory.**

### Ephemeral VMs

- **Real-install checks.** Need root, so not covered by tests: the jailed path (`--netns`,
  links into the jail), the dm-snapshot disk in a jail (`TestCloneDiskReal`), and UFFD with a
  jailed firecracker (`/dev/userfaultfd` in the jail, `TestEphemeralRealRunUffd` needs
  `vm.unprivileged_userfaultfd=1`).
- **Inbound networking to clones.** Clones are outbound only. A forwarded port needs a free
  host port, a host DNAT to the clone's veth end and a DNAT inside the netns to `10.200.0.2`,
  both removed with the clone. Surface: `--publish <host>:<guest>` on `launch --ephemeral`
  and kept clones, the mapping in `anvil ephemeral list`, `Run`/`CloneExec` gRPC fields, the
  TUI Ephemeral screen. Test the nft rules under `unshare -rn`.

### Migration

- **Live migration.** QMP `migrate` through an SSH tunnel, a short pause instead of stop,
  copy and start.

### Platforms

- **Apple Virtualization.framework backend** (`internal/vm/vz`) is scaffolding only.
- **Catalog checksums** are a work in progress; `scripts/verify-catalog.sh` checks them.

### Known limits, left on purpose

- Several clients can call one anvild at once (`docs/remote.md`). An `apply` recreate
  deletes then relaunches the same name; a manual launch of that name in between wins.
