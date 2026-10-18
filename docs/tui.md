# The TUI

![anvil tui](demo/gifs/tui.gif)

`anvil tui` opens a full-screen terminal UI built with Bubble Tea (`internal/tui`). It is
a client like the CLI: every action is a gRPC call to anvild through the same
`pkg/client` wrapper, so the TUI and the CLI always see the same state and the same
errors. This guide lists every screen and every key, taken from the key handlers in
`internal/tui/*.go`.

```
anvil tui                          # the local daemon, on /run/anvil/anvild.sock
anvil tui --socket /path/to.sock   # another local socket
anvil --remote user@host tui       # the anvild on another host, over ssh
ANVIL_REMOTE=user@host anvil tui   # same, from the environment
```

`--remote` works for the TUI exactly as for the CLI (`tui.Run` calls `client.DialSSH`):
anvil runs `ssh <host> anvil dial-stdio` and speaks gRPC over that pipe. The pieces that
run local programs follow the remote too: a VM shell or exec goes through `ssh -J <host>`,
a container exec runs `docker exec` with `DOCKER_HOST=ssh://<host>`, and a mount's host
path must then be absolute on the remote host.

## Layout

```
 ⚒ anvil  user@laptop  /run/anvil/anvild.sock      ● daemon connected │ 2 of 5 running │ 14:03:12

  MENU            │  ┌──────────────────────┐  ┌──────────────────────────────┐
                  │  │ list                 │  │ detail / second panel        │
 ▸ 1 Instances    │  │                      │  │                              │
   2 Snapshots    │  └──────────────────────┘  └──────────────────────────────┘
   3 Images       │  help bar
   ...
 ✓ status line (transient)
```

- **Header**: local `user@hostname`, the socket (or `host:socket` with `--remote`), the
  daemon state (`connecting…`, `daemon connected`, `daemon unreachable`, taken from the
  last instance list request), the running/total instance count and a clock.
- **Sidebar**: the eight screens of `screenOrder` in `internal/tui/app.go`. The entry with
  the arrow is the active screen; it is highlighted when the sidebar itself has focus.
- **Status line**: the result of the last action. A success stays 4 s, an error 10 s; known
  errors get a `hint:` line under them (`friendlyError`).

The TUI subscribes to the daemon's `Watch` stream. Any instance change (a launch, a stop,
a VM that powered itself off, a guest agent report) reloads the Instances, Snapshots and
Intents and Events screens; bursts of events are merged into one reload after 200 ms. If the stream
breaks (for example the daemon restarted), it is retried every 5 s and the TUI keeps
working without it.

Two screens are full-screen takeovers without the sidebar: **Launch** (`n` on Instances)
and **Logs** (`l` on Instances).

## Keys that work everywhere

| Key | Action |
|-----|--------|
| `ctrl+c` | Quit, from any screen, even during a launch. |
| `alt+1` … `alt+8` | Jump to that sidebar entry, focused on its content. Not in the Launch and Logs takeovers. This is checked before any form, so it also leaves a half-filled form. |

### Sidebar (when it has focus)

| Key | Action |
|-----|--------|
| `↑` `k` `shift+tab` | Previous screen (wraps around), loaded at once. |
| `↓` `j` `tab` | Next screen (wraps around). |
| `home` `g` / `end` `G` | First / last screen. |
| `enter` `→` `l` | Move focus into the screen's content. |
| `1` … `8` | Jump to that screen, focused on its content. |
| `q` | Quit. |

Inside a screen, `esc` or `q` gives focus back to the sidebar when no form, confirmation
or progress view is open.

### Lists

Every list is a Bubbles `list` with filtering turned off, so single-letter shortcuts never
collide with filter typing. Keys a screen does not use itself reach the list:

| Key | Action |
|-----|--------|
| `↑` `k` / `↓` `j` | Move the selection. |
| `pgup` `←` / `pgdown` `→` | Previous / next page (the letter aliases `h`, `b`, `u`, `l`, `f`, `d` work only where the screen does not bind them). |
| `home` `g` / `end` `G` | First / last item. |

### Forms

All prompts (launch, settings, mount, port, host, mirror, ...) use the same form
(`simpleForm` in `internal/tui/form.go`):

| Key | Action |
|-----|--------|
| `tab` | Complete the focused field from its suggestions (cycling on repeated presses, matching case-insensitively), or from the local file system for a path field; with nothing to complete, move to the next field. |
| `↓` | Next field. |
| `shift+tab` `↑` | Previous field. |
| `space` | Flip a toggle field. |
| `enter` | Next field; on the last field, submit. |
| `ctrl+s` | Submit from any field. |
| `esc` | Cancel. |

### Confirmations

Destructive actions ask first. `y` or `enter` confirms; any other key cancels. The
instance and intent delete prompts have an extra `p` choice, described below.

## 1. Instances

The main screen: the instance list on the left, a detail panel for the selected instance
on the right.

