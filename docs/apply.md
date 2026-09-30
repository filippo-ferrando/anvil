# Declarative intents

`anvil apply` reads an intent manifest (`anvil.yaml`), compares it with the intent's
current state and applies the difference. Running it again with no changes does nothing.

```
anvil apply -f anvil.yaml [--dry-run] [--prune] [--recreate] [--wait-timeout 15m]
```

The plan is always printed first. `--dry-run` stops there. In the TUI, press `A` on the
Intents screen: the plan is shown first and applied on `enter`.

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
`no_guest_agent`. The caller's own anvil key is always added. A VM in an intent has its
own address on the intent network, so it takes no `ports`.

Container only: `engine` (`docker`, the default, or `podman`), `env`, `entrypoint`,
`command` (both lists), `volumes` (`/host:/container[:ro]`, absolute host paths only)
and `ports` (`host:guest[/tcp|udp]`).

Unknown fields are errors, so a typo never passes silently. [example.yaml](example.yaml) uses every field.

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

Members are handled in `depends_on` order. Before a member starts, each member it depends
on has to be ready: running, and for a VM, cloud-init finished. `--wait-timeout` bounds
each of these waits.

Only one apply runs at a time on a host.
