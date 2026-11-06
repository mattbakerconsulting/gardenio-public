#!/usr/bin/env python3
"""Compute the FLAT objective-metric vector for the Users-FLAT audit.

FLAT = Fidelity, Legibility (transparency), All-in-time-series-order,
Tidiness. This is the objective (no-judgement) measure step of the
per-user-homepage audit loop (molecule gd-BTSKIWQPqD, step ka-0000001497).

It ingests a captures dir produced by the step-2 capture
(``docs/users-flat/<iteration>/captures`` with ``html/`` +
``screenshots/{desktop,narrow}/``) and emits ``metrics.json`` next to it.

Pixel/near-white/data-field signals are delegated to the sibling
``measure-density.py`` (imported), so the two tools stay consistent.

Design notes grounded in the ACTUAL captured DOM (not the rubric's
assumed class names):

* The populated activity lives on ``user-home.html`` as ``<li
  class="bead-row">`` items, each carrying a ``<span class="meta">``
  RELATIVE timestamp (e.g. ``20h``). The ``/section/inbox`` partial for a
  typical operator is EMPTY (``Messages (0)`` / ``No messages.``) — the
  ``user-inbox-msg`` / ``user-inbox-body`` strings in that file are CSS
  rules, not content. So message-bubble counting is done on rendered
  message bubbles only and an empty inbox yields ``transparency_ratio =
  1.0`` (0 == 0 hidden, NOT a fail).
* The homepage is a set of discrete ``user-home-section`` cards
  (``is-filled`` / ``is-empty`` / ``is-lazy``), each with its own dated
  list — i.e. a scatter of section cards, which the time-series
  dimension scores against ``unified_timeline_present``.
* Timestamps are relative-only (no ``title="2026-..."`` absolute
  attribute), so ``timeline_monotonic`` is measured by decoding the
  relative ages into a comparable ordinal per section list.
"""
import argparse
import json
import re
import sys
from pathlib import Path

# Reuse the density tool's PNG metrics so near_white_ratio/data_fields
# match the info-density audit exactly. The import is LAZY + guarded: the
# sibling tool hard-exits at import time when Pillow is absent, but a
# missing pixel dep must only null-out near_white_ratio here (the audit
# contract: unreadable screenshot ⇒ null + warn, never BLOCK).
def _load_png_metrics():
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "measure_density", Path(__file__).resolve().parent / "measure-density.py"
    )
    md = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(md)  # type: ignore
    return md.png_metrics


# --- shared regexes -------------------------------------------------------
STYLE_RE = re.compile(r"<style.*?</style>", re.S | re.I)
SCRIPT_RE = re.compile(r"<script.*?</script>", re.S | re.I)
TAG_RE = re.compile(r"<[^>]+>")
HREF_RE = re.compile(r'href="([^"]*)"')
BEAD_ROW_RE = re.compile(r'<li[^>]*class="[^"]*bead-row[^"]*"[^>]*>(.*?)</li>', re.S)
# A message bubble in the inbox feed (rendered content, not the CSS rule).
MSG_BUBBLE_RE = re.compile(r'<div[^>]*class="[^"]*user-inbox-msg[^"]*"[^>]*>(.*?)</div>', re.S)
MSG_BODY_RE = re.compile(r'<div[^>]*class="[^"]*user-inbox-body[^"]*"[^>]*>(.*?)</div>', re.S)
SECTION_RE = re.compile(r'<section[^>]*class="([^"]*user-home-section[^"]*)"', re.I)
# Empty-state <p> band. Captures the FULL element (opening tag + body) so
# tidiness() can inspect the opening tag for markers that mean the band is
# NOT actually occupying a visible section: a `hidden` attribute or the
# client-side facet no-match placeholder (`activity-empty-filter` /
# `data-activity-no-match`). The class regex uses \bempty\b, which also
# matches `activity-empty-filter` — that placeholder is hidden-by-default and
# must not score as a visible empty band (the merged-stream refactor
# ka-0000002041 elides truly-empty sections from the main page entirely).
EMPTY_P_RE = re.compile(r'(<p[^>]*class="[^"]*\bempty\b[^"]*"[^>]*>)(.*?)</p>', re.S | re.I)
# .meta spans hold BOTH relative timestamps (20h) and count badges ((44)).
META_RE = re.compile(r'<span class="meta">([^<]*)</span>')
# Relative-age token, e.g. 5s / 13m / 20h / 3d / 2w / 1mo / 1y or "just now".
REL_AGE_RE = re.compile(r"^\s*(\d+)\s*(s|m|h|d|w|mo|y)\s*$", re.I)
ABS_TS_RE = re.compile(r'title="(20\d\d-\d\d-\d\dT?[^"]*)"')

