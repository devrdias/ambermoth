# ambermoth brand

Flat pixel letters and a pixel icon, drawn as SVG rectangles (`shape-rendering="crispEdges"`), so there are no font files to license or load.

| Asset | Use |
|---|---|
| `banner.svg` | README header (it has its own dark plate, so it works on light and dark GitHub themes) |
| `wordmark-dark.svg`, `wordmark.svg` | AMBERMOTH for dark and light backgrounds |
| `icon.svg` | The mark on a transparent background: a moth preserved in a hexagon of amber |
| `icon-tile.svg`, `icon-512.png` | The mark on a night tile, for avatars and app icons |
| `favicon.svg` | A 16×16 version of the mark, drawn for that size rather than scaled down |
| `social-preview.svg`, `social-preview.png` | GitHub social preview (Settings → Social preview), 1280×640 |

The idea: amber keeps a moth exactly as it was, for millions of years, where anyone can see it. ambermoth does that for your LLM API usage: each call is sealed as it happens and can be inspected later, but not changed. The hexagon is the open-source sticker shape and a nod to resin and honeycomb. The name has no full stop: the moth closes it. The social card is a high-score table of sealed calls.

| Colour | Hex | Role |
|---|---|---|
| Night | `#0B0E1F` | background |
| Amber | `#FFB000` | AMBER, the hexagon, SEALED, calls to action (light backgrounds: `#C27A00`) |
| Amber light | `#FFD166` | hexagon highlight, spots on the wings |
| Amber dark | `#D97E00` | hexagon shadow |
| Resin | `#3B2300` | the moth |
| Cream | `#FFF4D6` | MOTH (light backgrounds: night `#0B0E1F`) |
| Lavender | `#A8B0D6` | taglines and secondary text |

Regenerate the SVGs with `python3 tools/brand/make_brand.py docs/assets` (Python 3, no dependencies). Export the PNGs with `python3 tools/brand/export_png.py docs/assets` (needs Playwright). The evidence pack itself stays plain and sober on purpose, because reviewers read it.
