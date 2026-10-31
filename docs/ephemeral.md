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

The upload goes while the clones restore, not before: anvild spools it to a file and each
clone waits for it only right before its command, so a large upload over `--remote` and the
restore overlap instead of adding up. It goes in full 256 KiB chunks, not one message per
file. When the command ends, the result (output, exit code) comes back at once; the clone
is deleted right after, in the background, and still counts for memory until it is gone.

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
into its jail and gets its own copy-on-write disk (see [Clone disks](#clone-disks)). Warm
snapshots are never exported or migrated.

## Clone pools

Even a fast restore spends a few hundred milliseconds starting firecracker and loading
the snapshot. For runs that need to start in milliseconds, such as agents that open many
short sandboxes, a pool keeps clones of one warm snapshot restored and running ahead of
time:

```
anvil warm pool set ubuntu-24.04 --size 4               # 2 CPUs, 512 MiB
anvil warm pool set alpine-3.22 --size 8 --memory 256
anvil warm pool list
anvil warm pool rm alpine-3.22 --memory 256              # or set --size 0
```

A run on a distro and size with a pool takes a ready clone instead of restoring one, shown
as "took a pooled clone in 2ms". Before handing it out, anvild sets the clone's clock
through `anvil-guest`, which also checks it still answers; one that doesn't is deleted and
the next one is tried. A pooled clone keeps the id it was restored with (`eph-P...`).
After each clone taken, the pool restores a new one in the background. When the pool is
empty, as when a matrix takes more clones than it holds, the rest restore as usual.

- **Stored by distro and size, not by key.** A pool is for a distro, arch, CPU count and
  memory size, kept in anvild's database. When an upgrade (firecracker, the guest kernel,
  `anvil-guest`) changes the warm key, the pool builds the new snapshot, deletes the clones
  of the old one and fills again. `anvil warm pool set` builds the warm snapshot itself
  when there is none yet.
- **Filled at start.** Pooled clones live only in anvild's memory, like every clone. When
  anvild starts it deletes the old ones and fills every pool again in the background.
- **Memory.** Pooled clones go through the same memory check as any other clone. They are
  idle, so a run that waits for memory deletes pooled clones first, of any pool, and their
  pools refill 30 seconds later. A refill that fails (no memory, a build error) is shown in
  `anvil warm pool list` and tried again every 30 seconds.
- **Removing.** `anvil warm rm` of a snapshot with a pool also removes the pool and its
  clones, so the snapshot isn't built again behind the user's back.
- Warm snapshots with an app (`anvil gui --ephemeral`) have no pool.

`anvil warm list` shows each snapshot's pool in the POOL column, as `<ready>/<size>`.

## Clone memory: shared file or UFFD

By default a clone maps the warm snapshot's memory file private (firecracker's `File`
backend). Pages load lazily from the host's page cache, and every clone of one snapshot
shares the pages it doesn't change. This keeps clones dense: many clones of one snapshot
cost little more memory than one.

`ANVIL_EPH_MEMORY=uffd` in anvild's environment switches clones to firecracker's `Uffd`
backend instead. Firecracker maps the guest memory anonymously and asks a page handler in
anvild for each page the guest first touches, through Linux's userfaultfd; the handler
copies the page in from the memory file. On its own this is slower than the file mapping,
so it comes with a prefetch:

1. The first clone of a warm snapshot records which pages it touches in its first 30
   seconds, and saves that list (its working set) as `workingset` in the warm dir.
2. Every later clone gets the whole working set copied in, in large contiguous runs,
   right after the snapshot loads, instead of one fault per page while the command runs.
   The restore timings show it as `prefetch <n> pages`. Pages outside the set are still
   served on demand.

