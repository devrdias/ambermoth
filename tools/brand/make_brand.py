"""ambermoth brand set, clean pixel style: a moth preserved in a hexagon of amber, flat bold pixel letters.
Pure SVG rectangles, no font files. Usage: python3 make_brand.py docs/assets"""

import os, sys
OUT = sys.argv[1]; os.makedirs(OUT, exist_ok=True)


F = {
 'A':"01110 10001 10001 11111 10001 10001 10001",'B':"11110 10001 10001 11110 10001 10001 11110",
 'C':"01110 10001 10000 10000 10000 10001 01110",'D':"11110 10001 10001 10001 10001 10001 11110",
 'E':"11111 10000 10000 11110 10000 10000 11111",'F':"11111 10000 10000 11110 10000 10000 10000",
 'G':"01110 10001 10000 10111 10001 10001 01111",'H':"10001 10001 10001 11111 10001 10001 10001",
 'I':"01110 00100 00100 00100 00100 00100 01110",'J':"00111 00010 00010 00010 00010 10010 01100",
 'K':"10001 10010 10100 11000 10100 10010 10001",'L':"10000 10000 10000 10000 10000 10000 11111",
 'M':"10001 11011 10101 10101 10001 10001 10001",'N':"10001 10001 11001 10101 10011 10001 10001",
 'O':"01110 10001 10001 10001 10001 10001 01110",'P':"11110 10001 10001 11110 10000 10000 10000",
 'Q':"01110 10001 10001 10001 10101 10010 01101",'R':"11110 10001 10001 11110 10100 10010 10001",
 'S':"01111 10000 10000 01110 00001 00001 11110",'T':"11111 00100 00100 00100 00100 00100 00100",
 'U':"10001 10001 10001 10001 10001 10001 01110",'V':"10001 10001 10001 10001 10001 01010 00100",
 'W':"10001 10001 10001 10101 10101 10101 01010",'X':"10001 10001 01010 00100 01010 10001 10001",
 'Y':"10001 10001 01010 00100 00100 00100 00100",'Z':"11111 00001 00010 00100 01000 10000 11111",
 '0':"01110 10001 10011 10101 11001 10001 01110",'1':"00100 01100 00100 00100 00100 00100 01110",
 '2':"01110 10001 00001 00010 00100 01000 11111",'3':"11111 00010 00100 00010 00001 10001 01110",
 '4':"00010 00110 01010 10010 11111 00010 00010",'5':"11111 10000 11110 00001 00001 10001 01110",
 '6':"00110 01000 10000 11110 10001 10001 01110",'7':"11111 00001 00010 00100 01000 01000 01000",
 '8':"01110 10001 10001 01110 10001 10001 01110",'9':"01110 10001 10001 01111 00001 00010 01100",
 ' ':"00000 00000 00000 00000 00000 00000 00000",'.':"00000 00000 00000 00000 00000 01100 01100",
 ',':"00000 00000 00000 00000 01100 00100 01000","'":"01100 00100 01000 00000 00000 00000 00000",
 '-':"00000 00000 00000 11111 00000 00000 00000",':':"00000 01100 01100 00000 01100 01100 00000",
 '#':"01010 01010 11111 01010 11111 01010 01010",'!':"00100 00100 00100 00100 00100 00000 00100",
 '/':"00001 00010 00010 00100 01000 01000 10000",'>':"01000 00100 00010 00001 00010 00100 01000",
}
def rects(cells, p, ox, oy):
    """cells: list of (col,row,color). Merge horizontal runs of the same color."""
    out, rows = [], {}
    for c, r, col in cells: rows.setdefault((r, col), []).append(c)
    for (r, col), cs in rows.items():
        cs.sort(); start = prev = cs[0]
        for c in cs[1:] + [None]:
            if c is not None and c == prev + 1: prev = c; continue
            out.append(f'<rect x="{ox+start*p}" y="{oy+r*p}" width="{(prev-start+1)*p}" height="{p}" fill="{col}"/>')
            if c is not None: start = prev = c
    return "".join(out)

