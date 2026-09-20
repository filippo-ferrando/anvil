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

### Creating a cloud-image mirror from scratch

A worked example, start to finish: you want to host a small internal mirror serving one
custom distro image (or a faster re-hosted copy of an existing one) from a plain static
file server.

1. **Get the qcow2 image onto some HTTP host.** Anything that can serve a static file
   over HTTP(S) works: S3/GCS/a bucket with public or VPN-only access, an nginx
   `autoindex`'d directory, a GitHub Pages site, even a `python3 -m http.server` for a
   quick test. anvil only ever does a plain `GET`, no special headers or auth scheme
   assumed, so anything requiring e.g. a signed URL or a login form in front of it won't
   work unless the URL itself is enough (a pre-signed S3 URL is fine; an interactive
   login page is not).

   ```
   mkdir -p /srv/anvil-mirror
   cp my-distro-cloudimg-amd64.qcow2 /srv/anvil-mirror/
   ```

2. **Compute its sha256.** anvil checks this after every download, so it must be the
   real hash of the exact bytes being served:

   ```
   sha256sum /srv/anvil-mirror/my-distro-cloudimg-amd64.qcow2
   ```

3. **Write the manifest**, `distros.json`, next to it (or anywhere else reachable by
   URL, it doesn't need to be on the same host as the images). One entry per image;
   `url` can point anywhere, it doesn't have to be relative to the manifest's own
   location:

   ```json
   {
     "schema_version": 1,
     "distros": [
       {
         "id": "my-distro-1.0",
         "name": "My Distro 1.0",
         "distro": "my-distro",
         "version": "1.0",
         "arch": "x86_64",
         "url": "https://mirror.internal.example.com/anvil-mirror/my-distro-cloudimg-amd64.qcow2",
         "sha256": "<the sha256sum output from step 2>",
         "min_disk_gib": 4,
         "default_user": "debian"
       }
     ]
   }
   ```

   Serving the manifest from the same static host works fine:

   ```
   cp distros.json /srv/anvil-mirror/
   ```

4. **Serve it** (skip this if you're using a bucket/CDN that's already serving over
   HTTP):

   ```
   cd /srv/anvil-mirror && python3 -m http.server 8080
   # or: nginx pointed at /srv/anvil-mirror, or an S3 bucket with static hosting on
   ```

5. **Register it with anvil**, from any anvil host that can reach that URL:

   ```
   anvil mirror add my-mirror --kind vm \
     --manifest-url http://mirror.internal.example.com:8080/distros.json
   ```

   anvil fetches `distros.json` immediately, validates every entry (rejects the whole
   manifest if `schema_version` isn't `1`, or if any entry is missing `id`/`url`), and
   caches the parsed result. If this succeeds, `anvil find` shows `my-distro-1.0`
   right away: you don't need to already have the qcow2 downloaded, only the manifest
   needs to be reachable at add-time.

6. **Launch from it** like any other catalog entry:

   ```
   anvil launch --kind vm my-distro-1.0 --name test
   ```

   The qcow2 itself is only fetched (and its sha256 checked) the first time something
   actually launches from that `id`, *adding the mirror doesn't pre-download anything*.

Updating the mirror later (new image version, moved URL) is just editing `distros.json`
on the server and re-running `anvil mirror add` with the same name: it re-fetches and
replaces the cached manifest.

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

## Cloud-init template repos

This is a separate, related mechanism, not a third `--kind` on `anvil mirror`: it
bulk-imports whole cloud-init configs into your saved library (`anvil cloud-init list`)
rather than adding launchable catalog entries. Think of a VM mirror as "where the disk
image comes from" and a template repo as "a bundle of ready-made `#cloud-config`
documents someone else already wrote" -> nginx-with-TLS, a Postgres instance with a data
volume pre-mounted, a k3s single-node setup, that kind of thing.

```
anvil cloud-init import-repo <manifest-url> [--force]
```

Every listed template is fetched and saved into the library under its own `name`, ready
to use immediately as `anvil launch --kind vm ... --cloud-init-name <name>`. A name
already in your library is left untouched unless you pass `--force`; a single template
failing to fetch doesn't stop the rest of the repo from importing, you get a per-
template result either way (`imported`, `skipped`, or `FAILED: <reason>`).

### The manifest

`<manifest-url>` points at a JSON document shaped like this:

```json
{
  "schema_version": 1,
  "templates": [
    {
      "name": "nginx-tls",
      "description": "nginx with a self-signed cert, ready on boot",
      "url": "https://templates.internal.example.com/nginx-tls.yaml"
    },
    {
      "name": "postgres-16",
      "description": "PostgreSQL 16 with a data volume pre-mounted",
      "url": "https://templates.internal.example.com/postgres-16.yaml"
    }
  ]
}
```

Field notes:

- `name` is what the template is saved into the library as, and what `--cloud-init-name`
  later refers to. Required, and must be non-empty: a template missing it fails the
  whole manifest at fetch time, same as a VM mirror manifest missing `id`.
- `url` points at the actual cloud-init YAML content (a plain `#cloud-config` document,
  fetched and saved byte-for-byte, no templating/substitution applied). Required.
- `description` is metadata only right now, not shown anywhere yet, just documents
  intent for whoever's reading the manifest.
- `schema_version` must be `1`, same "reject an unrecognized major version outright"
  rule as the VM mirror manifest.

Unlike a VM mirror, a template repo isn't a standing, re-consulted registry entry,
`import-repo` is a one-shot bulk copy into your library. There's nothing to `list`/
`remove`/`enable`/`disable` afterward; once imported, a template is just a normal saved
cloud-init config like any other, indistinguishable from one you wrote by hand.

### Creating your own template repo

Same static-hosting story as a VM mirror: anything that serves plain files over
HTTP(S) works.

1. **Write the cloud-init YAML files**, one per template, each a normal
   `#cloud-config` document exactly as you'd write it for `anvil cloud-init new`/`edit`:

   ```
   mkdir -p /srv/anvil-templates
   cat > /srv/anvil-templates/nginx-tls.yaml <<'EOF'
   #cloud-config
   packages:
     - nginx
     - openssl
   runcmd:
     - openssl req -x509 -nodes -newkey rsa:2048 -days 365 \
         -keyout /etc/ssl/private/nginx-selfsigned.key \
         -out /etc/ssl/certs/nginx-selfsigned.crt \
         -subj "/CN=localhost"
     - systemctl enable --now nginx
   EOF
   ```

2. **Write the manifest**, `templates.json`, listing each one with a `name` and the URL
   it'll be served at:

   ```json
   {
     "schema_version": 1,
     "templates": [
       {
         "name": "nginx-tls",
         "description": "nginx with a self-signed cert, ready on boot",
         "url": "https://templates.internal.example.com/nginx-tls.yaml"
       }
     ]
   }
   ```

3. **Serve the directory**:

   ```
   cd /srv/anvil-templates && python3 -m http.server 8081
   # or nginx/S3/GitHub Pages/anything else that serves static files over HTTP(S)
   ```

   A public GitHub repo works well here too: point `url` at the raw content URL
   (`https://raw.githubusercontent.com/<org>/<repo>/<branch>/nginx-tls.yaml`) and
   `<manifest-url>` at `templates.json`'s own raw URL.

4. **Import it**:

   ```
   anvil cloud-init import-repo https://templates.internal.example.com/templates.json
   ```

   Or from `anvil tui`'s Cloud Init page, press `R` and enter the same URL.

Updating a template later is just editing the YAML file (and `templates.json` if you
added/removed one) on the server and re-running `import-repo --force`, it *re-fetches*
and overwrites the matching saved configs by name.

## From the TUI

The Mirrors page (`anvil tui`, sidebar) shows the same list `anvil mirror list` does,
with `a` to add (the form has a field for both kinds; only the ones relevant to whichever
kind you pick actually get sent), `e` to enable/disable the selected one, and `x` to
remove it.

The Cloud Init page has its own, separate import action: `R` opens a form for a template
repo's manifest URL (see above) plus a `Force` toggle, and streams a live per-template
result as it imports, `m` (lowercase) is the older, single-file "Import" action
(reads one local file from disk into the library, unrelated to a repo's manifest, see
§CLI command surface's `anvil cloud-init new --from`/`import`) and still works
independently.