The trade-off: copied pages belong to one clone, so clones of one snapshot no longer share
memory. Use it for large guests where the first steps of a command dominate, keep the
default for many small clones. Pages the guest frees go back to the host the same way
(the balloon's removals unregister them from the handler). A rebuilt warm snapshot records
a new working set. Firecracker needs a userfaultfd: with the jailer, `/dev/userfaultfd`
in the jail, otherwise `vm.unprivileged_userfaultfd=1`.

## Clone disks

Every clone writes to its own disk, while the warm disk stays as it was for the next
clone. A plain copy of a 10 GiB sparse disk with a gigabyte or two in use takes seconds,
longer than the rest of the restore together, so anvild picks the cheapest way the host
allows, in this order:

1. **Reflink** (btrfs, xfs, bcachefs): the clone's disk is a copy-on-write clone of the
   file, made in a single call whatever the disk's size. Nothing to set up.
2. **dm-snapshot** (ext4 and other filesystems without reflinks): the root helper builds a
   device-mapper snapshot. The warm disk is its origin, behind a loop device, and is never
   written; a sparse file the size of the disk, in the clone's instance dir, holds the
   blocks the clone changes. The helper puts the device in the clone's jail as `disk.raw`,
   so firecracker opens it like the file it replaces. The setup takes the same time for any
   disk size, and only changed blocks take space. It needs `dmsetup` (the `device-mapper`
   or `dmsetup` package) and the kernel's `dm_snapshot` module, which the helper loads.
   Deleting the clone removes the device and its loop devices.
3. **Full copy**: when neither works, as with no `dmsetup`, the disk is copied as before.
   The first time the dm-snapshot fails, anvild logs why and copies for every clone after
   that until it restarts.

The restore timings show which one a clone got, as `disk reflink`, `disk dm-snapshot` or
`disk copy`.

## Memory and restore timings

Each clone needs its guest memory on the host. Before a clone restores, anvild checks the
host's available memory (`MemAvailable` in `/proc/meminfo`) and starts the clone only when
it covers the clone's memory plus a reserve kept for the host, 1024 MiB by default
(`ANVIL_EPH_RESERVE_MIB` in anvild's environment changes it, `0` turns the reserve off).
Clones still restoring count too, since their memory doesn't show as used yet. A clone that
doesn't fit waits, shown as a "waiting for memory" step, until other clones end. When
nothing else runs and it still doesn't fit, the run fails with how much memory is missing
instead of waiting forever. This is what keeps a large matrix from starting every clone at
once and pushing the host into swap or the OOM killer.

Once a clone is up, its run shows how long the restore took and where the time went, also
written to anvild's log:

```
[ubuntu-24.04] restored in 162ms (network 1ms, disk reflink 2ms, spawn 15ms, load 90ms, setup 47ms)
```

- **network**: taking a network namespace from the pool, or building one through the root
  helper when the pool is empty ([networking.md](networking.md#ephemeral-clones)).
- **disk**: the clone's copy-on-write disk, named after how it was made (see
  [Clone disks](#clone-disks)).
- **spawn**: starting firecracker until its API answers.
- **load**: loading the warm snapshot into it.
- **prefetch**: with `ANVIL_EPH_MEMORY=uffd`, copying the warm snapshot's working set in
  (see [Clone memory](#clone-memory-shared-file-or-uffd)).
- **setup**: setting the guest's clock and hostname through `anvil-guest`.

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
builds a warm snapshot, `p` sets the pool size of the selected one, `n` starts a run with one pane per clone, `a` starts a GUI app in
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
  its pages. Firecracker writes a new VMGenID on every restore, so each clone's kernel
  reseeds its RNG and clones don't make the same random numbers. Only the kernel's RNG:
  user-space state (`boot_id`, `machine-id`, SSH host keys) is the same in every clone.
- **Free memory goes back.** Warm VMs have an empty virtio-balloon with free page
  reporting on. It never inflates; it only tells firecracker which pages the guest freed,
  and firecracker drops them. Without it a clone holds every page it ever touched, a
  large build's page cache included, until it is deleted; with it, a long-lived clone
  shrinks back once the guest frees memory. It is part of the warm key, so warm snapshots
  from before it are built again once. The first thing a clone does is set its clock and hostname (its clone ID)
  through `anvil-guest`: the guest clock is still at the snapshot's time, which
  `clock_realtime` doesn't fix on the `tsc` clocksource.
- **Commands.** `anvil-guest` runs one command per vsock connection, as root, with the
  upload, stdio, an optional terminal and the exit code (`internal/guestexec`).

GUI apps in clones (`anvil gui --ephemeral`) are covered in [gui.md](gui.md#throwaway-apps).

Not done yet: ephemeral runs under QEMU, inbound networking to clones, user cloud-init for
ephemeral runs.
