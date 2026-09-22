# Intent DNS

Every intent with a shared network gets its own DNS zone, `<intent>.anvil`. `anvild`
serves it on port 53 of the intent's gateway address (UDP and TCP), so members find each
other by name without any static configuration.

## Names

Each member answers to:

| Name               | Example          |
|--------------------|------------------|
| role               | `db`             |
| role + zone        | `db.myapp.anvil` |
| instance name      | `myapp-db`       |

Names are lowercased and any character outside `a-z`, `0-9` and `-` becomes `-`, so a
role `Web_1` resolves as `web-1`. If two names collide, a role wins over an instance
name. `anvil intent info <name>` and the TUI's Intents page show each member's DNS name.

Only A records are served. A name in the zone with no member is `NXDOMAIN`. Any name
outside the zone is forwarded to the nameservers in the host's `/etc/resolv.conf`, so
members still reach the internet by name. Queries are only answered for clients inside
the intent's subnet (and the host itself).

## How members use it

- **VMs** get the gateway as their nameserver and `<intent>.anvil` as their search domain
  through the cloud-init network config.
- **Containers** get the same through Docker's `Dns`/`DnsSearch` options. Docker's own
  resolver still answers container-to-container names first and forwards the rest,
  including VM names, to `anvild`.

The static hosts entries written at launch are still there as a fallback.

## Keeping names up to date

The zone is reloaded whenever a member is launched, removed, or the intent is deleted.
Answers carry a 5 second TTL, so a member added with `anvil intent add` is resolvable by
every existing member within seconds, with no restart. `anvild` also reloads every 30
seconds: this re-reads each container's address from Docker, since a restart can change
it, and retries a listener that failed to bind (for example, a bridge that Docker hadn't
brought up yet after a reboot).

## Requirements and limits

- `anvild` needs `CAP_NET_BIND_SERVICE` to bind port 53. The packaged systemd unit
  grants it.
- A host firewall that drops input from the Docker bridges (for example `ufw` with a
  default deny) also blocks these queries. Allow UDP/TCP 53 from the intent subnets.
- Members launched before this feature keep their old resolver settings. Launching them
  again, or migrating them, picks up the intent DNS.
