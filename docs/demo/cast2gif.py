#!/usr/bin/env python3
"""Render an asciinema cast (v2 or v3) to a GIF with Pillow and ffmpeg, no agg needed.

Usage: cast2gif.py in.cast out.gif [--font-size 16] [--fps 15] [--font PATH]
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import unicodedata

from PIL import Image, ImageDraw, ImageFont

# One Dark-like theme: 16 ANSI colors, then default foreground and background.
PALETTE = [
    (40, 44, 52), (224, 108, 117), (152, 195, 121), (229, 192, 123),
    (97, 175, 239), (198, 120, 221), (86, 182, 194), (171, 178, 191),
    (92, 99, 112), (240, 128, 136), (172, 215, 141), (240, 208, 140),
    (117, 190, 255), (218, 140, 241), (106, 202, 214), (220, 223, 228),
]
FG, BG = (220, 223, 228), (30, 33, 39)


def color256(n):
    if n < 16:
        return PALETTE[n]
    if n < 232:
        n -= 16
        steps = [0, 95, 135, 175, 215, 255]
        return steps[n // 36], steps[(n // 6) % 6], steps[n % 6]
    v = 8 + (n - 232) * 10
    return v, v, v


class Style:
    __slots__ = ("fg", "bg", "bold", "inverse")

    def __init__(self, fg=None, bg=None, bold=False, inverse=False):
        self.fg, self.bg, self.bold, self.inverse = fg, bg, bold, inverse

    def copy(self):
        return Style(self.fg, self.bg, self.bold, self.inverse)

    def key(self):
        return self.fg, self.bg, self.bold, self.inverse


BLANK = Style()

# Box-drawing characters as the cell edges their lines reach (l, r, u, d) and their weight.
# They are drawn as shapes, since many fonts leave gaps between cells.
BOX = {}
for chars, edges, weight in [
    ("─╌┄┈", "lr", 1), ("│╎┆┊", "ud", 1), ("┌╭", "rd", 1), ("┐╮", "ld", 1), ("└╰", "ru", 1),
    ("┘╯", "lu", 1), ("├", "udr", 1), ("┤", "udl", 1), ("┬", "lrd", 1), ("┴", "lru", 1), ("┼", "lrud", 1),
    ("━╍┅┉═", "lr", 2), ("┃╏┇┋║", "ud", 2), ("┏╔", "rd", 2), ("┓╗", "ld", 2), ("┗╚", "ru", 2),
    ("┛╝", "lu", 2), ("┣╠", "udr", 2), ("┫╣", "udl", 2), ("┳╦", "lrd", 2), ("┻╩", "lru", 2), ("╋╬", "lrud", 2),
]:
    for c in chars:
        BOX[c] = (edges, weight)


class Screen:
    """A small VT100/xterm subset: enough for shell output, progress lines and a Bubble Tea TUI."""

    def __init__(self, cols, rows):
        self.cols, self.rows = cols, rows
        self.main = self._grid()
        self.alt = None
        self.grid = self.main
        self.x = self.y = 0
        self.wrap_pending = False
        self.style = Style()
        self.saved = (0, 0, Style())
        self.top, self.bottom = 0, rows - 1
        self.cursor_visible = True
        self.state, self.buf = "text", ""
        self.dirty = True
        self.used_cols = self.used_rows = 0  # furthest cell ever written or reached, for cropping

    def _grid(self):
        return [self._line() for _ in range(self.rows)]

    def _line(self):
        return [[" ", BLANK] for _ in range(self.cols)]

    # -- input --------------------------------------------------------------
    def feed(self, data):
        self.dirty = True
        for ch in data:
            st = self.state
            if st == "text":
                if ch == "\x1b":
                    self.state = "esc"
                elif ch < " " or ch == "\x7f":
                    self.control(ch)
                else:
                    self.put(ch)
            elif st == "esc":
                self.escape(ch)
            elif st == "csi":
                self.buf += ch
                if "\x40" <= ch <= "\x7e":
                    self.csi(self.buf)
                    self.state, self.buf = "text", ""
            elif st == "str":  # OSC, DCS: skipped until BEL or ESC \
                if ch == "\x07":
                    self.state = "text"
                elif ch == "\x1b":
                    self.state = "str_esc"
            elif st == "str_esc":
                self.state = "text" if ch == "\\" else "str"
            elif st == "charset":
                self.state = "text"

    def control(self, ch):
        if ch == "\n" or ch == "\x0b" or ch == "\x0c":
            self.linefeed()
        elif ch == "\r":
            self.x, self.wrap_pending = 0, False
        elif ch == "\b":
            self.x, self.wrap_pending = max(0, self.x - 1), False
        elif ch == "\t":
            self.x = min(self.cols - 1, (self.x // 8 + 1) * 8)

    def escape(self, ch):
        self.state = "text"
        if ch == "[":
            self.state, self.buf = "csi", ""
        elif ch in "]PX^_":
            self.state = "str"
        elif ch in "()*+":
            self.state = "charset"
        elif ch == "7":
            self.saved = (self.x, self.y, self.style.copy())
        elif ch == "8":
            self.x, self.y, s = self.saved
            self.style = s.copy()
        elif ch == "D":
            self.linefeed()
        elif ch == "E":
            self.x = 0
            self.linefeed()
        elif ch == "M":
            if self.y == self.top:
                self.scroll_down(1)
            else:
                self.y = max(0, self.y - 1)
        elif ch == "c":
            self.__init__(self.cols, self.rows)

    def put(self, ch):
        width = 2 if unicodedata.east_asian_width(ch) in "WF" else 1
        if unicodedata.combining(ch):
            return
        if self.wrap_pending or self.x + width > self.cols:
            self.x = 0
            self.linefeed()
        self.wrap_pending = False
        row = self.grid[self.y]
        row[self.x] = [ch, self.style.copy()]
        if ch != " ":
            self.used_cols = max(self.used_cols, self.x + width)
            self.used_rows = max(self.used_rows, self.y + 1)
        if width == 2 and self.x + 1 < self.cols:
            row[self.x + 1] = ["", self.style.copy()]
        self.x += width
        if self.x >= self.cols:
            self.x, self.wrap_pending = self.cols - 1, True

    def linefeed(self):
        self.wrap_pending = False
        if self.y == self.bottom:
            self.scroll_up(1)
        elif self.y < self.rows - 1:
            self.y += 1

    def scroll_up(self, n):
        for _ in range(n):
            del self.grid[self.top]
            self.grid.insert(self.bottom, self._line())

    def scroll_down(self, n):
        for _ in range(n):
            del self.grid[self.bottom]
            self.grid.insert(self.top, self._line())

    def erase(self, row, start, end):
        for i in range(max(0, start), min(self.cols, end)):
            self.grid[row][i] = [" ", Style(bg=self.style.bg)]

    def csi(self, seq):
        final, body = seq[-1], seq[:-1]
        private = body[:1] in ("?", ">", "<", "=")
        if private:
            body = body[1:]
        body = body.rstrip(" !\"#$%&'")
        params = [int(p) if p.isdigit() else 0 for p in body.replace(":", ";").split(";")] if body else []

        def p(i, default=1):
            return params[i] if len(params) > i and params[i] else default

        if private:
            if final in "hl":
                for mode in params:
                    self.mode(mode, final == "h")
            return
        if final == "m":
            self.sgr(params or [0])
            return
        self.wrap_pending = False
        if final == "A":
            self.y = max(0, self.y - p(0))
        elif final in "Be":
            self.y = min(self.rows - 1, self.y + p(0))
        elif final in "Ca":
            self.x = min(self.cols - 1, self.x + p(0))
        elif final == "D":
            self.x = max(0, self.x - p(0))
        elif final == "E":
            self.x, self.y = 0, min(self.rows - 1, self.y + p(0))
        elif final == "F":
            self.x, self.y = 0, max(0, self.y - p(0))
        elif final in "G`":
            self.x = min(self.cols - 1, p(0) - 1)
        elif final == "d":
            self.y = min(self.rows - 1, p(0) - 1)
        elif final in "Hf":
            self.y, self.x = min(self.rows - 1, p(0) - 1), min(self.cols - 1, p(1) - 1)
        elif final == "J":
            mode = p(0, 0)
            if mode == 0:
                self.erase(self.y, self.x, self.cols)
                for r in range(self.y + 1, self.rows):
                    self.erase(r, 0, self.cols)
            elif mode == 1:
                self.erase(self.y, 0, self.x + 1)
                for r in range(self.y):
                    self.erase(r, 0, self.cols)
            else:
                for r in range(self.rows):
                    self.erase(r, 0, self.cols)
        elif final == "K":
            mode = p(0, 0)
            start, end = {0: (self.x, self.cols), 1: (0, self.x + 1)}.get(mode, (0, self.cols))
            self.erase(self.y, start, end)
        elif final == "X":
            self.erase(self.y, self.x, self.x + p(0))
        elif final == "P":
            row = self.grid[self.y]
            n = min(p(0), self.cols - self.x)
            del row[self.x:self.x + n]
            row.extend([" ", Style(bg=self.style.bg)] for _ in range(n))
        elif final == "@":
            row = self.grid[self.y]
            n = min(p(0), self.cols - self.x)
            for _ in range(n):
                row.insert(self.x, [" ", Style(bg=self.style.bg)])
            del row[self.cols:]
        elif final in "LM" and self.top <= self.y <= self.bottom:
            top, self.top = self.top, self.y
            (self.scroll_down if final == "L" else self.scroll_up)(p(0))
            self.top = top
        elif final == "S":
            self.scroll_up(p(0))
        elif final == "T":
            self.scroll_down(p(0))
        elif final == "r":
            self.top, self.bottom = p(0) - 1, min(self.rows, p(1, self.rows)) - 1
            self.x = self.y = 0
        elif final == "s":
            self.saved = (self.x, self.y, self.style.copy())
        elif final == "u":
            self.x, self.y, s = self.saved
            self.style = s.copy()

    def mode(self, mode, on):
        if mode == 25:
            self.cursor_visible = on
        elif mode in (47, 1047, 1049):
            if on and self.alt is None:
                if mode == 1049:
                    self.saved = (self.x, self.y, self.style.copy())
                self.alt = self._grid()
                self.grid = self.alt
            elif not on and self.alt is not None:
                self.alt, self.grid = None, self.main
                if mode == 1049:
                    self.x, self.y, s = self.saved
                    self.style = s.copy()

    def sgr(self, params):
        s, i = self.style, 0
        while i < len(params):
            n = params[i]
            if n == 0:
                self.style = s = Style()
            elif n == 1:
                s.bold = True
            elif n == 22:
                s.bold = False
            elif n == 7:
                s.inverse = True
            elif n == 27:
                s.inverse = False
            elif 30 <= n <= 37:
                s.fg = PALETTE[n - 30]
            elif 90 <= n <= 97:
                s.fg = PALETTE[n - 90 + 8]
            elif 40 <= n <= 47:
                s.bg = PALETTE[n - 40]
            elif 100 <= n <= 107:
                s.bg = PALETTE[n - 100 + 8]
            elif n == 39:
                s.fg = None
            elif n == 49:
                s.bg = None
            elif n in (38, 48) and i + 1 < len(params):
                if params[i + 1] == 5 and i + 2 < len(params):
                    c, i = color256(params[i + 2]), i + 2
                elif params[i + 1] == 2 and i + 4 < len(params):
                    c, i = tuple(params[i + 2:i + 5]), i + 4
                else:
                    c = None
                if n == 38:
                    s.fg = c
                else:
                    s.bg = c
            i += 1


class Renderer:
    def __init__(self, cols, rows, font_path, bold_path, size):
        self.cols, self.rows, self.size = cols, rows, size
        self.font = ImageFont.truetype(font_path, size)
        self.bold = ImageFont.truetype(bold_path, size) if bold_path else self.font
        ascent, descent = self.font.getmetrics()
        self.cw = round(self.font.getlength("M"))
        # The font's own line height, like a terminal: box-drawing borders then join up.
        self.ch = ascent + descent
        self.ascent_pad = 0
        self.notdef = {}  # font -> mask bytes of its "missing glyph" box
        self.fallback = {}  # (font, char) -> font that has the glyph
        self.pad = self.cw * 2
        self.width = cols * self.cw + 2 * self.pad
        self.height = rows * self.ch + 2 * self.pad
        self.width += self.width % 2  # even sizes keep every encoder happy
        self.height += self.height % 2

    def render(self, screen):
        img = Image.new("RGB", (self.width, self.height), BG)
        draw = ImageDraw.Draw(img)
        for r, row in enumerate(screen.grid[:self.rows]):
            row = row[:self.cols]
            y = self.pad + r * self.ch
            c = 0
            while c < len(row):
                style = row[c][1]
                start, text = c, ""
                while c < len(row) and row[c][1].key() == style.key():
                    text += row[c][0] or ""
                    c += 1
                fg, bg = style.fg or FG, style.bg or BG
                if style.inverse:
                    fg, bg = bg, fg
                x = self.pad + start * self.cw
                if bg != BG:
                    draw.rectangle([x, y, x + (c - start) * self.cw - 1, y + self.ch - 1], fill=bg)
                if text.strip():
                    # Drawn cell by cell so fallback glyphs never shift the columns after them.
                    font = self.bold if style.bold else self.font
                    for i, cell in enumerate(row[start:c]):
                        if self.draw_shape(draw, x + i * self.cw, y, cell[0], fg, bg):
                            continue
                        if cell[0].strip():
                            f = self.font_for(font, cell[0])
                            draw.text((x + i * self.cw, y + self.ascent_pad), cell[0], font=f, fill=fg)
        if screen.cursor_visible and screen.alt is None:
            x, y = self.pad + screen.x * self.cw, self.pad + screen.y * self.ch
            if screen.x < self.cols and screen.y < self.rows:
                draw.rectangle([x, y, x + self.cw - 1, y + self.ch - 1], outline=FG)
        return img.tobytes()

    def draw_shape(self, draw, x, y, ch, fg, bg):
        """Draws box-drawing and block characters as shapes; False for anything else."""
        w, h = self.cw, self.ch
        if len(ch) != 1:  # the right half of a wide character
            return False
        if ch in BOX:
            edges, weight = BOX[ch]
            t = max(1, round(w / 9)) * weight
            cx, cy = x + (w - t) // 2, y + (h - t) // 2
            if "l" in edges:
                draw.rectangle([x, cy, cx + t - 1, cy + t - 1], fill=fg)
            if "r" in edges:
                draw.rectangle([cx, cy, x + w - 1, cy + t - 1], fill=fg)
            if "u" in edges:
                draw.rectangle([cx, y, cx + t - 1, cy + t - 1], fill=fg)
            if "d" in edges:
                draw.rectangle([cx, cy, cx + t - 1, y + h - 1], fill=fg)
            return True
        o = ord(ch)
        if 0x2581 <= o <= 0x2588:  # lower n/8 blocks, up to the full block
            top = y + h - round(h * (o - 0x2580) / 8)
            draw.rectangle([x, top, x + w - 1, y + h - 1], fill=fg)
            return True
        if 0x2589 <= o <= 0x258F:  # left n/8 blocks
            draw.rectangle([x, y, x + round(w * (0x2590 - o) / 8) - 1, y + h - 1], fill=fg)
            return True
        if ch in "▀▐▔▕":
            box = {"▀": [x, y, x + w - 1, y + h // 2 - 1], "▐": [x + w // 2, y, x + w - 1, y + h - 1],
                   "▔": [x, y, x + w - 1, y + h // 8], "▕": [x + w - w // 8 - 1, y, x + w - 1, y + h - 1]}[ch]
            draw.rectangle(box, fill=fg)
            return True
        if ch in "░▒▓":
            a = {"░": 0.25, "▒": 0.5, "▓": 0.75}[ch]
            draw.rectangle([x, y, x + w - 1, y + h - 1], fill=tuple(round(f * a + b * (1 - a)) for f, b in zip(fg, bg)))
            return True
        return False

    def font_for(self, font, ch):
        """Returns font, or a fontconfig fallback when font has no glyph for ch."""
        key = (font, ch)
        if key in self.fallback:
            return self.fallback[key]
        if font not in self.notdef:
            self.notdef[font] = bytes(font.getmask("\U0010fffd"))
        chosen = font
        if ord(ch) > 127 and bytes(font.getmask(ch)) == self.notdef[font]:
            path = font_file(f"monospace:charset={ord(ch):x}")
            if path:
                try:
                    chosen = ImageFont.truetype(path, self.size)
                except OSError:
                    pass
        self.fallback[key] = chosen
        return chosen


def read_cast(path):
    with open(path, encoding="utf-8") as f:
        header = json.loads(f.readline())
        events, t = [], 0.0
        v3 = header.get("version") == 3
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            time, kind, data = json.loads(line)[:3]
            t = t + time if v3 else time
            if kind == "o":
                events.append((t, data))
    if v3:
        cols, rows = header["term"]["cols"], header["term"]["rows"]
    else:
        cols, rows = header["width"], header["height"]
    return cols, rows, events


def font_file(pattern):
    try:
        return subprocess.run(["fc-match", "-f", "%{file}", pattern], capture_output=True, text=True, check=True).stdout
    except (OSError, subprocess.CalledProcessError):
        return None


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("cast")
    ap.add_argument("gif")
    ap.add_argument("--font-size", type=int, default=16)
    ap.add_argument("--fps", type=int, default=15)
    ap.add_argument("--font", help="monospace TTF/OTF file, defaults to fontconfig's monospace")
    ap.add_argument("--hold", type=float, default=2.0, help="seconds to keep the last frame")
    ap.add_argument("--no-crop", action="store_true", help="keep the whole recorded terminal, empty space included")
    args = ap.parse_args()

    font = args.font or font_file("monospace")
    if not font:
        sys.exit("cast2gif: no monospace font found, pass --font")
    bold = None if args.font else font_file("monospace:bold")

    cols, rows, events = read_cast(args.cast)
    view_cols, view_rows = cols, rows
    if not args.no_crop:
        # A big terminal leaves most of each frame empty: keep the part the cast ever used.
        probe = Screen(cols, rows)
        for _, data in events:
            probe.feed(data)
            probe.used_rows = max(probe.used_rows, probe.y + 1)
        view_cols, view_rows = min(cols, probe.used_cols + 1), min(rows, probe.used_rows)
    screen = Screen(cols, rows)
    renderer = Renderer(view_cols, view_rows, font, bold, args.font_size)
    end = (events[-1][0] if events else 0) + args.hold
    size = f"{renderer.width}x{renderer.height}"

    with tempfile.TemporaryDirectory() as tmp:
        # Frames go to a lossless file first: palettegen needs to see all of them.
        video, palette = os.path.join(tmp, "frames.mkv"), os.path.join(tmp, "palette.png")
        enc = subprocess.Popen(
            ["ffmpeg", "-loglevel", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", size,
             "-r", str(args.fps), "-i", "-", "-c:v", "ffv1", video],
            stdin=subprocess.PIPE)
        frame, i, n = None, 0, int(end * args.fps) + 1
        for k in range(n):
            t = k / args.fps
            while i < len(events) and events[i][0] <= t:
                screen.feed(events[i][1])
                i += 1
            if screen.dirty or frame is None:
                frame, screen.dirty = renderer.render(screen), False
            enc.stdin.write(frame)
        enc.stdin.close()
        if enc.wait() != 0:
            sys.exit("cast2gif: ffmpeg failed writing frames")
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", video,
                        "-vf", "palettegen=stats_mode=full:reserve_transparent=0", palette], check=True)
        # Repeated frames stay: diff_mode stores each as a tiny update, and dropping them
        # would also drop the hold on the last frame, which nothing after it would time.
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", video, "-i", palette,
                        "-lavfi", "paletteuse=dither=none:diff_mode=rectangle", "-loop", "0", args.gif], check=True)


if __name__ == "__main__":
    main()