# Case-insensitive fidelity error markers. Word-boundaried where a bare
# substring would false-positive (e.g. "nil" inside "until").
ERROR_MARKERS = [
    r"failed to",
    r"\bpanic\b",
    r"unprovisioned",
    r"no operator",
    r"\[redacted\]",
    r"internal server error",
    r"\b5\d\d\b error",
    r"traceback",
    r"\bnil\b",
    r"could not resolve",
]
TEMPLATE_LEAKS = [r"\{\{", r"\}\}", r"<no value>"]
# Redaction / cross-view-hiding phrases. ANY hit in an xview capture is an
# automatic Legibility fail. "hidden" is matched ONLY as a standalone word
# in body text (CSS attribute selectors like `[hidden]` are stripped with
# the <style> block, and we further exclude the bracketed form).
REDACTION_PHRASES = [
    r"message bodies are hidden",
    r"viewing another operator",
    r"\[hidden\]",
    r"\[redacted\]",
    r"bodies are hidden",
    r"content hidden",
]

# Rough seconds-per-unit for decoding relative ages into an ordinal.
UNIT_SECONDS = {
    "s": 1, "m": 60, "h": 3600, "d": 86400,
    "w": 604800, "mo": 2592000, "y": 31536000,
}


def _strip(html: str) -> str:
    """Drop <style>/<script> so CSS/JS never pollute content greps."""
    return SCRIPT_RE.sub("", STYLE_RE.sub("", html))


def _text(fragment: str) -> str:
    return re.sub(r"\s+", " ", TAG_RE.sub(" ", fragment)).strip()


def _read(path: Path) -> str:
    return path.read_text(errors="replace") if path.exists() else ""


def _rel_age_seconds(token: str):
    m = REL_AGE_RE.match(token)
    if not m:
        return None
    return int(m.group(1)) * UNIT_SECONDS[m.group(2).lower()]


def fidelity(body: str) -> dict:
    low = body.lower()
    error_markers = sum(len(re.findall(p, low)) for p in ERROR_MARKERS)
    template_leaks = sum(len(re.findall(p, body)) for p in TEMPLATE_LEAKS)
    hrefs = HREF_RE.findall(body)
    broken_links = sum(
        1 for h in hrefs if h in ("#", "") or h.endswith("/u/") or "{{" in h
    )
    # Unresolved htmx: a lazy section still showing its Loading placeholder
    # in the captured (server-rendered) DOM — the partial never inlined.
    htmx_unresolved = len(re.findall(r"user-home-loading", body))
    return {
        "error_markers": error_markers,
        "template_leaks": template_leaks,
        "broken_links": broken_links,
        "htmx_unresolved": htmx_unresolved,
    }


def _msg_stats(html: str):
    """(#message bubbles, total visible body chars) for an inbox capture."""
    body = _strip(html)
    bubbles = MSG_BUBBLE_RE.findall(body)
    bodies = MSG_BODY_RE.findall(body)
    body_chars = sum(len(_text(b)) for b in bodies)
    return len(bubbles), body_chars


def legibility(self_inbox: str, xview_inbox: str, xview_home: str) -> dict:
    # Redaction check runs over BOTH the xview inbox partial AND the xview
    # full homepage — a banner could live on either surface.
    xview_all = _strip(xview_inbox) + "\n" + _strip(xview_home)
    low = xview_all.lower()
    banner = 1 if any(re.search(p, low) for p in REDACTION_PHRASES) else 0

    self_msg, self_chars = _msg_stats(self_inbox)
    xview_msg, xview_chars = _msg_stats(xview_inbox)
    # transparency_ratio: cross-view visible body chars / self-view.
    # When the self-view inbox is empty (0 chars), there is nothing to hide,
    # so the ratio is a clean 1.0 rather than a divide-by-zero fail.
    if self_chars == 0:
        ratio = 1.0 if xview_chars == 0 else 1.0
    else:
        ratio = round(xview_chars / self_chars, 4)
    return {
        "redaction_banner_present": banner,
        "xview_msg_count": xview_msg,
        "self_msg_count": self_msg,
        "xview_body_chars": xview_chars,
        "self_body_chars": self_chars,
        "transparency_ratio": ratio,
    }


