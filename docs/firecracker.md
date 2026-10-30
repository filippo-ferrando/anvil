# Firecracker VMs

`--hypervisor firecracker` runs a VM as a [Firecracker](https://firecracker-microvm.github.io/)
microVM instead of under QEMU. It boots faster and uses less memory, and anvil keeps the
commands, the TUI screens and intent membership the same as for a QEMU VM:

```
anvil launch ubuntu-24.04 --kind vm --hypervisor firecracker --wait
anvil shell ubuntu-24.04
```

In `anvil.yaml` the field is `hypervisor: firecracker`. `anvil list` shows the hypervisor in
the ENGINE column, `anvil info` on a `Hypervisor:` line, and `anvil find` marks the distros
that boot under Firecracker.

## Requirements

- `firecracker` and `jailer` 1.17 or newer, and `/dev/kvm`. The anvild packages ship static
  builds in `/usr/lib/anvil/firecracker` (`ANVIL_FIRECRACKER_DIR` overrides it), used before
  any on `PATH`. A dynamically linked firecracker (Arch's `extra/firecracker` is one) can't
  run in the jailer's chroot, so anvild runs it unjailed. `make firecracker` downloads the
  pinned static release into `build/firecracker` for development builds.
- `nft`, for the outbound NAT of standalone VMs. anvild turns on `net.ipv4.ip_forward` the
  first time a standalone Firecracker VM starts.
- `setcap` (libcap), for the jailer copy `anvild.service` prepares at
  `/var/lib/anvil-jailer/jailer` before each start. Without it, or without `jailer`, a
  non-root anvild runs firecracker unjailed.

## The guest kernel

Firecracker has no firmware and no bootloader, so it boots a kernel anvil provides rather
than the distro's own: Linux 6.18 built from Firecracker's guest config plus what cloud
images need (partition tables, btrfs, xfs, vfat, iso9660, NFS, vsock, virtio-mem),
everything built in. It downloads on the first Firecracker launch into
`/var/cache/anvil/images/firecracker` and is checked against the catalog's sha256.
`ANVIL_FC_KERNEL=<path>` in anvild's environment uses a local kernel instead.

Kernel updates inside the guest have no effect, since the guest never boots its own.

The kernel is built by `scripts/build-fc-kernel.sh` (`make fc-kernel`, `ARCH=aarch64` for
arm64) and published by `.github/workflows/fc-kernel.yml` on a `fc-kernel-<version>` tag.

## The catalog

The image catalog has a `firecracker` section: the kernel per arch, and the distro ids that
boot with it, with extra kernel arguments where needed (`rootflags=subvol=root` for
Fedora's btrfs root). Images, checksums and mirrors are the same as for QEMU. A mirror
manifest can carry its own `firecracker` section (`docs/mirrors.md`), and the catalogue
submodule has `firecracker_x86_64.json` and `firecracker_aarch64.json`.

## How it differs from a QEMU VM

| | QEMU | Firecracker |
|---|---|---|
| Disk | qcow2 overlay on the base | raw, a reflink copy of the base's raw copy |
| cloud-init seed | ISO on a CD-ROM | the same ISO on a second, read-only drive |
| Guest agent | virtio-serial | vsock (`qemu-ga --method=vsock-listen`, set up by cloud-init) |
| Standalone network | SLIRP, hostfwd | tap on `anvil-fc0`, NAT, a port proxy in anvild |
| `anvil mount` | virtiofs | NFSv3 served by anvild on the gateway |
| GUI tools | virtiofs share | read-only ISO drive |
| Snapshots | qcow2 internal | reflink copies of the disk |
| CPUs | live | at the next start (no vCPU hotplug) |
| Memory | live, virtio-mem | live, virtio-mem |
| Disk resize | live | live |
| Stop | agent, then ACPI | agent reboot, then Ctrl+Alt+Del (x86_64 only) |

Details are in `docs/networking.md` (standalone networking), `docs/migration.md` and
`docs/export-import.md` (how a raw disk travels), and `docs/security.md` (the jailer and the
NFS server).

## Notes

- On x86_64 a guest `poweroff` leaves the Firecracker process running, only a reboot ends
  it. cloud-init links `poweroff.target` to `reboot.target`, and the agent asks for a reboot
  on `anvil stop`. A guest without systemd (Alpine) still hangs on `poweroff`.
- On aarch64 Firecracker has no Ctrl+Alt+Del, so without the guest agent `anvil stop` is a
  hard power-off.
- A migrated or imported standalone VM keeps its address, which is configured inside its
  disk. When another Firecracker VM there holds it, or it's outside `ANVIL_FC_SUBNET`, the
  VM gets a new address and a new seed, as a fork does, so cloud-init runs again.
- `ANVIL_FC_SUBNET` changes the standalone subnet (default `10.254.254.0/24`, a /24).
- `ANVIL_FC_JAILER=off` runs firecracker without the jailer.
- `anvil image list` shows the raw copies and kernels; deleting a base image also deletes
  its raw copy.

## Tests

`TestFirecrackerRealBoot` (`internal/vm/fc_integration_test.go`) boots a real cloud image
through every feature above. It needs `ANVIL_E2E_IMAGE` (a qcow2 cloud image) and
`ANVIL_FC_KERNEL`, plus `CAP_NET_ADMIN`, so it is meant to run in its own network namespace:

```
go test -c -o /tmp/vm.test ./internal/vm/
cd internal/vm && ANVIL_FC_KERNEL=... ANVIL_E2E_IMAGE=... \
  unshare --user --map-root-user --net /tmp/vm.test -test.run TestFirecrackerRealBoot -test.v
```

The jailer can't run in a user namespace; as root, `ANVIL_FC_TEST_JAILER=1` runs the same
test jailed.

`TestFirecrackerSnapshotRestore` (same file, same setup) restores a full snapshot of a
booted VM in a second firecracker and runs a command in it through `anvil-guest`.
`TestFirecrackerAnvilGuest` runs commands, an upload and a terminal through `anvil-guest`.
Both build `anvil-guest` themselves. `TestSnapshotRestore` in `internal/vm/firecracker`
restores a snapshot with only `ANVIL_FC_KERNEL`.

## Ephemeral clones

`anvil launch --ephemeral` and `anvil matrix` run commands on clones restored from warm
Firecracker snapshots. They need the jailer; see [ephemeral.md](ephemeral.md).
`TestEphemeralRealRun` (`internal/vm/fc_ephemeral_integration_test.go`) builds a real warm
snapshot and runs clones of it, unjailed since it runs without root: it needs `unshare -rnm`
instead of `-rn`, for the clones' network namespaces.

## anvil-guest

Ephemeral clones run commands through `anvil-guest` (`cmd/anvil-guest`), a static binary
the anvild package ships in `/usr/lib/anvil/guest` (`ANVIL_GUEST_TOOLS` overrides it). A
VM built for a warm snapshot gets it on a read-only ISO drive (`guest.iso`, label
`ANVILGUEST`); cloud-init copies it to `/usr/local/bin` and starts it as a systemd or
OpenRC service on vsock port 1025. One connection runs one command as root, with an
optional upload extracted into `/work`, its stdio, an optional terminal and its exit code
(`internal/guestexec`). A command that fails to install shows on the serial console.

After a snapshot restore the guest clock is still at the snapshot's time, even with
Firecracker's `clock_realtime`: anvil's guests run on the `tsc` clocksource, which it
doesn't move. A clone sets its clock, and its hostname, through `anvil-guest` first.