def svg(w, h, body, label="ambermoth"):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {w} {h}" width="{w}" height="{h}" '
            f'shape-rendering="crispEdges" role="img" aria-label="{label}">{body}</svg>\n')




import math
NIGHT, CREAM, DIM, LAV = "#0B0E1F", "#FFF4D6", "#7C84B8", "#A8B0D6"
AMBER, AMBER_HI, AMBER_LO, RESIN, AMBER_DEEP = "#FFB000", "#FFD166", "#D97E00", "#3B2300", "#C27A00"

BOLD_M = "1100011 1110111 1111111 1101011 1100011 1100011 1100011"   # 7 wide, so it doesn't read as H
def glyph(ch):
    """Bold glyph: the 5x7 bitmap OR'd with itself shifted one cell right (6x7). M has its own 7x7 form."""
    if ch == "M":
        return {(c, r) for r, row in enumerate(BOLD_M.split()) for c, b in enumerate(row) if b == "1"}, 7
    cells = {(c, r) for r, row in enumerate(F[ch].split()) for c, b in enumerate(row) if b == "1"}
    return cells | {(c + 1, r) for c, r in cells}, 6

def word(s, gap=2):
    cells, x = set(), 0
    for ch in s.upper():
        g, w = glyph(ch)
        cells |= {(c + x, r) for c, r in g}
        x += w + gap
    return cells, x - gap

def small(s, color, p):
    """Thin 5x7 text for taglines."""
    cells, x = [], 0
    for ch in s.upper():
        cells += [(c + x, r, color) for r, row in enumerate(F[ch].split()) for c, b in enumerate(row) if b == "1"]
        x += 6
    return rects(cells, p, 0, 0), (x - 1) * p

def wordmark(amber=AMBER, moth=CREAM):
    """AMBER in amber, MOTH in cream. No full stop: the moth closes the name."""
    a, aw = word("AMBER"); b, bw = word("MOTH")
    return [(c, r, amber) for c, r in a] + [(c + aw + 2, r, moth) for c, r in b], aw + 2 + bw, 7

# the moth: 15x11, X = body/wings, o = a light spot showing through the wing
MOTH = [".....X...X.....", "......X.X......", ".XXX...X...XXX.", "XXXXX.XXX.XXXXX", "XXoXXXXXXXXXoXX",
        ".XXXXXXXXXXXXX.", "..XXXXXXXXXXX..", "...XXX.X.XXX...", "..XoXX.X.XXoX..", "..XXX..X..XXX..", ".......X......."]
TINY = ["..X...X..", "...X.X...", "XXX.X.XXX", "XXXXXXXXX", ".XXXXXXX.", "..XX.XX..", "....X...."]

def inpoly(x, y, pts):
    ins, j = False, len(pts) - 1
    for i in range(len(pts)):
        (xi, yi), (xj, yj) = pts[i], pts[j]
        if (yi > y) != (yj > y) and x < (xj - xi) * (y - yi) / (yj - yi) + xi: ins = not ins
        j = i
    return ins

