# Remote access: concurrency and dropped connections

How `anvil --remote <[user@]host>` behaves when several clients share one remote `anvild`,
and what happens when the link between the local and the remote host breaks. Migration
details live in `docs/migration.md`; this file only covers the remote and failure angle.

## How a remote client connects

`DialSSH` (`pkg/client/ssh.go`) runs `ssh -T -o BatchMode=yes <host> anvil dial-stdio`
and speaks gRPC over that process's stdin/stdout. On the remote host, `dial-stdio`
(`internal/cli/commands/dial_stdio.go`) opens the local unix socket and copies bytes in
both directions. To the remote `anvild`, a remote client is just one more local socket
connection, made by the SSH login user.

```
anvil (host A) ── ssh ──▶ anvil dial-stdio (host B) ── unix socket ──▶ anvild (host B)
```

### One SSH connection per remote host

An SSH login costs several network round trips (TCP, key exchange, authentication), from
about a hundred milliseconds on a LAN to a second over a slow link, and every `anvil
--remote` command used to pay it. So anvil's `ssh` calls share one connection per remote
host, with OpenSSH's own connection sharing: `ControlMaster=auto`, `ControlPersist=60s`
and a control socket `anvil-ssh-<hash>` in `$XDG_RUNTIME_DIR` (or in a private
`anvil-ssh-<uid>` dir under the temp dir; with neither, sharing is off). The first command
opens the connection, every command in the next 60 seconds reuses it, and it closes a
minute after the last one. Against a local `sshd` a remote `anvil list` went from about
120 ms to 14 ms; on a real network the gain per command is the whole login.

A VM shell, exec or transfer reaches the guest through the same connection: instead of
`ssh -J <host>`, which would open its own, the hop is a `ProxyCommand` running
`ssh -W %h:%p <host>` with the same sharing options.

These options are passed on the command line, so for anvil's calls they win over
`ControlMaster`/`ControlPath`/`ControlPersist` in `~/.ssh/config`. `ANVIL_SSH_MUX=off`
turns the sharing off: every call opens its own connection, and the guest hop goes back
to `-J`.

## Two clients at once

Each `anvil --remote` call (or TUI session) starts its own `ssh` process and its own
`dial-stdio`, so the daemon sees two independent socket connections. A local client on
host B is a third one. The gRPC server handles every request on its own goroutine, so
nothing is queued per client and nothing is exclusive: both clients see and can act on
the same instances.

What is and is not protected against concurrent use:

| Area | Behaviour |
|---|---|
| Reads (`list`, `info`, `stats`, `logs`) | Safe. bbolt serializes writers and gives readers a consistent view. |
| Intent membership (`Launch`/`Remove`/`Delete` of an intent member) | Serialized by `intent.Manager.mu` (`internal/intent/manager.go`). |
| Store writes | Each `PutInstance` is one bbolt transaction, so a single write is never torn. |
| Launch with the same name | Protected. Launch, Fork and import reserve the name up front; the second caller fails with "name already in use". |
| Start/stop/delete/mount/port/snapshot/update of the same instance | Protected by a per-instance lock in `instance.Manager`. A second caller fails fast with "instance busy" rather than waiting behind a slow operation. Writes patch the stored record and never recreate a deleted one. |
| Migrating, exporting or applying while another client acts on the same instances | Protected. `migrate`, `export` and `apply` hold their instances for the whole run; other clients get "instance busy" until it ends. |

The event log records which user did what (`ActorInterceptors` in
`internal/daemon/events.go`), so concurrent actions can be told apart afterwards, but it
does not prevent them.

In practice: concurrent clients are fine for different instances, and for reads. On the
same instance, one write runs at a time and the others get "instance busy".

## The connection drops

A gRPC call's context on the daemon is tied to its connection. Long operations
(`Launch`, `Migrate`, stop/start, uploads) receive that context directly from the stream
(`internal/daemon/server.go`, `migrate_server.go`), and the SSH and `qemu-img` children
they spawn use `exec.CommandContext`. When the daemon notices the client is gone, the
context is canceled and the operation is aborted where it stands. It does **not** keep
running in the background, and there is no reattach.

### When the daemon notices

