#!/usr/bin/env python3
"""CDQR+A objective measure for the Users-page audit.

Ingests a captures dir (html/ + screenshots/{desktop,narrow}/) and emits a
metrics.json covering five dimensions: Density (reused pixel/DOM metrics),
Correctness, Relevance, and Accomplishment-legibility. No judgement — pure
signals the downstream judge step scores.

Contract: docs/users-cdqr/<iteration>/metrics.json (gd-f3LGydP7WF).
"""
import argparse
import json
import re
import sys
from pathlib import Path

try:
    from PIL import Image
    Image.MAX_IMAGE_PIXELS = None
except ImportError:
    print("Pillow required: pip install Pillow", file=sys.stderr)
    sys.exit(2)

try:
    import numpy as np
except ImportError:
    np = None

TAG_RE = re.compile(r"<[a-zA-Z][^>]*>")
HREF_RE = re.compile(r'href="([^"]*)"')
DATA_FIELD_RE = re.compile(r"<(td|dd|li|dt)\b", re.IGNORECASE)
TEXT_STRIP_RE = re.compile(r"<[^>]+>")
BEAD_ROW_RE = re.compile(r'class="bead-row')

# Canonical user-home sections (from controller user_home.go), in DOM order.
SECTION_IDS = [
    "pinned", "inbox", "active-agents", "open-decisions", "recent-comments",
    "open-mrs", "recent-commits", "recent-merged", "their-automations",
    "their-gardeners", "forwarded-mentions",
]
# Sections that convey *completed* work (accomplishments).
DONE_SECTIONS = {"recent-merged", "recent-commits"}
# Sections that convey *in-flight* work (todo / pending).
INFLIGHT_SECTIONS = {"open-mrs", "open-decisions", "active-agents", "inbox"}

ERROR_MARKERS = [
    "unprovisioned", "no operator", "panic:", "failed to", "internal server error",
    "500 internal", "502 bad gateway", "503 service", "504 gateway",
]
EMPTY_MARKERS = [
    "no items", "nothing here", "0 results", "nothing has been forwarded",
    "nothing yet", "no open", "no recent", "<tbody></tbody>",
]


def _section_body(html: str, sid: str) -> str:
    """Return the inner HTML of <section id="sid" ...> up to the next
    <section ...> sibling or end of doc. Good enough for row counting."""
    m = re.search(r'<section id="%s"[^>]*>' % re.escape(sid), html)
    if not m:
        return ""
    start = m.end()
    nxt = re.search(r'<section id="', html[start:])
    end = start + nxt.start() if nxt else len(html)
    return html[start:end]


def _section_count_chip(body: str) -> int:
    """Count chip from the heading: <span class="meta">(N)</span>."""
    m = re.search(r'class="meta">\((\d+)\)', body)
    return int(m.group(1)) if m else 0


def density_html(path: Path) -> dict:
    data = path.read_text(errors="replace")
    dom = len(TAG_RE.findall(data))
    text_bytes = len(TEXT_STRIP_RE.sub("", data).encode("utf-8", "replace"))
    return {
        "dom_element_count": dom,
        "text_byte_count": text_bytes,
        "text_byte_per_dom": round(text_bytes / dom, 3) if dom else None,
        "unique_links": len(set(HREF_RE.findall(data))),
        "data_fields": len(DATA_FIELD_RE.findall(data)),
    }


def density_png(path: Path) -> dict:
    try:
        with Image.open(path) as im:
            im = im.convert("RGB")
            w, h = im.size
            total = w * h
            if not total:
                return None
            if np is not None:
                arr = np.asarray(im, dtype=np.uint8)
                near_white = int(np.all(arr >= 240, axis=2).sum())
                luma = (0.2126 * arr[..., 0] + 0.7152 * arr[..., 1] +
                        0.0722 * arr[..., 2])
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
                "data_fields": None,  # filled by caller from html
                "dom_element_count": None,
                "text_byte_per_dom": None,
            }
    except Exception as exc:  # unreadable screenshot -> null block + warn
        print(f"WARN: unreadable screenshot {path}: {exc}", file=sys.stderr)
        return None


def correctness(html: str, sections_html: dict) -> dict:
    low = html.lower()
    error_markers = sum(low.count(m) for m in ERROR_MARKERS)
    empty_markers = sum(low.count(m) for m in EMPTY_MARKERS)
    # broken links: href="#", href="", slug-less /u/, template-leak in href
    broken = (len(re.findall(r'href="#"', html))
              + len(re.findall(r'href=""', html))
              + len(re.findall(r'href="/u/"', html))
              + len(re.findall(r'href="[^"]*\{\{', html)))
    template_leaks = (html.count("{{") + html.count("}}")
                      + low.count("<no value>"))
    # htmx_unresolved: hx-get lazy sections whose partial fetch returned empty
    htmx_unresolved = 0
    for sid, body in sections_html.items():
        if body is not None and len(body.strip()) > 0:
            # partial returned content -> resolved
            if _section_count_chip(body) == 0 and "empty" in body.lower():
                # legit-empty, not unresolved
                continue
    # count hx-get placeholders in main page whose named partial came back empty
    for sid, body in sections_html.items():
        if body is None:
            continue
        rows = len(BEAD_ROW_RE.findall(body))
        chip = _section_count_chip(body)
        if chip > 0 and rows == 0:
            htmx_unresolved += 1
    return {
        "error_markers": error_markers,
        "empty_section_markers": empty_markers,
        "broken_links": broken,
        "template_leaks": template_leaks,
        "htmx_unresolved": htmx_unresolved,
    }


