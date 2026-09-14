# Anvil: what's missing

## Podman backend

Docker is the only container engine right now. `--engine podman` returns a clear
"not implemented yet" error instead of silently doing nothing, but there's no actual
Podman code (`internal/container/podman/` is an empty package).

A few things are blocked on this landing:

- **Podman engine support.** The `anvil` CLI needs to be able to talk to Podman
  instead of Docker, and `anvild` needs to be able to run Podman containers.
- **Cross-kind networking for Podman.** A VM and a container sharing an entire
  network is done and working for Docker; there's no equivalent design for Podman/
  netavark yet.
- **Mixing engines in one intent.** Whether an intent can contain both a Docker
  container and a Podman container at once is undecided, current plan is "don't
  allow it" until there's a real reason to.

## mDNS discovery of other Anvil hosts

Migration currently relies entirely on `anvil host add` (a saved `user@host` alias).
There's no automatic discovery of other machines running `anvild` on the same
network. Discovery wouldn't grant any trust by itself either way, so it's a pure
convenience feature, not a blocker for anything, just not built.

## Man pages

`anvil --help`/`anvil <command> --help` work; there's no `man anvil` yet. Needs
`cobra/doc`'s `GenManTree` wired into the build, plus the `go-md2man` dependency it
requires.

## Migration gaps

- **Arbitrary bind mounts aren't migrated.** Moving a container only carries over
  anvil-managed volumes; a host bind mount's actual data doesn't travel with it.
  `--dry-run` should warn about this instead of staying silent.
