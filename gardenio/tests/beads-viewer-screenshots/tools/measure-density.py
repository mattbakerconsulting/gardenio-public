#!/usr/bin/env python3
"""Compute objective info-density metrics for the audit harness.

Walks docs/info-density/<iteration>/captures and writes a metrics.json
roll-up. Per gd-yth2uubWfn deliverable contract.
"""
import argparse
import json
import os
import re
import sys
from pathlib import Path

try:
    from PIL import Image
except ImportError:
    print("Pillow required: pip install Pillow", file=sys.stderr)
    sys.exit(2)

# Full-page screenshots can exceed Pillow's default decompression-bomb guard.
# These are trusted local captures, so disable the limit.
Image.MAX_IMAGE_PIXELS = None

try:
    import numpy as np
except ImportError:
    np = None

TAG_RE = re.compile(rb"<[a-zA-Z][^>]*>")
HREF_RE = re.compile(rb'href="([^"]+)"')
DATA_FIELD_RE = re.compile(rb"<(td|dd|li|dt)\b", re.IGNORECASE)
TEXT_STRIP_RE = re.compile(rb"<[^>]+>")


def html_metrics(path: Path) -> dict:
    data = path.read_bytes()
    dom_count = len(TAG_RE.findall(data))
    text_bytes = len(TEXT_STRIP_RE.sub(b"", data))
    unique_links = len(set(HREF_RE.findall(data)))
    data_fields = len(DATA_FIELD_RE.findall(data))
    return {
        "dom_element_count": dom_count,
        "text_byte_count": text_bytes,
        "text_byte_per_dom": round(text_bytes / dom_count, 3) if dom_count else None,
        "unique_links": unique_links,
        "data_fields": data_fields,
    }


def png_metrics(path: Path) -> dict:
    with Image.open(path) as im:
        im = im.convert("RGB")
        w, h = im.size
        total = w * h
        if not total:
            return {"pixel_count": 0, "near_white_ratio": None, "mean_luma": None}
        if np is not None:
            arr = np.asarray(im, dtype=np.uint8)  # (h, w, 3)
            near_white = int(np.all(arr >= 240, axis=2).sum())
            luma = (0.2126 * arr[..., 0] + 0.7152 * arr[..., 1] + 0.0722 * arr[..., 2])
            mean_luma = float(luma.mean()) / 255.0
        else:
            px = im.load()
            near_white = 0
            luma_sum = 0.0
            for y in range(h):
                for x in range(w):
                    r, g, b = px[x, y]
                    if r >= 240 and g >= 240 and b >= 240:
                        near_white += 1
                    luma_sum += 0.2126 * r + 0.7152 * g + 0.0722 * b
            mean_luma = (luma_sum / total) / 255.0
        return {
            "pixel_count": total,
            "near_white_ratio": round(near_white / total, 4),
            "mean_luma": round(mean_luma, 4),
        }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True, help="captures dir (html/, screenshots/)")
    ap.add_argument("--out", required=True, help="output metrics.json path")
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    desktop_dir = cap / "screenshots" / "desktop"
    narrow_dir = cap / "screenshots" / "narrow"

    pages = {}
    slugs = sorted({p.stem for p in html_dir.glob("*.html")} |
                   {p.stem for p in desktop_dir.glob("*.png")} |
                   {p.stem for p in narrow_dir.glob("*.png")})
    for slug in slugs:
        entry = {"html": None, "desktop": None, "narrow": None}
        h = html_dir / f"{slug}.html"
        if h.exists():
            entry["html"] = html_metrics(h)
        for vp_name, vp_dir in (("desktop", desktop_dir), ("narrow", narrow_dir)):
            p = vp_dir / f"{slug}.png"
            if p.exists():
                entry[vp_name] = png_metrics(p)
        pages[slug] = entry

    out = {
        "target_url": args.target_url,
        "iteration": args.iteration,
        "pages": pages,
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out} ({len(pages)} pages)")


if __name__ == "__main__":
    main()
