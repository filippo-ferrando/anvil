# Declarative intents

`anvil apply` reads an intent manifest (`anvil.yaml`), compares it with the intent's
current state and applies the difference. Running it again with no changes does nothing.

```
anvil apply -f anvil.yaml [--dry-run] [-y] [--prune] [--recreate] [--wait-timeout 15m]
            [--var KEY=VALUE]... [--env-file FILE]
```

The plan is always printed first. `--dry-run` stops there. When stdin and stdout are both
a terminal, `anvil apply` asks `Apply these changes? [y/N]` after the plan and only applies
on `y`. A plan with nothing to change, `-y/--yes`, a manifest read from stdin (`-f -`) and
a non-terminal stdin or stdout (scripts, CI) skip the question and apply right away. In the TUI, press `A` on the
Intents screen: the plan is shown first and applied on `enter`.

Manifests can also be saved on the daemon and applied by name:

```
anvil manifest new shop --from anvil.yaml   # or without --from, for a starter template
anvil manifest list|show|edit|rename|delete|validate
anvil apply --saved shop
```

A saved manifest's `.env` is read from the current directory. The TUI's Manifests
screen edits, applies and tears down saved manifests.

## Manifest

```yaml
intent: shop
members:
  db:                      # the member's role, resolvable as db.shop.anvil
    kind: container
    image: postgres:16
    env:
      POSTGRES_PASSWORD: secret
    volumes: ["/srv/shop/db:/var/lib/postgresql/data"]
    ports: ["5432:5432"]
    restart: on-failure:3
  app:
    kind: vm
    image: ubuntu-24.04
    cpus: 2
    memory: 2048           # MiB
    disk: 20               # GiB, 0 or unset = catalog minimum
    cloud_init: |
      #cloud-config
      packages: [nginx]
    depends_on: [db]
    autostart: true
```

Fields for every member: `kind` (`vm` or `container`, required), `image` (required),
`name` (defaults to `<intent>-<role>`), `depends_on`, `autostart`, `restart`
(`no`, `on-failure[:N]` or `always`).

VM only: `cpus` (default 1), `memory` (default 1024), `disk`, `cloud_init` (inline) or
`cloud_init_name` (from the saved library), `ssh_keys` (literal public keys),
`no_guest_agent`, `hypervisor` (`qemu`, the default, or `firecracker`), `devices` (host USB/PCI
devices, qemu only, see [passthrough.md](passthrough.md); a change recreates the VM). The caller's own
anvil key is always added. A VM in an intent has its own address on the intent network,
so it takes no `ports`.

Container only: `engine` (`docker`, the default, or `podman`), `env`, `entrypoint`,
`command` (both lists), `volumes` (`/host:/container[:ro]`, absolute host paths only)
and `ports` (`host:guest[/tcp|udp]`).

Unknown fields are errors, so a typo never passes silently. [example.yaml](example.yaml) uses every field.

## Variables

Values can use variables, with the same syntax as docker compose:

| Syntax | Result |
| --- | --- |
| `${VAR}` or `$VAR` | the value of `VAR`, an error if unset |
| `${VAR:-default}` | `default` if `VAR` is unset or empty |
| `${VAR-default}` | `default` if `VAR` is unset |
| `${VAR:?message}` | an error with `message` if `VAR` is unset or empty |
| `${VAR?message}` | an error with `message` if `VAR` is unset |
| `$$` | a literal `$` |

Values come from, first match wins:

1. `--var KEY=VALUE` (CLI only, can be repeated)
2. the environment of the shell running `anvil apply` or `anvil tui`
3. a `.env` file beside the manifest (`KEY=VALUE` lines, `#` comments), or `--env-file`;
   for a saved manifest, `.env` in the current directory
4. the manifest's own top-level `vars:` map

```yaml
vars:
  PG_TAG: "16"
intent: shop
members:
  db:
    kind: container
    image: postgres:${PG_TAG}
    env:
      POSTGRES_PASSWORD: ${DB_PASSWORD:?set DB_PASSWORD in .env}
```

Every unset variable without a default is listed in one error, and nothing is applied.
Only values are expanded, never keys. Variables are expanded inside inline `cloud_init` too,
so a shell `$HOME` there has to be written as `$$HOME`. Inside a `{...}` flow map, quote a
value that uses `${...}`, since YAML ends the map at the first `}`. Only the variables the
manifest uses are sent to the daemon, also with `--remote`, and the plan never shows their values.

## What apply does

| Case | Action |
| --- | --- |
| member not created yet | `create` |
| only cpus, memory, disk growth, autostart or restart changed | `update`, in place (live when possible) |
| anything else changed (image, env, ports, cloud-init...) | `recreate`: delete and create again |
| member not running | `start` |
| member no longer in the manifest | kept, or deleted with `--prune` |

Replacing a VM loses its disk, so a VM `recreate` fails the plan unless `--recreate` is
given. Containers are replaced as needed, data in their volumes stays on the host.
Changing cloud-init or SSH keys only means something on first boot, which is why it
counts as a replacement.

Members are handled in `depends_on` order, one dependency level at a time: first every
member that depends on nothing, then those depending only on the first level, and so on.
The members of one level don't depend on each other, so they are applied side by side, up
to 4 at once: an intent of five independent members takes about as long as its slowest
member instead of the sum of all five. Before a member starts, each member it depends on
has to be ready: running, and for a VM, cloud-init finished. `--wait-timeout` bounds each
of these waits. Members no longer in the manifest are deleted (or kept) one at a time
before anything else, since a replacement may reuse their name or host port.

When a member of a level fails, the other members of that level still finish, every
failure is reported, and the next levels don't start. So several members can fail in one
apply, where a one-at-a-time apply stopped at the first.

One apply per intent runs at a time; applies of different intents run side by side.

`anvil intent create` and `anvil intent add` launch their members side by side too, up to
4 at once. A failing member doesn't stop the others: they are kept, and the failure is the
error reported. Inside the daemon, launching members of one intent at once works because
only the short setup (network, address, role check) and the final save hold the intent's
lock, not the launch itself; addresses and roles of launches still running are reserved,
so two members never get the same IP. A member started next to others in the same level
doesn't find them in its `/etc/hosts`, as their addresses aren't known yet; it reaches them
through the intent's DNS (`<role>.<intent>.anvil`), like any member started later.
