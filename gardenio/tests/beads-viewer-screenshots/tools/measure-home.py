#!/usr/bin/env python3
"""Objective structural measure for the Home-beauty CLUB audit.

Ingests a captures dir (html/ + screenshots/) for the beads-viewer Home
"Live agents" grid and emits metrics.json per the Home-beauty step-3
contract (molecule gd-1E4l0qbduj, epic gd-qipN5RP8Do). No judgement — pure
counts. The judge step reads the PNGs and these numbers together.

Signals grep the post-render DOM (page.content()), NOT the raw HTTP body, so
htmx-loaded partials are counted. CSS lives in a <style> block that mentions
the class names too, so every detector scans only the markup *after* the last
</style> to avoid double-counting stylesheet rules.

Beauty/near-white numbers are folded in from an already-computed density.json
(measure-density.py) when --density is passed.

Note on the target surface: as of decision ka-0000004036 the activity feed is
the homepage `/` (handleFeed) and the Live-agents grid (handleIndex) moved to
`/index`. This tool measures whichever HTML slug is named by --page-slug
(default `index-livegrid`, i.e. the captured `/index` grid).
"""
import argparse
import json
import re
import sys
from pathlib import Path

BARE_ID_RE = re.compile(r"^\s*(gd|ka)-[A-Za-z0-9]+\s*$")
# Visible text of an <a class="... index-active-card-title ...">TEXT</a>.
TITLE_ANCHOR_RE = re.compile(
    r'<a\b[^>]*class="[^"]*index-active-card-title[^"]*"[^>]*>(.*?)</a>',
    re.IGNORECASE | re.DOTALL,
)
TAG_STRIP_RE = re.compile(r"<[^>]+>")
HREF_RE = re.compile(r'href="([^"]*)"')


def body_only(html: str) -> str:
    """Return markup after the last </style> so CSS rules don't inflate counts."""
    idx = html.rfind("</style>")
    return html[idx:] if idx != -1 else html


def count(needle: str, hay: str) -> int:
    return hay.count(needle)


def visible_text(fragment: str) -> str:
    return TAG_STRIP_RE.sub("", fragment).strip()


def clarity(body: str) -> dict:
    card_count = count('class="index-active-card ', body) + count(
        'class="index-active-card"', body
    )
    titles = TITLE_ANCHOR_RE.findall(body)
    headline_is_title = 0
    headline_raw_id = 0
    for t in titles:
        txt = visible_text(t)
        if not txt:
            continue
        if BARE_ID_RE.match(txt):
            headline_raw_id += 1
        else:
            headline_is_title += 1
    return {
        "card_count": card_count,
        "headline_is_title": headline_is_title,
        "headline_raw_id": headline_raw_id,
        "cards_with_events": count('class="index-active-card-events"', body),
        "event_rows": count('class="index-active-card-event ', body),
        "event_squawks": count("index-active-card-event-squawk", body),
        "event_comments": count("index-active-card-event-comment", body),
        "agent_link_in_foot": count('class="index-active-card-agentlink"', body),
        "notask_cards": count("index-active-card-notask", body),
    }


def linkage(body: str) -> dict:
    title_hrefs = 0
    for m in re.finditer(
        r'<a\b[^>]*class="[^"]*index-active-card-title[^"]*"[^>]*>',
        body,
        re.IGNORECASE,
    ):
        h = HREF_RE.search(m.group(0))
        if h and h.group(1).startswith("/kd-"):
            title_hrefs += 1
    dangling = 0
    for m in HREF_RE.finditer(body):
        v = m.group(1).strip()
        if v in ("", "#"):
            dangling += 1
    return {
        "task_link_count": title_hrefs,
        "project_link_count": count('class="index-rail-project"', body),
        "mr_badges": count("index-rail-agent-mr", body),
        "dec_badges": count("index-rail-agent-dec", body),
        "stats_banner_links": count("stats-banner-item", body),
        "dangling_hrefs": dangling,
    }