def timeseries(home_html: str) -> dict:
    body = _strip(home_html)
    rows = BEAD_ROW_RE.findall(body)
    rows_total = len(rows)
    # A row is time-anchored if it carries a decodable relative age OR an
    # absolute title timestamp.
    rows_with_ts = 0
    ages = []  # per-row age in seconds, in DOM order, for monotonicity
    for r in rows:
        metas = META_RE.findall(r)
        secs = None
        for tok in metas:
            s = _rel_age_seconds(tok)
            if s is not None:
                secs = s
                break
        if secs is None and ABS_TS_RE.search(r):
            secs = 0  # present but not decoded to an ordinal
            rows_with_ts += 1
            ages.append(None)
            continue
        if secs is not None:
            rows_with_ts += 1
            ages.append(secs)
    coverage = round(rows_with_ts / rows_total, 4) if rows_total else 0.0

    # Distinct dated section cards: each user-home-section that renders its
    # own list is a break in "one merged stream". Count the sections that
    # actually hold rows (filled) — an is-empty card is junk, not a timeline
    # break, and is scored under Tidiness instead.
    section_classes = SECTION_RE.findall(body)
    filled_sections = sum(1 for c in section_classes if "is-filled" in c)
    lazy_sections = sum(1 for c in section_classes if "is-lazy" in c)
    # disjoint_section_cards = the number of separate row-bearing lists NOT
    # merged into one primary stream. filled + lazy (each lazy loads its own
    # dated list) minus the one that would BE the primary stream.
    disjoint = max(0, filled_sections + lazy_sections - 1)

    # unified_timeline_present: the page is ONE merged chronological list
    # only if there is a single row-bearing container. Multiple filled/lazy
    # section cards ⇒ 0.
    unified = 1 if (filled_sections + lazy_sections) <= 1 and rows_total else 0

    # timeline_monotonic: within the primary (first filled) section, are the
    # decoded ages monotonic? With scattered cards this is measured over the
    # whole DOM-ordered age list as a best-effort signal.
    decoded = [a for a in ages if isinstance(a, int)]
    if len(decoded) >= 2:
        non_dec = all(decoded[i] <= decoded[i + 1] for i in range(len(decoded) - 1))
        non_inc = all(decoded[i] >= decoded[i + 1] for i in range(len(decoded) - 1))
        monotonic = 1 if (non_dec or non_inc) else 0
    else:
        monotonic = 0

    return {
        "unified_timeline_present": unified,
        "timeline_monotonic": monotonic,
        "rows_with_timestamp": rows_with_ts,
        "rows_total": rows_total,
        "timestamp_coverage": coverage,
        "disjoint_section_cards": disjoint,
        "filled_sections": filled_sections,
        "lazy_sections": lazy_sections,
    }


def tidiness(home_html: str, desktop_png: dict, dup_source: str) -> dict:
    body = _strip(home_html)
    # Empty-state bands: <p class="empty">No ....</p> occupying a VISIBLE
    # section. Skip a match when its opening tag is hidden-by-default or is the
    # client-side facet no-match placeholder — neither occupies a real band, so
    # neither is junk-on-screen.
    empty_markers = 0
    for open_tag, inner in EMPTY_P_RE.findall(body):
        low_open = open_tag.lower()
        if "hidden" in low_open or "data-activity-no-match" in low_open \
                or "activity-empty-filter" in low_open:
            continue
        if "loading" in _text(inner).lower():
            continue
        empty_markers += 1

    # Duplicate rows: same (bead-id | title) key appearing twice.
    rows = BEAD_ROW_RE.findall(_strip(dup_source))
    keys = []
    for r in rows:
        idm = re.search(r"/beads/((gd|ka)-[A-Za-z0-9]+)", r)
        title = re.search(r'bead-row-title[^>]*>(.*?)</span>', r, re.S)
        key = (idm.group(1) if idm else "") or (_text(title.group(1))[:60] if title else "")
        if key:
            keys.append(key)
    seen = set()
    dupes = 0
    for k in keys:
        if k in seen:
            dupes += 1
        seen.add(k)

    near_white = desktop_png.get("near_white_ratio") if desktop_png else None
    # data_fields: count of row-level data carriers actually present.
    data_fields = len(re.findall(r"<li[^>]*class=\"[^\"]*bead-row", body))
    return {
        "empty_section_markers": empty_markers,
        "duplicate_rows": dupes,
        "near_white_ratio": near_white,
        "data_fields": data_fields,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True, help="captures dir (html/, screenshots/)")
    ap.add_argument("--out", required=True, help="output metrics.json path")
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--user-slug", required=True)
    ap.add_argument("--xview-slug", required=True)
    args = ap.parse_args()

    cap = Path(args.captures)
    html = cap / "html"
    desk = cap / "screenshots" / "desktop"

    home = _read(html / "user-home.html")
    xhome = _read(html / "xview-user-home.html")
    self_inbox = _read(html / "section-inbox.html")
    xview_inbox = _read(html / "xview-section-inbox.html")

    warnings = []
    if not home:
        warnings.append("user-home.html missing — fidelity/timeseries/tidiness null")
    if not xhome and not xview_inbox:
        warnings.append("no xview-* capture — legibility measured self-only")

    # Desktop PNG for near_white_ratio. Mark null (not fail) if the pixel
    # dep is missing OR the capture is unreadable.
    desktop_png = None
    dp = desk / "user-home.png"
    if dp.exists():
        try:
            png_metrics = _load_png_metrics()
            desktop_png = png_metrics(dp)
        except SystemExit as e:  # measure-density exits(2) when Pillow absent
            warnings.append(
                f"pixel dep unavailable (measure-density exit {e.code}) — near_white_ratio null"
            )
        except Exception as e:  # pragma: no cover - corrupt capture
            warnings.append(f"desktop png unreadable ({e}) — near_white_ratio null")

    body_home = _strip(home)
    out = {
        "target_url": args.target_url,
        "user_slug": args.user_slug,
        "xview_slug": args.xview_slug,
        "iteration": args.iteration,
        "fidelity": fidelity(body_home) if home else None,
        "legibility": legibility(self_inbox, xview_inbox, xhome),
        "timeseries": timeseries(home) if home else None,
        "tidiness": tidiness(home, desktop_png, home) if home else None,
        "warnings": warnings,
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")
    print(json.dumps(out, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