def hexagon(n, R):
    """Pointy-top hexagon on an n x n grid, shaded: light on the upper-left edges, dark on the lower-right."""
    c0 = n / 2
    pts = [(c0 + R * math.cos(math.radians(-90 + 60 * i)), c0 + R * math.sin(math.radians(-90 + 60 * i))) for i in range(6)]
    inside = {(c, r) for r in range(n) for c in range(n) if inpoly(c + .5, r + .5, pts)}
    out = {}
    for c, r in inside:
        k = AMBER
        if (c - 1, r) not in inside or ((c, r - 1) not in inside and c <= n // 2): k = AMBER_HI
        if (c + 1, r) not in inside or ((c, r + 1) not in inside and c >= n // 2): k = AMBER_LO
        out[(c, r)] = k
    return out

def icon_cells(n=27):
    cells = hexagon(n, 12.6)
    for r, row in enumerate(MOTH):
        for c, ch in enumerate(row):
            if ch == "X": cells[(c + 6, r + 8)] = RESIN
            elif ch == "o": cells[(c + 6, r + 8)] = AMBER_HI
    return [(c, r, k) for (c, r), k in cells.items()]

def favicon_cells():
    cells = hexagon(16, 7.6)
    for r, row in enumerate(TINY):
        for c, ch in enumerate(row):
            if ch == "X": cells[(c + 3, r + 5)] = RESIN
    return [(c, r, k) for (c, r), k in cells.items()]

def save(n, s): open(os.path.join(OUT, n), "w").write(s)

# icons: transparent (the hexagon is the shape); the avatar PNG adds a night tile
icon = icon_cells()
save("icon.svg", svg(27, 27, rects(icon, 1, 0, 0)).replace('width="27" height="27" shape', 'width="256" height="256" shape'))
save("icon-tile.svg", svg(31, 31, f'<rect width="31" height="31" fill="{NIGHT}"/>' + rects(icon, 1, 2, 2)).replace('width="31" height="31" shape', 'width="512" height="512" shape'))
save("favicon.svg", svg(16, 16, rects(favicon_cells(), 1, 0, 0)).replace('width="16" height="16" shape', 'width="32" height="32" shape'))

# wordmarks (transparent): -dark for dark backgrounds, plain for light ones
wm, ww, wh = wordmark()
save("wordmark-dark.svg", svg(ww * 4, wh * 4, rects(wm, 4, 0, 0)))
wml, _, _ = wordmark(AMBER_DEEP, NIGHT)
save("wordmark.svg", svg(ww * 4, wh * 4, rects(wml, 4, 0, 0)))

# lockup: icon + wordmark on one baseline (icon 27 rows; wordmark 7 rows scaled 2x the icon pixel)
def lockup(p, ox, oy, amber=AMBER, moth=CREAM):
    w, ww_, _ = wordmark(amber, moth)
    return rects(icon, p, ox, oy) + rects(w, p * 2, ox + 33 * p, oy + 6 * p), (33 + ww_ * 2) * p

# README banner: own night plate so it reads on light and dark themes
W, H, P = 1280, 280, 6
lk, lw = lockup(P, 0, 0)
tag, tw = small("YOUR LLM API USAGE, PRESERVED AS IT HAPPENED.", LAV, 3)
body = (f'<rect width="{W}" height="{H}" rx="16" fill="{NIGHT}"/>'
        + f'<g transform="translate({(W - lw) // 2},40)">{lk}</g>'
        + f'<g transform="translate({(W - tw) // 2},{224})">{tag}</g>')
save("banner.svg", svg(W, H, body))

# social preview 1280x640: lockup, tagline and a high-score table of sealed calls
W, H, P = 1280, 640, 6
lk, lw = lockup(P, 0, 0)
rows = [("RANK", "SEQ", "TIME UTC", "PURPOSE", "STATUS"),
        ("1ST", "104231", "14:03:09", "EVAL-REGRESSION", "SEALED"),
        ("2ND", "104232", "14:03:10", "EVAL-REGRESSION", "SEALED"),
        ("3RD", "104233", "14:03:11", "PRODUCTION", "SEALED")]
cols = [0, 120, 300, 520, 920]
table = ""
for i, row in enumerate(rows):
    for j, v in enumerate(row):
        color = DIM if i == 0 else (AMBER if j == 4 else CREAM)
        t, _ = small(v, color, 4)
        table += f'<g transform="translate({cols[j]},{i * 46})">{t}</g>'
hs, hw = small("YOUR LLM API USAGE, PRESERVED AS IT HAPPENED", LAV, 4)
ps, pw = small("PRESS START > AMBERMOTH INIT", AMBER, 4)
body = (f'<rect width="{W}" height="{H}" fill="{NIGHT}"/>'
        + f'<g transform="translate({(W - lw) // 2},40)">{lk}</g>'
        + f'<g transform="translate({(W - hw) // 2},{256})">{hs}</g>'
        + f'<g transform="translate(110,{326})">{table}</g>'
        + f'<g transform="translate({(W - pw) // 2},{560})">{ps}</g>')
save("social-preview.svg", svg(W, H, body))
print("ok")
