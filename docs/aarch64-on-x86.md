# Running aarch64 VMs on an x86_64 host

Anvil picks the QEMU binary from the image's architecture, so an `aarch64` image runs
under `qemu-system-aarch64` even on an x86_64 host. Nothing has to be turned on, but
there are four things worth knowing before trying it.

## Dependencies

Both have to be on the host:

- `qemu-system-aarch64`
- `edk2-aarch64` (the UEFI firmware; packaged as AAVMF on Debian and Fedora)

Anvil looks for the firmware under `/usr/share` and matches on the arch, so any of the
usual packaging layouts works. A missing firmware package is reported as an error
naming the arch, not as a boot failure.

## It runs without KVM

KVM only applies when the guest arch matches the host's. Anvil checks this itself
(`internal/vm/backend.go`) and falls back to `-accel tcg` for a cross-arch guest.
Emulation is a lot slower than a native VM: expect a boot measured in minutes, not
seconds. This is a property of emulating a foreign CPU, not something anvil tunes.

## The built-in catalog has no aarch64 images

Every one of the 15 entries in the built-in catalog is x86_64, and `anvil launch` has
no `--arch` flag. The way to get an aarch64 image is to add a mirror whose manifest
carries `"arch": "aarch64"` on the entry:

```
anvil mirror add my-arm-images --kind vm \
  --manifest-url https://example.org/distribution-info.json
```

See [mirrors.md](mirrors.md) for the manifest schema. `anvil image checksum --arch` and
`anvil image delete --arch` take the arch when an image id exists for more than one.

## Resizing applies at the next start

`anvil set --cpus` and `--memory` are applied live over QMP where the machine type
supports CPU and memory hotplug. The `virt` machine type that aarch64 uses does not get
the same treatment as x86_64's `q35`, so a change is recorded and takes effect the next
time the VM starts.
