# Ephemeral runs and matrix runs

`anvil launch --ephemeral` runs one command on a throwaway clone of a distro and throws
the clone away after. `anvil matrix` does the same on several distros at once:

```
anvil launch --ephemeral ubuntu-24.04 . -- make test
anvil matrix ubuntu-24.04,debian-13,alpine-3.22 . -- ./ci/check.sh
```

The optional path before `--` is uploaded into `/work` in every clone, where the command
starts. In a git repo only what git doesn't ignore goes (`git ls-files --cached --others
--exclude-standard`), everything otherwise. After `--` everything belongs to the command,
so anvil's own flags (`-e`, `--keep-failed`...) come before the distro.

A clone starts from a warm snapshot: a VM of that distro, booted once, with cloud-init
done, paused and saved. Restoring one takes a few hundred milliseconds, so a warm run is
about as fast as `docker run`. The first run of a distro and size builds its warm
snapshot first, which takes as long as a first boot (seconds to a minute).

## Requirements

- Firecracker with its jailer, in use by anvild (see [firecracker.md](firecracker.md)).
  There is no fallback: without the jailer a run fails with a clear error.
- The root helper `anvil-devhelper.socket` (shipped and enabled by the packages), which
  builds each clone's network namespace.
- `anvil-guest`, shipped by the anvild package in `/usr/lib/anvil/guest`.

## Output

Each line of output carries its distro, in its own color on a terminal, and lines of
different clones never mix. On a terminal, the bottom lines show each clone's current
step (building its warm snapshot, waiting for cloud-init, restoring, running) with a
spinner and the time spent on it, redrawn in place while the output scrolls above. A step
that took more than a second stays above once it is done, with its time. Off a terminal
every step is printed on its own line instead. A summary follows on stderr:

```
[ubuntu-24.04] ok
[debian-13   ] FAIL: test_net
DISTRO        EXIT  DURATION  NOTE
ubuntu-24.04  0     2.31s
debian-13     1     2.40s
```

- `--logs <dir>` also writes each distro's full output to `<dir>/<distro>.log`.
- `launch --ephemeral` exits with the command's exit code; `matrix` exits 1 when any
  clone failed or never ran.
- `launch --ephemeral -t` runs the command on a terminal with stdin, for an interactive
  shell: `anvil launch --ephemeral -t debian-13 -- bash`. Without `-t` a piped stdin is
  passed on: `echo hi | anvil launch --ephemeral alpine-3.22 -- cat`.
- `-e KEY=value` sets environment variables for the command.
- Ctrl-C (or a dropped connection) stops the run and deletes its clones.

## Clone size and warm snapshots

`--cpus` (default 2) and `--memory` (default 512 MiB) size the clones. They are part of
the warm snapshot's key, so each size has its own snapshot. So are the image, the
generated cloud-init, the Firecracker version, the guest kernel and `anvil-guest`: an
upgrade of any of them builds a new snapshot on the next run.

```
anvil warm create ubuntu-24.04 --memory 1024   # build ahead of time
anvil warm list
anvil warm rm d1791f77                          # a key, or a unique start of one
anvil warm rm --all
```

A warm snapshot takes the guest memory size plus the disk's used space. Its files are in
`/var/lib/anvil/warm/<key>`, on the jailer's filesystem: every clone links the memory file
into its jail and gets a copy-on-write copy of the disk (a full copy without reflink
support, on ext4 for example). Warm snapshots are never exported or migrated.

## Failed clones

`--keep-failed` keeps a clone whose command failed, still running, for a look inside:

```
anvil matrix --keep-failed ubuntu-24.04,debian-13 . -- make test
anvil ephemeral list
anvil ephemeral shell eph-01M66  # a root shell; `-- <command>` runs one command
anvil ephemeral rm eph-01M66     # or --all
```

Clones live only in anvild's memory: when anvild restarts, it deletes every clone, kept
ones included.

## Networking

Clones reach outside through NAT (the LAN included), and nothing reaches them. They can't
reach each other, the host, or other VMs and containers. Each runs in its own network namespace with the
same layout inside; [networking.md](networking.md#ephemeral-clones) has the details.

## TUI

The Ephemeral screen lists warm snapshots and clones (`tab` switches between them). `c`
builds a warm snapshot, `n` starts a run with one pane per clone, `a` starts a GUI app in
a throwaway clone ([gui.md](gui.md#throwaway-apps)), `s` opens a shell in a clone and `x`
removes the selected one. Each pane shows the clone's current step with a spinner.

## How it works

- **Build.** A Firecracker VM boots in the clone network layout with `anvil-guest` on a
  read-only drive. Once cloud-init is done and `anvil-guest` answers, the guest syncs its
  disk, the VM is paused and snapshotted (`snapshot_type: Full`), and the snapshot, a copy
  of the disk and its drives are kept. Memory hotplug is off for these VMs: the memory
  file would cover the whole hotplug region.
- **Restore.** A new firecracker starts in the clone's jail and netns with no config, then
  loads the snapshot with its memory file mapped private, so clones of one snapshot share
  its pages. The first thing a clone does is set its clock and hostname (its clone ID)
  through `anvil-guest`: the guest clock is still at the snapshot's time, which
  `clock_realtime` doesn't fix on the `tsc` clocksource.
- **Commands.** `anvil-guest` runs one command per vsock connection, as root, with the
  upload, stdio, an optional terminal and the exit code (`internal/guestexec`).

GUI apps in clones (`anvil gui --ephemeral`) are covered in [gui.md](gui.md#throwaway-apps).

Not done yet: ephemeral runs under QEMU, inbound networking to clones, a limit on how many
clones start at once (a large matrix starts them all), user cloud-init for ephemeral runs.
