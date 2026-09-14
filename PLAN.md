# Anvil — what's missing

Everything else that used to live in this file (the original design rationale, the
milestone-by-milestone build log) is done and shipped — see `README.md` for what Anvil
actually does today. This file is just the list of what isn't built yet.

## Podman backend

Docker is the only container engine right now. `--engine podman` returns a clear
"not implemented yet" error instead of silently doing nothing, but there's no actual
Podman code (`internal/container/podman/` is an empty package).

A few things are blocked on this landing:

- **Cross-kind networking for Podman.** A VM and a container sharing an intent's
  network is done and working for Docker; there's no equivalent design for Podman/
  netavark yet.
- **Mixing engines in one intent.** Whether an intent can contain both a Docker
  container and a Podman container at once is undecided, current plan is "don't
  allow it" until there's a real reason to.
- **Default engine.** The intent has always been for Podman to be the default once
  both engines exist (rootless-first, no daemon socket to trust). Nothing enforces
  that yet since Docker is still the only option.
- **Access model under an unprivileged daemon.** `anvild` now runs as its own
  unprivileged system user, not root. Podman's rootful socket doesn't have a
  Docker-group-style shared-access convention, so this needs its own answer —
  rootless Podman most likely, but not decided.

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
- **No bootstrap over SSH.** `anvil migrate` assumes `anvil`/`anvild` are already
  installed on the target. There's no path that installs Anvil there for you first.
