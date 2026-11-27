"""Export the PNG versions of the brand SVGs with a headless browser.
Usage: python3 export_png.py docs/assets   (needs: pip install playwright && playwright install chromium)"""
import os, sys
from playwright.sync_api import sync_playwright
D = sys.argv[1]
JOBS = [("icon-tile.svg", "icon-512.png", 512, 512), ("social-preview.svg", "social-preview.png", 1280, 640)]
with sync_playwright() as p:
    b = p.chromium.launch(); pg = b.new_page()
    for src, dst, w, h in JOBS:
        pg.set_viewport_size({"width": w, "height": h})
        pg.goto("file://" + os.path.abspath(os.path.join(D, src)))
        pg.screenshot(path=os.path.join(D, dst), clip={"x": 0, "y": 0, "width": w, "height": h})
    b.close()
print("ok")
