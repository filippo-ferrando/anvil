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
- a terminal of about 110x30, set before recording (the cast keeps the size it was
  recorded at)

## Recording

```
docs/demo/record.sh                  # every CLI scene
docs/demo/record.sh 02-snapshot      # one scene
TARGET=user@host docs/demo/record.sh 06-migrate
bash docs/demo/scenes/99-cleanup.sh  # remove what the scenes created
```

Scenes run in order and build on each other (`02-snapshot` uses the `box` VM from
`01-launch`). Each one types its commands with a short delay and then runs them for
real, so the output is genuine. `-i 2` caps idle time at 2 seconds, so image pulls and
cloud-init waits do not make the cast long.

Output: `casts/<scene>.cast` and `gifs/<scene>.gif`.

## The TUI

The TUI is recorded by hand, since it needs key presses:

```
asciinema rec -i 2 docs/demo/casts/tui.cast -c "anvil tui"
python3 docs/demo/cast2gif.py docs/demo/casts/tui.cast docs/demo/gifs/tui.gif
```

Suggested path through it, about 40 seconds:

1. Instances: move over a running VM so the detail panel shows the live gauges.
2. `l` on it for the live log view, then back.
3. `n` to open the launch form, launch a container, watch the progress redraw.
4. Snapshots screen: create one and restore it.
5. Intents screen: open an intent and show its members and DNS names.
6. Instances: `E` to export the selected VM.
7. Migration screen: `d` to scan the network for other hosts.

## Reuse outside the repo

- Blog or site: `<script src="https://asciinema.org/a/<id>.js" id="asciicast-<id>" async></script>`
  after `asciinema upload casts/<scene>.cast`, or self-host the
  [asciinema-player](https://docs.asciinema.org/manual/player/) with the `.cast` file.
- Slides or social media: the GIFs in `gifs/`.
