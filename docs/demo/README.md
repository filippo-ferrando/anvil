# Demo recordings

The GIFs in the README are rendered from [asciinema](https://asciinema.org) casts, so the
same recording can be embedded as a GIF (README, forge) or as a live, copyable player
(blog, asciinema.org).

## Requirements

- `asciinema` 2.x or 3.x, `ffmpeg`, and Python 3 with Pillow. `cast2gif.py` turns a cast into
  a GIF: it replays the cast through a small terminal emulator, draws each frame with the
  system's monospace font (falling back through fontconfig for missing glyphs) and encodes
  them with ffmpeg
- a running `anvild` and the caller in the `anvil` group
- for `06-firecracker`, `07-ephemeral` and `08-pipeline`: Firecracker with its jailer
  (see [../firecracker.md](../firecracker.md) and [../ephemeral.md](../ephemeral.md))
- any terminal of at least 110x30: `record.sh` records every cast at 110x30 whatever the
  terminal size is (`DEMO_SIZE=120x34` picks another); a smaller terminal still records
  correct casts, but live progress lines wrap and pile up on screen while a scene runs

## Recording

```
docs/demo/record.sh                  # every CLI scene
docs/demo/record.sh 02-snapshot      # one scene
TARGET=user@host docs/demo/record.sh 10-migrate
bash docs/demo/scenes/99-cleanup.sh  # remove what the scenes created
```

Scenes run in order and build on each other (`02-snapshot` uses the `box` VM from
`01-launch`). Each one types its commands with a short delay and then runs them for
real, so the output is genuine. `-i 2` caps idle time at 2 seconds, so image pulls and
cloud-init waits do not make the cast long.

A scene can have a `<scene>.prep.sh` next to it, run before recording and kept off the
cast: `05-export.prep.sh` makes the export directory with `anvil create-dir` (anvild only
writes into a directory the caller owns and the `anvil` user can reach, so not straight
into `/tmp`), `07-ephemeral.prep.sh` builds the warm snapshots, so the cast shows warm starts and
not a first boot. `ci/` is the small project that `07-ephemeral` and `08-pipeline` upload,
with its own `anvil-ci.yaml`.

Output: `casts/<scene>.cast` and `gifs/<scene>.gif`.

## Reuse outside the repo

- Blog or site: `<script src="https://asciinema.org/a/<id>.js" id="asciicast-<id>" async></script>`
  after `asciinema upload casts/<scene>.cast`, or self-host the
  [asciinema-player](https://docs.asciinema.org/manual/player/) with the `.cast` file.
- Slides or social media: the GIFs in `gifs/`.
