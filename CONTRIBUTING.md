# Contributing

## Setup

Go 1.27+, `protoc`, and on Linux `qemu-system-x86_64`, `qemu-img`, `xorriso` and
`virtiofsd`. Docker is only needed for the container backend and its tests.

```
git clone --recurse-submodules https://github.com/filippo-ferrando/anvil.git
make proto build
make test vet fmt
```

Integration tests that need QEMU or Docker skip themselves when the tool is missing, so
`make test` passes on a bare machine. Run them with the tools installed before sending
a change to a backend.

## Where code goes

- The daemon owns all logic. `internal/cli/commands` and `internal/tui` only build a
  request, call the daemon and show the reply. Logic in a client drifts between the two.
- New API surface starts in `api/proto/anvil/v1/anvil.proto`, then `make proto`, then the
  matching `internal/daemon/*_server.go`. `api/gen` is generated, never edited by hand.
- A capability only one backend has (snapshots, mounts, ...) is a small interface in
  `internal/instance/backend.go`, not a type switch in the daemon.
- A user-facing feature gets a CLI command, a TUI entry point where it makes sense, and
  a test.

[docs/architecture.md](docs/architecture.md) explains the layout and the reasons behind it.

## Style

- `gofmt -s`, `go vet` clean.
- Comments and docs: plain language, at most two lines per comment, no em-dashes, no
  "we", "you" or "I".
- Commit subjects follow `type: summary` (`feat:`, `fix:`, `docs:`, ...); release notes and
  `CHANGELOG.md` are built from them by `scripts/release-notes.sh` and `scripts/changelog.sh`.

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Reporting bugs

The bug report template asks for the output of `anvil version`, the host distro and
kernel, the command, and the relevant part of `journalctl -u anvild`. Security issues go through
[SECURITY.md](SECURITY.md) instead.