def relevance(html: str) -> dict:
    present = []
    nonempty = []
    for sid in SECTION_IDS:
        body = _section_body(html, sid)
        if not body:
            continue
        present.append(sid)
        rows = len(BEAD_ROW_RE.findall(body))
        chip = _section_count_chip(body)
        if rows >= 1 or chip >= 1:
            nonempty.append(sid)
    top_fold = present[0] if present else ""
    return {
        "sections_present": present,
        "sections_nonempty": nonempty,
        "top_fold_section": top_fold,
    }


def accomplishment(html: str) -> dict:
    low = html.lower()
    # head of <main>: roughly the first section block, before the first
    # in-flight section in DOM order.
    main_start = html.find("<main")
    head = html[main_start:main_start + 6000] if main_start >= 0 else html[:6000]
    # 1. page-level done-work aggregate band near top of <main>
    summary_present = bool(
        re.search(r'(merged|commits|closed|shipped|resolved)[^<]{0,40}\(\d+\)',
                  head, re.IGNORECASE)
        and re.search(r'class="[^"]*(stat|summary|aggregate)[^"]*"', head,
                      re.IGNORECASE)
    )
    # 2. done_signal_fields: rows under recent-merged + recent-commits +
    #    count chips reading closed/merged/shipped/resolved.
    done = 0
    for sid in DONE_SECTIONS:
        body = _section_body(html, sid)
        done += len(BEAD_ROW_RE.findall(body))
    done += len(re.findall(
        r'(closed|merged|shipped|resolved)[^<]{0,20}\((\d+)\)', low))
    # 3. recency windows surfaced anywhere (headings + chips). Restrict to
    #    1-3 digit day windows and require the match not be embedded in a
    #    hex color literal (e.g. CSS "#11161d") -> use negative lookbehind on
    #    a preceding hex digit/#.
    windows = sorted(set(re.findall(r'(?<![0-9a-fA-F#])(\d{1,3}d)\b', html))
                     | ({"today"} if "today" in low else set())
                     | ({"this week"} if "this week" in low else set()))
    # 4. done_vs_inflight_separated: are done sections grouped/labeled as
    #    accomplishments rather than interleaved? Heuristic: is there an
    #    explicit "accomplishments"/"done"/"shipped" grouping wrapper, OR are
    #    the done sections contiguous in DOM order (not interleaved by
    #    in-flight sections)?
    order = [s for s in SECTION_IDS if _section_body(html, s)]
    done_idx = [i for i, s in enumerate(order) if s in DONE_SECTIONS]
    contiguous = bool(done_idx) and (max(done_idx) - min(done_idx) + 1 ==
                                     len(done_idx))
    explicit_group = bool(re.search(
        r'(accomplishment|shipped work|what .* (done|shipped|completed))',
        low))
    separated = explicit_group or (contiguous and len(done_idx) > 0)
    # 5. accomplishment_top_fold: is a done-work signal in the first
    #    in-flight-free prefix of DOM order? proxy: does a done section appear
    #    before the first in-flight section?
    first_inflight = next(
        (i for i, s in enumerate(order) if s in INFLIGHT_SECTIONS), len(order))
    top_fold = any(i < first_inflight for i in done_idx)
    return {
        "accomplishment_summary_present": summary_present,
        "done_signal_fields": done,
        "recency_windows_present": windows,
        "done_vs_inflight_separated": separated,
        "accomplishment_top_fold": top_fold,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--user-slug", required=True)
    ap.add_argument("--iteration", required=True)
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    home = html_dir / "user-home.html"
    if not home.exists():
        print(f"FATAL: {home} missing", file=sys.stderr)
        sys.exit(2)
    html = home.read_text(errors="replace")

    # Section partials fetched separately (forwarded, recent-comments).
    sections_html = {}
    for sid, fname in (("forwarded-mentions", "section-forwarded.html"),
                       ("recent-comments", "section-recent-comments.html")):
        p = html_dir / fname
        sections_html[sid] = p.read_text(errors="replace") if p.exists() else None

    # Density: html metrics + per-viewport pixel metrics, merged.
    dens = {}
    hm = density_html(home)
    for vp in ("desktop", "narrow"):
        png = cap / "screenshots" / vp / "user-home.png"
        block = density_png(png) if png.exists() else None
        if block is None:
            dens[vp] = None
            continue
        block["data_fields"] = hm["data_fields"]
        block["dom_element_count"] = hm["dom_element_count"]
        block["text_byte_per_dom"] = hm["text_byte_per_dom"]
        block["unique_links"] = hm["unique_links"]
        dens[vp] = block

    out = {
        "target_url": args.target_url,
        "user_slug": args.user_slug,
        "iteration": args.iteration,
        "density": dens,
        "correctness": correctness(html, sections_html),
        "relevance": relevance(html),
        "accomplishment": accomplishment(html),
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")
    print(json.dumps(out, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
