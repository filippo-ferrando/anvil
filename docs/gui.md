# GUI apps from instances

`anvil gui <name> -- <app>` runs a graphical app inside a VM or container and shows its
windows on the host's Wayland desktop, like a native app. Linux hosts with a Wayland
session only.

```
anvil gui dev -- gnome-text-editor
anvil gui --x11 re-lab -- ghidraRun
anvil gui --detach dev -- firefox
```

## Requirements

- A Wayland desktop session on the host running `anvil gui`. With `--x11`, the desktop
  also needs XWayland.
- An instance created by an anvil version with GUI support. Older instances get a clear
  error: recreate them to use GUI apps. `anvil info` (`GUI` field) and the TUI detail
  panel show whether an instance supports it.
- VMs only: the guest agent (no `--no-guest-agent`), and the static `waypipe` and
  `xwayland-satellite` that the packages install in `/usr/lib/anvil/gui`.

A stopped instance is started first, and `anvil gui` waits for a VM's first boot to
finish before running the app.

## Flags

| Flag | Meaning |
|---|---|
| `--x11` | Also make X11 apps work, through XWayland. Needed for Java/Swing apps such as Ghidra. Off by default. |
| `--detach` | Return right away. Output goes to `$XDG_STATE_HOME/anvil/gui/<name>.log`. |
| `--user U` | User to run the app as (VM: SSH user, container: `docker exec --user`). |
| `-i, --identity` | Private key for SSH instead of anvil's managed key (VM only). |

Put `--` before the app's command when it has flags of its own.

## How it works

### Containers

Every GUI-enabled container gets a per-instance directory, `/run/anvil/gui/<id>` on the
host, bind-mounted at `/run/anvil-gui` (and its `x11` subdirectory over `/tmp/.X11-unix`).
When the host has `/dev/dri`, the container also gets the GPU and the matching render/video
groups at create time.

`anvil gui` starts a small relay in that directory that forwards the host's Wayland (and,
with `--x11`, X11) sockets, file descriptors included, then runs the app with
`docker exec` and the right `WAYLAND_DISPLAY`/`DISPLAY`/`XAUTHORITY`. The sockets exist
inside the container only while `anvil gui` runs, and the relay stays up as long as the
app still has windows open.

### VMs

The app runs under `waypipe` over the same SSH path that `anvil exec` uses, so it works
with `--remote` too. The host's `waypipe` and `xwayland-satellite` are shared read-only
into the VM over virtiofs and mounted at `/opt/anvil` by the guest agent on first use, so
both ends run the same version.

The first `--x11` run installs Xwayland in the guest through the guest agent (apt, dnf,
zypper, pacman or apk), which can take a few minutes. With `--x11`, the app also gets
`_JAVA_AWT_WM_NONREPARENTING=1`, so Java windows don't stay blank.

A VM started before the GUI tools were installed on the host needs a restart before
`anvil gui` works for it.

A Firecracker VM has no virtiofs: the tools dir is packed once into a read-only ISO
(`/var/cache/anvil/images/firecracker/gui-tools.iso`, rebuilt when the tools change),
attached as a drive at start and mounted by its label (`anviltools`).

## Desktop entries

`anvil gui export` adds an instance's app to the host's app launcher. The entry runs
`anvil gui --detach`, so clicking it starts the instance if needed and opens the app.

```
anvil gui export --x11 re-lab -- ghidraRun
anvil gui export --title Inkscape --icon ~/icons/inkscape.svg dev -- inkscape
anvil gui exports            # list them, -o json for scripts
anvil gui unexport re-lab ghidrarun
```

The app's name and icon come from its own `.desktop` file inside the instance (read
through the guest agent for a VM, Docker's archive API for a container). Without one, the
command name and a generic icon are used. `--title` and `--icon` override them.

Entries are written to `~/.local/share/applications/anvil-<instance>-<app>.desktop`,
icons to `~/.local/share/anvil/icons/`. An export made with `--remote` keeps it in the
entry.

## TUI

On the Instances screen, `g` opens the GUI app form: command, an "X11 app" toggle, and an
"Add to desktop" toggle that also runs `anvil gui export`. The app starts detached, so it
keeps running after the TUI quits. The detail panel shows whether the instance supports
GUI apps.

The **GUI Apps** screen lists the exported entries of the machine running the TUI (like
`anvil gui exports`): `enter` starts the app with the entry's own command line, `x`
removes it after a confirmation (like `anvil gui unexport`).

## Limitations

- Wayland hosts only, no plain X11 host session.
- `--remote` works for VMs, not yet for containers.
- The host GPU is passed through to containers only, VM apps get no GPU access.

## Building the tools

`make gui-tools` (`scripts/build-gui-tools.sh`) builds static musl `waypipe` and
`xwayland-satellite` in an Alpine container, into `build/gui-tools`. The Arch, deb and
rpm packages ship them in `/usr/lib/anvil/gui`. `ANVIL_GUI_TOOLS=<dir>` in anvild's (and
the CLI's) environment points at another directory, for development builds.