Each list row shows a state marker, the name, the kind, the state, the image and an
address (a bridged VM's static IP, otherwise the first IP the guest agent reported).

The detail panel shows kind, state, image, creation time, size (vCPUs, memory, disk) for
a VM, the autostart flag and restart policy, the snapshot schedule, the intent and role,
port forwards and mounts. For a running instance it adds live data, streamed every 2 s
for the selected row only (`StreamStats`): address, uptime, the guest agent state, cloud-init state and
guest IPs (VM), and gauges for CPU, memory, disk usage (VM) or disk read/write rate
(container), and network rate (shown as "not available" for a SLIRP VM).
Under that, as many of the instance's recent events as fit, newest first, with the user
who caused each one and any error (see `anvil events`).

| Key | Action |
|-----|--------|
| `n` | Open the Launch takeover. |
| `s` | Stop the selected instance if it is running, otherwise start it. Stop uses the daemon's default 30 s graceful timeout. |
| `x` | Open a shell in a VM: asks for a user (blank means the image's default user), then suspends the TUI and runs `ssh` into the VM until the session ends. On a container it opens `/bin/sh` through `docker exec`, like `anvil shell`. |
| `e` | Run one command: asks for the command (and a user, for a VM), then runs it over `ssh` for a VM or `docker exec -it` for a container. |
| `l` | Open the Logs takeover. |
| `d` | Delete, after a confirmation: `y`/`enter` deletes, `p` also removes the base image if no other VM uses it, any other key cancels. |
| `f` | Fork the selected VM: new name, and whether to start it at once. Shows the progress until dismissed. |
| `u` | Settings: vCPUs, memory, disk (VM), autostart and restart policy, pre-filled with the current values. Only changed fields are sent (`anvil set`). |
| `w` | Wait for first-boot setup (cloud-init) of a running instance, streaming progress. `esc` stops waiting. |
| `m` | Mount a host directory into a VM: host path, guest path, read-only toggle. |
| `M` | Unmount from a VM; the guest path field suggests the current mounts. |
| `p` | Add a port forward: host port, guest port, UDP toggle. |
| `P` | Remove a port forward; the host port field suggests the current ones. |
| `E` | Export the selected instance to a bundle (output path). |
| `i` | Import a bundle (bundle path, optional rename). |
| `r` | Reload the list. |
| `esc` `q` | Back to the sidebar. |

While an export, import or fork runs, every key is ignored; when it ends, its transcript
stays on screen until `esc`, `enter` or `q`. The same keys dismiss a finished wait.

Either choice removes the instance and its disk. See [lifecycle.md](lifecycle.md#delete).

## Launch (takeover)

A form for a new instance. The subtitle shows the kind (`vm` or `container`), and the
fields depend on it:

- Both: Name (defaults to the image), Image (suggestions from the VM catalog and cached
  images, or from local container images), CPUs, Memory MiB.
- VM: Disk GiB (default 8), Cloud-init name (suggestions from the library), Guest agent
  toggle (on by default), Wait toggle.
- Container: Env (`K=V,K2=V2`), Volumes (`host:guest[:ro],...`).
- Both, at the end: Autostart toggle, Restart policy (suggestions `no`, `on-failure`,
  `on-failure:5`, `always`), Intent name, Role, Ports (`host:guest[/tcp|udp],...`).

For a container, the CPUs and Memory fields are shown but not sent with the request.

| Key | Action |
|-----|--------|
| `ctrl+k` | Switch between VM and container, keeping the values of the shared fields. |
| `ctrl+e` | VM only: open an ad hoc cloud-init editor, seeded from the saved config named in the form, or an empty `#cloud-config`. |
| form keys | As above; submitting starts the launch. `esc` returns to Instances. |

In the cloud-init editor, `ctrl+s` keeps the text for this launch only (it is not saved
to the library, and it takes priority over the name field), and `esc` closes the editor
without keeping it.

During the launch the progress lines redraw in place and all keys except `ctrl+c` are
ignored. On success the TUI returns to Instances; on failure the form stays open with the
error in the status line. With the Wait toggle on, the launch view stays open until
cloud-init finished (`launch --wait`).

## Logs (takeover)

Streams the selected instance's log with `follow` from the start of the available output:
the serial console for a VM, the container's output for a container. At most the last
2000 lines are kept in memory. Follow mode is on when the view opens.

| Key | Action |
|-----|--------|
| `f` | Toggle follow (turning it on jumps to the bottom). |
| `↑` `k` / `↓` `j` | Scroll one line. Scrolling up from the bottom turns follow off. |
| `pgup` `b` / `pgdown` `space` | Scroll one page. |
| `u` `ctrl+u` / `d` `ctrl+d` | Scroll half a page. |
| `esc` `q` | Stop the stream and return to Instances. |

## 2. Snapshots

Two panels: the VMs on the left, the selected VM's snapshots on the right, headed by its
snapshot schedule. Each snapshot shows its creation time and whether it holds only the
disk or the disk plus the VM state. `tab` moves focus between the panels.

| Key | Action |
|-----|--------|
| `tab` | Switch focus between VMs and snapshots. |
| `n` | New snapshot of the selected VM (name: letters, digits, `_`, `-`, `.`). |
| `S` | Set the schedule: Every (for example `6h`; empty turns it off) and Keep (default 7). |
| `a` `enter` | Snapshots panel: restore the selected snapshot, after a confirmation. This discards disk changes made since. |
| `d` | Snapshots panel: delete the selected snapshot, after a confirmation. |
| `r` | Reload the snapshots of the selected VM. |
| `esc` `q` | Back to the sidebar. |

## 3. Images

Two modes, shown as tabs at the top.

- **VM images**: cached (downloaded) images on the left, with arch, size, how many
  instances use them and, once computed, the sha256; the catalog on the right, with
  distro, version, arch, minimum disk and default user.
- **Container images**: the engine's local images, with engine, size and how many
  containers use them.

| Key | Action |
|-----|--------|
| `v` / `c` | Switch to VM images / container images. |
| `tab` | VM mode: switch focus between cached images and the catalog. |
| `h` | VM mode, cached panel: compute the image's sha256. |
| `x` | Delete the selected cached VM image (only the download, not the catalog entry) or container image, after a confirmation. |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 4. Intents

The list of intents, each with its member count and subnet.

| Key | Action |
|-----|--------|
| `i` `enter` | Show the members (role, kind, IP, DNS name) and the intent's DNS zone and server. Any key goes back. |
| `x` | Remove the intent: `y`/`enter` ungroups only, `p` ungroups and also deletes every member instance, any other key cancels. |
| `E` | Export the intent to a bundle. |
| `I` | Import a bundle (optional rename of the intent). |
| `A` | Apply a manifest: path, Prune toggle, Recreate VMs toggle. A dry run shows the plan first; `enter` or `y` applies it, `esc`, `q` or `n` cancels. See [apply.md](apply.md). |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

While an export, import or apply runs, keys are ignored; the finished transcript stays
until `esc`, `enter` or `q`.

## 5. Cloud-Init

The saved cloud-init library: names on the left, an editor on the right. The editor title
shows the loaded config, with `*` after unsaved changes.

| Key | Action |
|-----|--------|
| `enter` `tab` | Load the selected config into the editor. |
| `e` | Start editing the loaded config. |
| `ctrl+s` | While editing: save to the library. |
| `esc` | While editing: stop editing (the text stays in the editor, unsaved). |
| `n` | New config with an empty `#cloud-config`. |
| `m` | Import a local file under a name. The file is read on the machine running the TUI. |
| `R` | Import a template repo from a manifest URL, with a Force toggle to overwrite existing names. Any key hides the progress; the import keeps running. |
| `w` | Wizard: name, default user, SSH public key, packages (comma-separated), run commands (semicolon-separated). |
| `r` | Rename the loaded config. |
| `d` | Delete the loaded config, after a confirmation. |
| `esc` `q` | Back to the sidebar. |

## 6. Mirrors

The configured mirrors, each with kind, source (manifest URL, or registry and the registry
it mirrors) and whether it is enabled. See [mirrors.md](mirrors.md).

| Key | Action |
|-----|--------|
| `a` | Add a mirror: Name, Manifest URL (VM), Registry and Mirror of (container), Priority. `ctrl+k` switches between VM and container. |
| `e` | Enable or disable the selected mirror. |
| `x` | Remove it, after a confirmation. |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 7. Migration

Known hosts on the left, a migrate form on the right (Name of an instance or intent, To a
host alias or `user@host[:port]`, and the Copy, Best-effort and Dry run toggles).

| Key | Action |
|-----|--------|
| `tab` | Move focus to the migrate form. In the form, the form keys apply and `esc` returns to the host list. |
| `a` | Add a known host: alias, `user@host[:port]`, optional identity path, strict host key toggle. |
| `d` | Look for anvil hosts on the local network over mDNS. The results replace the host list. |
| `enter` | On a discovered host: open the add form, pre-filled with its name and `root@<address>`. |
| `x` | Remove the selected saved host, after a confirmation. |
| `t` | Test the connection to the selected saved host. |
| `r` | Reload the saved hosts. |
| `esc` `q` | Leave the discovery results; from the saved hosts, back to the sidebar. |

During a migration keys are ignored. When it ends, `tab` returns to the host list and
`esc` or `q` returns to the sidebar.

## 8. Events

The event log (`anvil events`), newest first: time, instance, action, the user who caused
it (or `anvild`) and the message or error. A failed operation is marked `✕`. The newest
500 events are loaded when the screen opens and again after instance changes.

| Key | Action |
|-----|--------|
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |
