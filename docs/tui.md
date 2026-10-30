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
a container exec runs `docker exec` with `DOCKER_HOST=ssh://<host>`, and a path the
daemon reads or writes (a mount's host path, an export's output, an import's bundle) must
then be absolute on the remote host. Those fields then have no local Tab completion and
the export output has no default. A transfer's local path stays on the machine running
the TUI, as with `anvil transfer`: `scp -J <host>` for a VM, `docker cp` with
`DOCKER_HOST=ssh://<host>` for a container.

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

- **Header**: the TUI's own version, local `user@hostname`, the socket (or `host:socket` with `--remote`), the
  daemon state (`connecting…`, `daemon connected` with the daemon's version,
  `daemon unreachable`, taken from the last instance list request), the running/total instance count and a clock.
- **Sidebar**: the ten screens of `screenOrder` in `internal/tui/app.go`. The entry with
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
| `alt+1` … `alt+9`, `alt+0` | Jump to that sidebar entry (`0` is the tenth), focused on its content. Not in the Launch and Logs takeovers. This is checked before any form, so it also leaves a half-filled form. |

### Sidebar (when it has focus)

| Key | Action |
|-----|--------|
| `↑` `k` `shift+tab` | Previous screen (wraps around), loaded at once. |
| `↓` `j` `tab` | Next screen (wraps around). |
| `home` `g` / `end` `G` | First / last screen. |
| `enter` `→` `l` | Move focus into the screen's content. |
| `1` … `9`, `0` | Jump to that screen (`0` is the tenth), focused on its content. |
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

### YAML editors

The Cloud-Init, Manifests and Launch (`ctrl+e`) editors highlight YAML and check the text
as it is typed. A line with an error is marked `✕` in the gutter, a line with a warning
`!`. The row under the text shows the problem on the cursor's line, else the first one.

- cloud-init: the first line must be `#cloud-config` (scripts, `#include` and the other
  user-data formats are accepted unchecked), the YAML must parse, top-level keys must be
  known cloud-init keys (a warning, as cloud-init still runs the rest), and common keys
  must have the right shape (`runcmd` a list, `package_update` true or false, every
  `write_files` entry a `path`, every `users` entry a `name`).
- manifests: the same checks as `anvil apply`, with variables from the shell and `.env`
  in the current directory.

The same checks run from the CLI: `anvil cloud-init validate` and `anvil manifest validate`.

### Confirmations

Destructive actions ask first. `y` or `enter` confirms; any other key cancels. The
instance and intent delete prompts have an extra `p` choice, and the image delete prompts
an extra `f` choice, described below.

## 1. Instances

The main screen: the instance list on the left, a detail panel for the selected instance
on the right.

Each list row shows a state marker, the name, the kind (`vm`, `vm/firecracker` or
`container`), the state, the image and an
address (a bridged VM's static IP, otherwise the first IP the guest agent reported).

The detail panel shows kind, state, image, creation time, size (vCPUs, memory, disk) and
the SSH user and forwarded port for a VM, the engine, short container ID, network alias
and volumes for a container, the autostart flag and restart policy, whether it can run
GUI apps, the snapshot schedule, the intent and role, port forwards, mounts and the other
intent members' host names it resolves. For a running instance it adds live data, streamed every 2 s
for the selected row only (`StreamStats`),
while the Instances screen is open: address, uptime, the guest agent state, cloud-init state and
guest IPs (VM), and gauges for CPU, memory, disk usage (VM) or disk read/write rate
(container), and network rate (shown as "not available" for a SLIRP VM).
Under that, as many of the instance's recent events as fit, newest first, with the user
who caused each one and any error (see `anvil events`).

| Key | Action |
|-----|--------|
| `n` | Open the Launch takeover. |
| `space` | Mark or unmark the selected instance (a `✓` in front of its name) and move down. While any instance is marked, `s`, `S` and `d` act on the marked ones instead of the selected one. |
| `s` | Stop the selected instance if it is running, otherwise start it. Stop uses the daemon's default 30 s graceful timeout. With marks: stop the marked ones that run, or start them all when none runs. |
| `S` | Stop a running instance with options: Force (skip the graceful shutdown) and Timeout seconds (`anvil stop --force --timeout`). With marks: the same for every marked instance that runs. |
| `x` | Open a shell in a VM: asks for a user (blank means the image's default user) and an optional private key (`--identity`, blank means anvil's own key), then suspends the TUI and runs `ssh` into the VM until the session ends. On a container it opens `/bin/sh` through `docker exec`, like `anvil shell`. |
| `e` | Run one command: asks for the command (and a user and private key, for a VM), then runs it over `ssh` for a VM or `docker exec -it` for a container. |
| `t` | Copy a file or directory to or from the instance (`anvil transfer`): a "From instance" toggle (off copies into the instance, the choice is remembered), the local path, the path inside the instance, and for a VM a user and a private key (pre-filled like `x`). The TUI is suspended while `scp -r` (VM) or `docker cp` (container) runs. |
| `g` | Run a graphical app: command, X11 toggle, user, private key (VM), and whether to add it to this desktop's app launcher, with an optional launcher title and icon file (`anvil gui` and `anvil gui export`, see [gui.md](gui.md)). |
| `l` | Open the Logs takeover. |
| `L` | Open the Logs takeover from the last N lines only (`anvil logs --tail`). |
| `d` | Delete, after a confirmation that names the instance, or the count and names of the marked ones: `y`/`enter` deletes, `p` also removes the base image if no other VM uses it, any other key cancels. |
| `f` | Fork the selected VM: new name, and whether to start it at once. Shows the progress until dismissed. |
| `u` | Settings: vCPUs, memory, disk (VM), autostart and restart policy, pre-filled with the current values. Only changed fields are sent (`anvil set`). |
| `w` | Wait for first-boot setup (cloud-init) of a running instance, streaming progress. `esc` stops waiting. |
| `m` | Mount a host directory into a VM: host path, guest path, read-only toggle. Without `--remote` a "Create and grant access" toggle first creates the host directory and grants the `anvil` user access to it, like `anvil create-dir`. |
| `M` | Unmount from a VM; the guest path field suggests the current mounts. |
| `p` | Add a port forward: host port, guest port, UDP toggle. |
| `P` | Remove a port forward; the host port field suggests the current ones. |
| `E` | Export the selected instance to a bundle (output path). |
| `i` | Import a bundle (bundle path, optional rename). |
| `r` | Reload the list. |
| `esc` `q` | Clear the marks if any, otherwise back to the sidebar. |

Marks are cleared once `s`, `S` or `d` runs, and dropped for instances that disappear.
While an export, import or fork runs, every key is ignored; when it ends, its transcript
stays on screen until `esc`, `enter` or `q`. The same keys dismiss a finished wait.

Either choice removes the instance and its disk. See [lifecycle.md](lifecycle.md#delete).

## Launch (takeover)

A form for a new instance. The subtitle shows the kind (`vm` or `container`), and the
fields depend on it:

- Both: Name (defaults to a name derived from the image, e.g. `ubuntu-24.04`), Image (suggestions from the VM catalog and cached
  images, or from local container images).
- VM: CPUs, Memory MiB, Hypervisor (`qemu`, the default, or `firecracker`; with `firecracker` the Image
  suggestions only list images it can boot, see [firecracker.md](firecracker.md)),
  Disk GiB (default 0, the image minimum, as in the CLI), Cloud-init name (suggestions
  from the library), Extra SSH keys (literal keys or `.pub` paths, comma-separated, on top
  of anvil's own key, `--ssh-key`), Guest agent toggle (on by default).
- qemu VM, at the very end: a `Pass <id>` toggle per free host device (`--device`, see
  [passthrough.md](passthrough.md)). Blocked and in-use devices are listed above the form.
- Container: Env (`K=V,K2=V2`), Volumes (`host:guest[:ro],...`), Entrypoint, Command
  (split on spaces, overrides the image's command like `anvil launch image -- cmd`).
- Both: Start toggle (on by default; off is `--no-start`), Wait until ready toggle
  (cloud-init for a VM, `--wait`), Wait timeout (default `15m`).
- Both, at the end: Autostart toggle, Restart policy (suggestions `no`, `on-failure`,
  `on-failure:5`, `always`), Intent name, Role, Ports (`host:guest[/tcp|udp],...`).

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
the instance finished its first-boot setup (`launch --wait`). Wait needs Start on.

## Logs (takeover)

Streams the selected instance's log with `follow` from the start of the available output
(or from the last N lines, with `L`):
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
  instances use them and, once computed, the sha256. Firecracker's raw copies of a base
  image and its guest kernel are listed too, marked by kind; the catalog on the right, with
  distro, version, arch, minimum disk and default user.
- **Container images**: the engine's local images, with engine, size and how many
  containers use them.

| Key | Action |
|-----|--------|
| `v` / `c` | Switch to VM images / container images. |
| `tab` | VM mode: switch focus between cached images and the catalog. |
| `/` | VM mode: filter the catalog by id, distro, version or name, case-insensitive, like `anvil find <term>`. The list updates while typing; `enter` keeps the filter, `esc` clears it. |
| `h` | VM mode, cached panel: compute a base image's sha256 (not offered for raw copies or the kernel). |
| `x` | Delete the selected cached VM image (only the download, not the catalog entry; deleting a base also deletes its Firecracker raw copy) or container image, after a confirmation. There `f` deletes even if an instance still uses it (`--force`, this breaks that instance). |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 4. Intents

The list of intents, each with its member count and subnet.

| Key | Action |
|-----|--------|
| `n` | Create an intent with members (`anvil intent create`): Name, VM members and Container members (each `role:image,...`), Hypervisor for the VMs. |
| `a` | Add members to the selected intent (`anvil intent add`), with the same fields minus the name. |
| `i` `enter` | Show the members (role, kind, instance ID, IP, DNS name) and the intent's network: engine network and bridge, subnet and gateway, DNS zone and server. Any key goes back. |
| `x` | Remove the intent: `y`/`enter` ungroups only, `p` ungroups and also deletes every member instance, any other key cancels. |
| `U` | Remove one member from the intent (role or instance ID; `tab` suggests the roles). The instance is kept as a standalone one. |
| `E` | Export the intent to a bundle. |
| `I` | Import a bundle (optional rename of the intent). |
| `A` | Apply a manifest: path, Vars (`KEY=VALUE,...`, `--var`), Env file (`--env-file`), Wait timeout, Prune toggle, Recreate VMs toggle. A dry run shows the plan first; `enter` or `y` applies it, `esc`, `q` or `n` cancels. See [apply.md](apply.md). |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

`n` and `a` launch the members one at a time (VMs first) with `IntentService.AddMembers`,
showing each member's progress prefixed with its role. The first failure stops the rest;
members launched before it are kept, as with the CLI.

While an export, import, apply or member launch runs, keys are ignored; the finished
transcript stays until `esc`, `enter` or `q`.

## 5. Manifests

The saved manifest library (`anvil manifest`): names on the left, each with its intent,
an editor on the right. Manifests are stored by anvild, so they are the same over `--remote`.

| Key | Action |
|-----|--------|
| `enter` `tab` | Load the selected manifest into the editor. |
| `e` | Start editing the loaded manifest. |
| `ctrl+s` | While editing: save to the library. |
| `esc` | While editing: stop editing (the text stays in the editor, unsaved). |
| `n` | New manifest from a starter template; the name is also used as the intent name. |
| `m` | Import a local `anvil.yaml` (the name defaults to the file name). The file is read on the machine running the TUI. |
| `E` | Export the loaded manifest to a local file. |
| `a` | Apply the loaded manifest, with the Prune and Recreate VMs toggles. Unsaved edits must be saved first. The plan is shown first; `enter` or `y` applies it, `esc`, `q` or `n` cancels. |
| `x` | Tear down the manifest's intent: `y`/`enter` deletes the intent and its members, `u` ungroups only, any other key cancels. |
| `r` | Rename the loaded manifest. |
| `d` | Delete the loaded manifest from the library, after a confirmation. A running intent is left alone. |
| `esc` `q` | Back to the sidebar. |

Variables are filled as for `anvil apply`, with `.env` read from the directory the TUI
was started in. See [apply.md](apply.md).

## 6. Cloud-Init

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
| `w` | Wizard: name, default user, SSH public key, packages (comma-separated), run commands (semicolon-separated). Same as `anvil cloud-init new <name> --user --ssh-key --package --run`. |
| `r` | Rename the loaded config. |
| `d` | Delete the loaded config, after a confirmation. |
| `esc` `q` | Back to the sidebar. |

## 7. Mirrors

The configured mirrors, each with kind, source (manifest URL, or registry and the registry
it mirrors) and whether it is enabled. See [mirrors.md](mirrors.md).

| Key | Action |
|-----|--------|
| `a` | Add a mirror: Name, Manifest URL (VM), Registry, Mirror of and Insecure toggle (container), Priority. `ctrl+k` switches between VM and container, showing only that kind's fields. |
| `e` | Enable or disable the selected mirror. |
| `x` | Remove it, after a confirmation. |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 8. Migration

Known hosts on the left, a migrate form on the right (Name of an instance or intent, To a
host alias or `user@host[:port]`, an optional Dest name to rename a single instance on the
target, and the Copy, Best-effort and Dry run toggles).

| Key | Action |
|-----|--------|
| `tab` | Move focus to the migrate form. In the form, the form keys apply and `esc` returns to the host list. |
| `a` | Add a known host: alias, `user@host[:port]`, optional identity path, strict host key toggle. |
| `d` | Look for anvil hosts on the local network over mDNS. The results replace the host list. |
| `D` | The same, asking first how long to wait for answers (`anvil host discover --timeout`). |
| `enter` | On a discovered host: open the add form, pre-filled with its name and `root@<address>`. |
| `x` | Remove the selected saved host, after a confirmation. |
| `t` | Test the connection to the selected saved host. |
| `K` | Show anvild's migration public key (`anvil migrate-key`), to add to `authorized_keys` on a target host. Any key goes back. |
| `r` | Reload the saved hosts. |
| `esc` `q` | Leave the discovery results; from the saved hosts, back to the sidebar. |

During a migration keys are ignored. When it ends, `tab` returns to the host list and
`esc` or `q` returns to the sidebar.

## 9. Events

The event log (`anvil events`), newest first: time, instance, action, the user who caused
it (or `anvild`) and the message or error. A failed operation is marked `✕`. The newest
500 events are loaded when the screen opens and again after instance changes; `f` changes
the instance and the number of events loaded. With the
cursor on the newest event it follows new ones; elsewhere it stays on the same event.

| Key | Action |
|-----|--------|
| `f` | Filter: only one instance's events (blank for all) and how many of the newest to load (0 for all), like `anvil events <name> -n`. |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 10. Devices

The host's USB and PCI devices (`anvil host devices`): id and name, then vendor:product,
driver and status (free, in use by an instance, or blocked and why). A PCI device also
shows the other IOMMU group members it brings along. See [passthrough.md](passthrough.md).

| Key | Action |
|-----|--------|
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 11. GUI Apps

The desktop entries added with `anvil gui export` (or the "Add to desktop" toggle of `g`),
like `anvil gui exports`: the app's launcher name and instance, then the app id, its
command and the entry's file. These are files of the machine running the TUI, so the
screen shows the same list with or without `--remote`. See [gui.md](gui.md).

| Key | Action |
|-----|--------|
| `enter` | Start the app with the entry's own command line, as the app launcher does (`anvil gui --detach`). |
| `x` | Remove the entry and its icon, after a confirmation (`anvil gui unexport`). |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |

## 12. Ephemeral

Warm snapshots and the clones of ephemeral runs (`anvil warm list`, `anvil ephemeral
list`): `tab` switches between the two lists. A run started here shows one pane per clone
with its last lines of output, its progress and then its exit code. See
[ephemeral.md](ephemeral.md).

| Key | Action |
|-----|--------|
| `tab` | Switch between warm snapshots and clones. |
| `n` | Start a run: distros, a path to upload, a command (run with `sh -c`), size and Keep failed. `esc` in the run view cancels it and deletes its clones, or closes it once done. |
| `c` | Build a warm snapshot for a distro and size (`anvil warm create`). |
| `s` `enter` | On a clone: a root shell in it (`anvil ephemeral shell`). |
| `x` | Remove the selected warm snapshot or clone, after a confirmation. |
| `r` | Reload. |
| `esc` `q` | Back to the sidebar. |
