# GUI apps from instances (implementation plan)

Status: in progress. This file becomes the user guide once the feature is done.

`anvil gui <name> -- <app>` opens a graphical app running inside a VM or container as a
native window on the host's Wayland compositor. Linux hosts and Wayland only.

## Decisions

| Topic | Decision |
|---|---|
| VM transport | waypipe over the SSH path `anvil exec` already uses (works with `--remote`). |
| Container transport | Per-instance relay directory bind-mounted at create, plus a local fd-passing relay run by `anvil gui`. |
| X11 apps | Supported through XWayland, only with `--x11` (off by default). |
| Default | Every instance created from this version on supports GUI apps. Older ones get a clear error. |
| Exposure | Wayland/X11 sockets exist inside an instance only while `anvil gui` runs. |
| GPU | Containers get `/dev/dri` (plus the render/video groups) at create when the host has it. Permanent, unlike the sockets. |
| VM tools | Static `waypipe` and `xwayland-satellite` shipped with anvil, shared read-only into VMs over virtiofs. |
| Xwayland in VMs | Installed through the guest agent on first `--x11` use. |
| Stopped instance | `anvil gui` starts it (and waits for a VM to be ready) first. |
| Desktop entries | `anvil gui export` writes a `.desktop` file, with the app's icon copied from the instance. |
| `--remote` + container | Not supported in the first version. |

## Shape

```
anvil gui <name> -- ghidraRun
  |
  +- gRPC InstanceService.PrepareGUI -> anvild: checks, starts, prepares, replies with session details
  |
  +- container: local relay  /run/anvil/gui/<id>/{wayland-0, x11/X0}  <->  real host sockets
  |             then `docker exec -e WAYLAND_DISPLAY=... <cid> ghidraRun`
  |
  +- VM:        waypipe [--xwls] ssh <existing ssh args/jump> user@vm ghidraRun
```

The daemon decides and prepares (directories, tools share, Xwayland install, start). The
CLI only does what must run inside the desktop session: the socket relay and `waypipe`.

## Phase 0: check the risky parts

1. Static `waypipe` (0.10+, lz4/zstd, no dmabuf/video) and `xwayland-satellite` built with musl.
2. waypipe `--xwls` flag in the pinned version, and the guest finding `xwayland-satellite` over non-interactive SSH.
3. Xlib/xcb in a container only looks at `/tmp/.X11-unix/X<n>`: a second bind over that directory.
4. Xauthority cookie needs (GNOME, KDE) and `FamilyWild` acceptance.

## Phase 1: API, domain, store

- `anvil.proto`: `Instance.gui_enabled`; `rpc PrepareGUI(PrepareGUIRequest) returns (stream PrepareGUIProgress)`
  (request: `name`, `x11`; last message: session details); `rpc GetGUIAppInfo`;
  `LaunchRequest.gui_enabled` (internal, keeps the flag across migrate/import).
- `instance.Spec.GUI`; `GUIPreparer` and `GUIAppInspector` capability interfaces.
- `Manager`: sets `GUI` on new instances, Fork inherits it, `PrepareGUI` starts a stopped
  instance (and waits for a VM), rejects `GUI == false`.
- `config.GUIRelayDir(id)` = `/run/anvil/gui/<id>`, `config.GUIToolsDir()` = `/usr/lib/anvil/gui`
  (`ANVIL_GUI_TOOLS` overrides it).

## Phase 2: container backend

- Create with `GUI`: bind `GUIRelayDir(id)` to `/run/anvil-gui` and `GUIRelayDir(id)/x11`
  to `/tmp/.X11-unix`; `/dev/dri` plus render/video group ids when present. Recreate keeps them.
- Start makes both directories (0777, under the 0750 `/run/anvil`) before `docker start`,
  since `/run` is empty after a reboot. Delete removes them.
- `PrepareGUI` returns the paths and the container ID.

## Phase 3: relay (`internal/gui/relay`)

- `Serve`: unix listener, one upstream dial per connection, copies bytes and `SCM_RIGHTS`
  fds together in one `sendmsg` per chunk. Sockets 0777, removed on exit.
- `xauth`: reads the cookie for `:n` from `$XAUTHORITY`, writes it as `FamilyWild`.
- `display`: finds the host Wayland socket and X display number.

## Phase 4: `anvil gui` for containers

`anvil gui <name> [--x11] [--detach] [--user U] -- <cmd>`: PrepareGUI, start relays,
`docker exec` with `WAYLAND_DISPLAY`, `XDG_RUNTIME_DIR` (and `DISPLAY`, `XAUTHORITY` with
`--x11`). `--detach` re-runs itself in a new session and logs to
`$XDG_STATE_HOME/anvil/gui/<name>.log`.

## Phase 5: VM backend

- `qemu.Config.ToolsShare`: a `vhost-user-fs-pci` with tag `anviltools` on `pcie.0`, so it
  doesn't use one of the 8 hot-plug ports.
- Start runs a read-only virtiofsd for `GUIToolsDir()` when `GUI` is set and the dir exists.
- Seed: fstab `anviltools /opt/anvil virtiofs ro,nofail 0 0`, symlinks into `/usr/local/bin`.
- `PrepareGUI` (guest agent): mounts the share if missing, installs Xwayland for `--x11`
  (package per distro, overridable by a catalog `xwayland_package` field).

## Phase 6: `anvil gui` for VMs

`waypipe [--xwls] ssh <commonSSHArgs> user@host <cmd>`, preferring the shipped host
`waypipe` so both ends match.

## Phase 7: desktop entries

`anvil gui export|unexport|exports`. `GetGUIAppInfo` (daemon side, VM through the guest
agent, container through Docker's archive API) finds the app's `.desktop` file and icon.
Icon saved under `~/.local/share/icons/hicolor/<size>/apps/`, generic fallback, `--icon` wins.

## Phase 8: TUI

Key `g` on the instances screen: command plus X11 toggle, starts `anvil gui --detach`.
Info panel shows GUI support.

## Phase 9: packaging, docs

`scripts/build-gui-tools.sh` + `make gui-tools`, binaries in `/usr/lib/anvil/gui/` for
every package format, README, CLAUDE.md, `make man`.

## Tests

relay fd passing (inode check), xauth round trip, qemu args, seed merge, docker params,
manager dispatch/start/flag, xwayland package mapping, `.desktop` matching and icon lookup,
waypipe argv builder, `.desktop` rendering, plus integration tests that skip themselves
without a compositor, docker or waypipe.