def ux(body: str, narrow_body: str | None) -> dict:
    sections = 0
    for marker in (
        "stats-banner",  # stats banner
        "index-active-grid",  # live-agents grid
        ">Projects</h2",  # Projects column
        ">Recent activity</h2",  # Recent activity column
        "index-rail-panel",  # Platform-pulse aside panels (Hot ready / epics / decisions)
    ):
        if marker in body:
            sections += 1
    narrow_ok = 1
    if narrow_body is not None:
        # A horizontal-scroll / overflow marker or a rendered grid is the signal.
        if "index-active-grid" not in narrow_body:
            narrow_ok = 0
    # Stats-banner honesty: how many of the banner numbers rendered as an
    # em-dash placeholder ("—") instead of a real count. A high count means the
    # banner is decorative-only (data unavailable), which the judge weighs.
    banner_placeholders = 0
    for m in re.finditer(
        r'<span class="stats-banner-num">(.*?)</span>', body, re.DOTALL
    ):
        if visible_text(m.group(1)) in ("—", "-", ""):
            banner_placeholders += 1
    return {
        "sections_present": sections,
        "stale_cards": count("index-rail-agent-stale", body),
        "empty_states": count('class="empty"', body),
        "stats_banner_placeholders": banner_placeholders,
        "narrow_ok": narrow_ok,
    }


def correctness(body: str) -> dict:
    lowered = visible_text(body).lower()
    template_leaks = body.count("{{") + body.count("}}") + body.count("<no value>")
    error_markers = 0
    for kw in ("panic", "failed to", "<nil>"):
        error_markers += lowered.count(kw)
    card_count = count('class="index-active-card ', body) + count(
        'class="index-active-card"', body
    )
    return {
        "template_leaks": template_leaks,
        "error_markers": error_markers,
        "empty_index": 1 if card_count == 0 else 0,
    }


def fold_beauty(density_path: Path, page_slug: str) -> dict:
    beauty = {"desktop": None, "narrow": None}
    if not density_path or not density_path.exists():
        return {page_slug: beauty}
    d = json.loads(density_path.read_text())
    pages = d.get("pages", {})
    for vp in ("desktop", "narrow"):
        # density.py keys screenshots by their bare stem, e.g. index-livegrid-desktop
        cand = f"{page_slug}-{vp}"
        if cand in pages and pages[cand].get(vp):
            beauty[vp] = pages[cand][vp]
        elif page_slug in pages and pages[page_slug].get(vp):
            beauty[vp] = pages[page_slug][vp]
    return {page_slug: beauty}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument(
        "--page-slug",
        default="index-livegrid",
        help="HTML slug (in captures/html) of the Live-agents grid to measure",
    )
    ap.add_argument("--density", default=None, help="path to density.json to fold beauty")
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    # Prefer the post-render desktop DOM; fall back to the raw body.
    desktop_html = html_dir / f"{args.page_slug}-desktop.html"
    if not desktop_html.exists():
        desktop_html = html_dir / f"{args.page_slug}.html"
    narrow_html = html_dir / f"{args.page_slug}-narrow.html"

    if not desktop_html.exists():
        print(f"ERROR: no HTML capture at {desktop_html}", file=sys.stderr)
        sys.exit(1)

    body = body_only(desktop_html.read_text())
    narrow_body = body_only(narrow_html.read_text()) if narrow_html.exists() else None

    density_path = Path(args.density) if args.density else None
    out = {
        "target_url": args.target_url,
        "iteration": args.iteration,
        "page_slug": args.page_slug,
        "source_html": str(desktop_html.relative_to(cap.parent))
        if cap.parent in desktop_html.parents
        else str(desktop_html),
        "beauty": fold_beauty(density_path, args.page_slug) if density_path else {},
        "clarity": clarity(body),
        "linkage": linkage(body),
        "ux": ux(body, narrow_body),
        "correctness": correctness(body),
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