- **Clean break** (the local `ssh` is killed, the laptop's `anvil` exits, `ssh` sees a
  reset): sshd closes `dial-stdio`'s stdin, `dial-stdio` half-closes the socket, the
  gRPC server sees EOF and cancels the call within moments.
- **Silent break** (Wi-Fi gone, NAT entry expired, cable pulled): neither `ssh` nor gRPC
  sets a keepalive (`DialSSH` passes no `ServerAliveInterval`, `grpc.NewServer` in
  `cmd/anvild/main.go` sets no keepalive). The remote `sshd` may not notice for a long
  time (TCP retransmit timeout, or the kernel TCP keepalive, about two hours by default).
  Until then the operation **keeps running** on the remote daemon. It may even finish;
  the result is simply never delivered.

So a dropped remote call can end in either state, and the client cannot tell which.
After reconnecting, check with `anvil --remote <host> list` / `info` before retrying.

### Plain operations

| Operation | If canceled mid-way |
|---|---|
| `launch` | `Create` aborts. If `Start` fails because of the cancellation, the rollback `b.Delete` runs with the same canceled context and can fail too, leaving disk/container leftovers with no store record (logged by the daemon). If the instance already reached `PutInstance`, it exists normally. |
| `stop` / `start` | The backend call is interrupted. The instance can end up in either state; the stored state is only updated on success. |
| `delete` | Instances are deleted side by side, up to 4 at a time; those done before the drop are gone, the rest remain. |
| `logs -f`, `stats` stream | Just stop. Nothing to clean up. |
| `shell` / `exec` / `transfer` | These do not go through the daemon (a hop through `<host>`, see above, or `DOCKER_HOST=ssh://<host>`), so the session ends like any dropped SSH session. |

### Migration from a remote daemon

There are two separate links in `anvil --remote B migrate x --to C`:

1. **A to B** (the client to the source daemon): carries only the progress stream.
2. **B to C** (the source daemon to the target over SSH): carries the data.

**Link A to B drops.** The migration context is `stream.Context()`, so once B notices, the
migration is canceled at whatever step it is in:

- Before the source is stopped: nothing happened.
- After the stop, during export or upload: the running `qemu-img` or `ssh` is killed.
  Staged files on C are removed, because `removeRemote` and the failed-upload cleanup use
  `context.WithoutCancel` (`internal/migrate/transfer.go`). The source stays **stopped**
  on B and has to be started by hand.
- During `anvil migrate-import` on C: the `ssh` to C is killed, so `migrate-import` loses
  its stdin/stdout and C's `Launch` may or may not complete. The source is kept (stopped),
  so the worst case is two copies, never zero.
- Intent migration: the rollback (`rollbackRemote`) and the source deletes use the same
  canceled context, so they fail immediately. Members already launched on C stay there,
  all members on B stay stopped.

With a silent break, B does not notice, so the migration usually **runs to completion**,
including deleting the source if `--copy` was not set. Only the progress output is lost.

**Link B to C drops.** This is the failure the migration code is built for:

- Disk uploads resume: up to 4 attempts, continuing from the remote file size, then a
  `sha256sum` check (`uploadDisk`).
- Folder uploads (`tar` over SSH) do not resume. The step fails and staged files are
  removed.
- A drop during `migrate-import` is ambiguous in the same way as above: the source is
  kept stopped, and C has to be checked with `anvil --remote C list` before retrying.

The full table of leftover state per step is in `docs/migration.md`, "Failure modes and
leftover state".

## Practical advice

- Run long remote operations from a stable link, or run them on the daemon's host itself
  (`ssh B anvil migrate ...` inside `tmux`), so a dropped laptop does not cancel them.
- Add `ServerAliveInterval 15` and `ServerAliveCountMax 3` for the host in `~/.ssh/config`.
  `DialSSH` uses the normal ssh config, so this turns a silent break into a clean one
  within about 45 seconds, and the daemon cancels instead of continuing blind.
- After any drop: `anvil --remote <host> list`, then start a stopped source or delete a
  half-made copy by hand.

## Known gaps

- No per-instance lock: concurrent writes to the same instance, or launches of the same
  name, are not serialized.
- No keepalive on the remote gRPC link, so a silent drop is detected late.
- Operations are bound to the client connection: no detach, no reattach, no resumable
  migration.
