# Changelog

Built from commit subjects by `scripts/changelog.sh`.

## v1.2.7 (2026-10-26)

### Features

- USB/PCI device passthrough (3ae82e0)

### Fixes

- fixed rpm spec file (bfd8e64)

## v1.2.6 (2026-10-20)

### Fixes

- test peaked at 6Gb of ram, OOM CI (497fe72)
- timing problem on `process.go` (4e7421a)
- closed gaps (c14af22)
- fix for migrate/export/ecc (12ffc10)
- fix on high impact (1ca787c)

## v1.2.5 (2026-10-19)

### Fixes

- improve image list handling (d6be391)

## v1.2.4 (2026-10-19)

### Features

- full firecracker integration (test version) (0c1ee19)
- firecracker implementation (phase 7) (64129df)

### Fixes

- jailer problems (dd5a6e2)

## v1.2.3 (2026-10-17)

### Features

- graphical enhancement (f415849)

### Fixes

- phase 2 for docs (added asciinema gifs) (038c5b8)

## v1.2.2 (2026-10-16)

### Fixes

- `fork` created a too long name -> qmp fails (525cb7f)
- security fixes and phase 1 for docs (missing gifs) (4fb89da)

### Uncategorized

- updated documentation (00b0db7)

## v1.2.1 (2026-10-15)

### Fixes

- improved TUI and gui export (vm) (f44ffba)

## v1.2.0 (2026-10-14)

### Features

- introduced first GUI export test (e511d87)

## v1.1.1 (2026-10-13)

### Fixes

- fixed preflight errors (a12bb91)
- removed old folder (7468b48)
- remote tui connection is broken (2c3bc23)
- reference the correct repo (1aef05b)
- fixed submodule (425e8ac)
- cloudinit error codes (954b45d)
- improve cli commands and removed old integration (92329de)

## v1.1.0 (2026-10-09)

### Features

- prometheus metrics and remote anvild control (via ssh) (54c681a)

## v1.0.10 (2026-10-05)

### Features

- migration enhancements (e1ea010)

### Fixes

- ovmf search was borked (0d90ffc)

### Other changes

- refactore code (46603d0)

## v1.0.9 (2026-09-30)

### Features

- declarative intents (1a5f318)

### Fixes

- anvil shared folder creation fix (555c25b)

## v1.0.8 (2026-09-29)

### Features

- finished implementation of the VM related improvements (05dc066)
- new imrpovements for vm lifecycle (08f7b18)
- new performance updates (9b6bca7)

## v1.0.7 (2026-09-22)

### Features

- dns dynamic resolution in intent network (7b91925)
- added man pages (#12) (72f92d2)

### Other changes

- added screenshots to readme (e7096a1)

## v1.0.6 (2026-09-21)

### Fixes

- Makefile works without exporting anything to PATH (e63eb04)

### Other changes

- not the best way of handle hash (a32393f)
- improved docs and added `fork` feature (7f34d8e)

## v1.0.5 (2026-09-17)

### Fixes

- arch is inferred from image arch now (from catalog) (fd38c93)

## v1.0.4 (2026-09-17)

### Fixes

- don't remove automatically anvil user and group (c570a13)
- OVMF path dynamically found (0c4d2dc)

### Other changes

- updated neovim plugin (84d51a6)

## v1.0.3 (2026-09-17)

### Features

- added snapshot support (#5) (2142087)
- add or remove port from container/vms (#7) (3b24efc)
- Cloudinit config wizard and Import/Export (a05228e)

## v1.0.2 (2026-09-16)

### Features

- anvil neovim integration (05480dc)

## v1.0.1 (2026-09-16)

### Features

- baseline for Apple VZ implementation (7e850f5)

## v1.0.0 (2026-09-16)

### Features

- packaging actions (487e74b)
- autocompletions (7fcb99e)
- added features on instance launch (d63d69e)
- added container image visibility (227b6eb)
- remote catalogue (0c2b4c8)
- implemented M7, M8 (ea852f8)
- implemented M7, M8 (4176878)
- new QoL automations (599880f)
- Phase M3, M4 (852d3a1)
- first feature set (c01f5e0)

### Fixes

- network not clean after vm/container migration or delete/purge (3e8e2a6)
- prepared PKGBUILD (e683aa2)
- fixing mirror doc (035299e)
- cloudinit page (3f7e8ee)
- catalogue on public repo (a8246b4)
- missing proto def (e87611d)
- bug fix + clouinit features (d5d4400)
- various fixes over ssh (7c49d0c)
- new ui features (7b23fc3)
- try to fix TUI (5120d50)
- missings deps (98082e7)
- moved to bubbletea (d5dbec6)
- regen proto stubs (67521e9)
- fixed gitignore (ca54562)
- added env in makefile build (8228ca8)
- removed executables (0d39f02)

### Other changes

- clean doc (21c2bbd)
- prepare for public release (5ca8063)
- added license (6cb441c)

### Uncategorized

- PKGBUILD for arch (161701c)

