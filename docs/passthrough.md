# USB and PCI passthrough

A QEMU VM can take host USB and PCI devices at launch:

```
anvil host devices                       # what the host has, and what is free
anvil launch ubuntu --kind vm --device usb:046d:c52b --device pci:0000:04:00.0
```

The same list is on the TUI's Devices screen (alt+9), and the launch form offers every
free device as a toggle for a qemu VM. An instance's devices show in `anvil info` and on
its detail page. In `anvil.yaml` a VM member takes `devices: [usb:3-2]`.

## Device ids

| Form | Example | Notes |
| --- | --- | --- |
| `usb:<vendor>:<product>` | `usb:046d:c52b` | survives replugging into another port; refused when two plugged-in devices match |
| `usb:<bus>-<port>[.<port>...]` | `usb:3-2.1` | a physical port, whatever is plugged into it |
| `pci:<domain>:<bus>:<slot>.<fn>` | `pci:0000:04:00.0` | needs the IOMMU on (`intel_iommu=on` or `amd_iommu=on`) |

A PCI device always brings the other devices of its IOMMU group with it (bridges stay on
the host): `anvil host devices` lists them next to the device.

## Rules

- QEMU only. Firecracker VMs and containers refuse `--device`.
- Devices are set at launch and don't change afterwards.
- A device is held by one running VM at a time. Two stopped VMs may list the same one;
  the second to start fails while the first runs.
- A start fails if a device is not on the host.
- Migration and fork refuse a VM with devices. Snapshots work as usual. Export keeps the
  device list, so an imported VM fails to start where a device is missing.
- Devices behind the root filesystem, the default route or the host display are blocked,
  and so is anything in the same IOMMU group as one of them.

## How it works

anvild runs as the unprivileged `anvil` user, so a small root helper does the privileged
part. `anvil-devhelper.socket` listens on `/run/anvil/devhelper.sock` (only the `anvil`
user can connect) and starts `anvild devhelper` as root for each request.

At VM start, for each device:

- **USB**: the helper hands `/dev/bus/usb/<bus>/<dev>` to the `anvil` user, and QEMU gets
  a `usb-host` device on a `qemu-xhci` controller.
- **PCI**: the helper binds every device of the IOMMU group to `vfio-pci` (loading the
  module if needed), remembering each one's driver, and hands `/dev/vfio/<group>` to the
  `anvil` user. Each device gets its own `vfio-pci` on a PCIe root port.

The claims are written to the VM's `runtime.json` before QEMU starts. When the VM stops
or exits, or anvild finds after a restart that its QEMU is gone, the helper gives the
devices back: device files return to root and the udev rules, and PCI devices go back
to the driver they had.

VFIO pins all guest RAM, so `anvild.service` sets `LimitMEMLOCK=infinity`. Memory added
live through virtio-mem works with VFIO. The memory balloon stays, but QEMU stops it from
returning memory to the host for these VMs.

## Host requirements

- `usb-host` in QEMU (Arch: `qemu-hw-usb-host`).
- For PCI, the IOMMU on in the kernel command line and `vfio-pci` available.
- `hwdata` (or `pci.ids`/`usb.ids`) for device names; without it only ids are shown.
