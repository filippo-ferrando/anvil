# Mirrors

A mirror is a runtime-added source anvil pulls images from, on top of whatever it
already knows about by default. There are two completely different kinds under one
name, because they solve the same problem ("I want images to come from somewhere else")
for two different systems:

- **VM mirrors** add or override entries in the distro catalog `anvil launch --kind vm`
  resolves against.
- **Container mirrors** rewrite an image reference to pull through a different registry
  before Docker (or, later, Podman) fetches it.

Both are managed with the same command, `anvil mirror`, and the same TUI screen
(Mirrors). Which fields matter depends on `--kind`.

```
anvil mirror add <name> --kind vm|container [flags]
anvil mirror list [--kind vm|container]
anvil mirror remove <name>
anvil mirror enable <name>
anvil mirror disable <name>
```

A disabled mirror stays in the registry but is skipped everywhere: `anvil find`,
`anvil launch`, and container pulls all only look at enabled mirrors.

## VM mirrors

anvil ships with a built-in catalog of common distros (Ubuntu, Debian, Arch, Fedora,
Rocky, and a few others), embedded in the binary. A VM mirror adds to that catalog, or
overrides an entry in it, without needing a new anvil release. Two real reasons you'd
want one:

- **A faster or internal copy of the same distro images.** Say your org already mirrors
  Ubuntu cloud images internally. Point a mirror at your own manifest that re-lists the
  same `id`s with your own URLs, give it a higher priority than the default (which is
  priority 0), and every `anvil launch --kind vm ubuntu-24.04` now pulls from your
  mirror instead of the public one, with zero changes anywhere else.
- **Distros the built-in catalog doesn't have at all.** Add entries under new `id`s and
  they show up in `anvil find` and are launchable immediately.

### The manifest

`--manifest-url` points at a JSON document in the exact same shape as the built-in
catalog:

```json
{
  "schema_version": 1,
  "distros": [
    {
      "id": "ubuntu-24.04",
      "name": "Ubuntu 24.04 LTS (Noble Numbat)",
      "distro": "ubuntu",
      "version": "24.04",
      "arch": "x86_64",
      "url": "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img",
      "sha256": "<the image's real sha256, checked after download>",
      "min_disk_gib": 3,
      "default_user": "ubuntu"
    }
  ]
}
```

Field notes:

- `id` is what you pass to `anvil launch --kind vm <id>` and `anvil find`. If it
  collides with an existing entry (built-in or from another mirror), the one with the
  higher `priority` wins. Same `id`, different `arch`, are two separate images, not a
  collision.
- `sha256` is checked against the actual downloaded file. A wrong checksum here means
  every launch of that distro fails at download time, not silently boots the wrong
  image.
- `min_disk_gib` is a floor, not the real virtual size anvil goes by. The disk overlay
  is sized against the image's own actual virtual size at download time, so a stale
  `min_disk_gib` in an old manifest doesn't cause a broken launch.
- `default_user` is the account cloud-init already sets up in that image, used so
  `anvil shell`/`exec`/`transfer` know who to log in as without you passing `--user`
  every time. Best-effort: if it's wrong, `anvil shell --user <actual-user>` still
  works.
- `schema_version` must be `1` (the only version that exists right now). A manifest
  with an unrecognized major version is rejected outright when you try to add it,
  rather than silently misread.

### Adding one

```
anvil mirror add corp-ubuntu --kind vm \
  --manifest-url https://images.internal.example.com/anvil/distros.json \
  --priority 10
```

The manifest is fetched and validated once, right when you run `add`, not on every
later launch. The fetched JSON is cached in anvil's own registry, so a machine with no
network access can still launch from a mirror it already knows about; you only need
connectivity again to add a new one or to actually download an image for the first
time.

```
anvil mirror list --kind vm
anvil find                      # see everything currently resolvable, built-in plus mirrors
anvil mirror disable corp-ubuntu
anvil mirror remove corp-ubuntu
```

## Container mirrors

A container mirror redirects a pull from one registry to another, entirely on anvil's
side, before the image is fetched. This is deliberately not the same thing as Docker's
own `daemon.json` `registry-mirrors` setting: that setting only ever mirrors Docker Hub,
and editing a system-wide daemon config (plus restarting dockerd) for a per-anvil
concern is a bigger blast radius than it needs to be. Instead, anvil rewrites the image
reference itself, client-side, right before creating the container.

```
anvil mirror add corp-registry --kind container \
  --registry registry.internal.example.com:5000 \
  --mirror-of docker.io \
  --priority 10
```

What this does: any image reference that would otherwise resolve to Docker Hub (that's
`--mirror-of`'s value; leave it unset and it defaults to `docker.io`, since that's what
an unqualified reference like `nginx:alpine` already means) gets its registry host
swapped for `registry.internal.example.com:5000` before the pull. So with the mirror
above enabled:

```
anvil launch --kind container nginx:alpine
# actually pulls registry.internal.example.com:5000/nginx:alpine
```

A reference that already names an explicit registry host (`quay.io/something:latest`)
is untouched unless you *also* add a mirror with `--mirror-of quay.io`. Multiple
enabled mirrors for the same upstream resolve by priority, same as VM mirrors: highest
wins.

`--insecure` records that this registry should be treated as HTTP/self-signed, but it's
only a record right now for Docker specifically: Docker itself still needs its own
`insecure-registries` entry in `/etc/docker/daemon.json` (and a restart) for that to
actually take effect, since anvil doesn't manage dockerd's own config. Podman's
`registries.conf.d` mechanism (once the Podman backend lands) will be able to apply this
properly at the daemon level instead of needing that extra manual step.

### Podman

Container mirrors are Docker-only for now. Podman has its own native mirror mechanism
(`registries.conf.d` drop-ins), which is a better fit for it than anvil's own rewrite
logic, but that integration hasn't been built yet since the Podman backend itself
hasn't landed.

## From the TUI

The Mirrors page (`anvil tui`, sidebar) shows the same list `anvil mirror list` does,
with `a` to add (the form has a field for both kinds; only the ones relevant to whichever
kind you pick actually get sent), `e` to enable/disable the selected one, and `x` to
remove it.
