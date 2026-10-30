# Anvil compared with similar tools

This page places anvil next to the tools it overlaps with, so the choice between them
rests on facts rather than on a feature list. Claims about other projects come from their
public documentation as of late 2026. They change fast; corrections are welcome.

## The design space

Local virtualization tools differ along four axes:

1. **Workload type.** Full VMs (own kernel), system containers (shared kernel, full init)
   or application containers (OCI images, one process tree).
2. **Scope.** One host, a set of independent hosts, or a cluster with shared state.
3. **Control model.** Imperative commands, a declarative file that is converged, or both.
4. **Abstraction depth.** A thin layer over the native hypervisor or engine, or a general
   provider layer that can target many of them.

Anvil sits at: VMs plus OCI containers, a set of independent hosts tied together only by
SSH, imperative commands plus a declarative file (`anvil apply`), and a thin layer per
platform (QEMU over QMP, the Docker HTTP API, Virtualization.framework on macOS in
progress).

## Feature matrix

| | Anvil | Multipass | Incus / LXD | Vagrant | Lima | Docker Compose |
|---|---|---|---|---|---|---|
| Full VMs | yes, QEMU/KVM | yes | yes, QEMU | yes, through providers | yes | no |
| OCI containers | yes, Docker | no | yes (Incus 6.3+) | through the Docker provider | inside the VM | yes |
| System containers | no | no | yes, the core feature | no | no | no |
| Grouping of mixed workloads | intents: shared bridge, IPAM, DNS | no | projects and networks, wired by hand | multi-machine Vagrantfile | no | services in one file |
| Declarative file | `anvil.yaml`, plan then apply | no | preseed, profiles, Terraform | Vagrantfile (Ruby) | per-VM YAML template | `compose.yaml` |
| Guest images | 15 distros built in, mirrors for more | Ubuntu first | large image server | Vagrant Cloud boxes | many templates | any OCI image |
| First-boot config | cloud-init, saved template library | cloud-init | cloud-init | provisioners (shell, Ansible, ...) | cloud-init-like provisioning | image + env |
| Snapshots | yes, live, scheduled with retention | yes | yes, scheduled | yes, provider-dependent | experimental | no |
| Clone a running VM | `anvil fork`, shares the base image | `multipass clone` (stopped) | `incus copy` | no | `limactl clone` | no |
| Throwaway VM for one command | `launch --ephemeral`, clone of a warm Firecracker snapshot, exit code back | no | `--ephemeral` instances, deleted on stop, VMs boot cold | no | no | `compose run --rm`, containers only |
| Same command on several distros | `anvil matrix`, in parallel, per-distro output and summary | no | no | no | no | no |
| Export to a file | bundle with disks and bind-mounted data | no | `incus export` | `vagrant package` | no | no |
| Cross-host move | cold, over plain SSH, delta transfer | no | live or cold between remotes | no | no | no |
| Remote control | `--remote` over SSH | no | HTTPS API with TLS certs | no | no | `DOCKER_HOST` |
| Interface | CLI, TUI | CLI, GUI | CLI, web UI | CLI | CLI | CLI |
| Guest GUI apps on the host | `anvil gui`: Wayland relay, launcher export | no | by hand (device passthrough) | X11 forwarding over SSH | no | by hand (socket mounts) |
| Host OS | Linux; macOS in progress | Linux, macOS, Windows | Linux | Linux, macOS, Windows | macOS, Linux | Linux, macOS, Windows |
| API | gRPC on a unix socket | gRPC | REST | none (Ruby plugins) | none | Docker API |
| Maturity | young, single maintainer | mature, Canonical | mature, large community | mature, HashiCorp | mature, CNCF | mature, Docker |

## Tool by tool

**Multipass.** The model anvil started from: one daemon, quick cloud-init VMs, `shell`
and `exec`. It stays VM-only and Ubuntu-first, has no grouping and no way to move an
instance. Anvil keeps the interaction style and adds containers, intents, export and
migration. Multipass runs on more host systems and is far more tested.

**Incus / LXD.** The closest tool in scope and the most capable. It runs system
containers, VMs and (Incus) OCI containers, with clustering, storage pools, live
migration and a REST API. The cost is setup: storage pools, a network, remotes with TLS
trust, and for multi-host work a cluster. OCI containers run under Incus's own runtime,
not under an existing Docker daemon. Anvil trades breadth for less setup: Docker images
and the Docker daemon already on the host are used as they are, and cross-host trust is
whatever SSH access already exists.

**Vagrant.** A provider layer over many hypervisors, driven by a Ruby `Vagrantfile`.
Good for reproducible dev boxes on any OS. It has no daemon, so there is no live view,
no restart policy and no state shared between shells; each provider brings its own
limits. Anvil has one backend per platform and keeps state in a daemon.

**Lima.** Linux VMs, mostly on macOS, often as the engine under container tools
(Colima, Rancher Desktop). Containers run inside the VM, they are not first-class
instances next to it. Anvil treats VMs and containers as peers on the same host.

**Docker Compose.** Declarative groups of containers. No VMs. `anvil apply` borrows the
idea (members, `depends_on`, a converge step) and extends it to members that need their
own kernel: a database in a container and a service that needs a full systemd VM in the
same file, resolving each other by name.

## When anvil is the wrong choice

- A cluster with shared storage, live migration or HA: use Incus or Proxmox.
- Windows hosts: use Multipass, Vagrant or Hyper-V directly.
- Containers only: Docker Compose or Podman are simpler and well known.
- Production with uptime requirements: anvil is young and has one maintainer.

## Where anvil is different

- VMs and OCI containers as peers, in one group, on one bridge, with per-group DNS.
- Cold migration and export that carry disk deltas, bind-mounted data and network config,
  with no trust setup beyond SSH.
- Live snapshots with a schedule and retention, and forks of a running VM that share the
  base image.
- A TUI over the same API as the CLI, usable against a remote host.
