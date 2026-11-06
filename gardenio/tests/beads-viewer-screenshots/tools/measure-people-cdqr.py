#!/usr/bin/env python3
"""CDQR+Dedup objective measure for the People-Directory (/people) audit.

Ingests a captures dir (html/ + screenshots/{desktop,narrow}/) and emits a
metrics.json triaging five dimensions: Density (pixel/DOM metrics),
Correctness (error/broken-link/template-leak greps), Dedup & Top-Stats (THE
HEADLINE — operator rows parsed, clustered by likely-same-human, plus the
page-level aggregate header), and Relevance (sort order + human/bot
segmentation). No judgement — pure signals the downstream judge step scores.

Contract: docs/people-cdqr/<iteration>/metrics.json (step gd-WhWbzcEDru).

The current /people page (controller people.go + templates/people.html)
already performs render-time identity collapse: each <li.people-row> is ONE
canonical human with every linked identity (oidc/slack/gitlab/agent) rendered
as a chip inside the row. So a cross-ROW dupe cluster is, by construction, an
*unmerged* one — the daemon has not linked those identities. That makes the
DOM itself the authoritative merge state; we do not need a daemon round-trip
to determine `unmerged_dupe_clusters` (and the client_credentials bearer lacks
the beads-api audience in-cluster anyway, so the daemon is 401 here).
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
PEOPLE_ROW_RE = re.compile(r'<li class="bead-row people-row".*?</li>', re.S)

# Correctness markers (case-insensitive substring counts over the body).
ERROR_MARKERS = [
    "unprovisioned", "no operator", "panic:", "failed to",
    "internal server error", "500 internal", "502 bad gateway",
    "503 service", "504 gateway", "&lt;no value&gt;",
]

# The page-level aggregate header chip classes (templates/people.html
# .people-summary). Maps DOM class -> canonical field label.
STATS_FIELD_CLASSES = {
    "people-summary-people": "people",
    "people-summary-merged": "identities_merged",
    "people-summary-active": "active_now",
    "people-summary-open": "open_beads",
    "people-summary-created": "created_7d",
    "people-summary-closed": "closed_7d",
    "people-summary-mrs": "open_mrs",
    "people-summary-decisions": "pending_decisions",
    "people-summary-gardeners": "gardeners",
}

# Placeholder display names that are really unresolved handles, not human
# names — a Quality/Correctness smell the judge weighs.
PLACEHOLDER_DISPLAY_RE = re.compile(
    r"@noreply\.kbeads\.local|@noreply\.gitlab\.com|_bot_[0-9a-f]{8}|"
    r"^slack-U[0-9A-Z]+$|^op-\d+$",
    re.IGNORECASE,
)


# --------------------------------------------------------------------------
# Density
# --------------------------------------------------------------------------
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
                "width": w,
                "height": h,
                "near_white_ratio": round(near_white / total, 4),
                "mean_luma": round(mean_luma, 4),
                "data_fields": None,      # filled by caller from html
                "dom_element_count": None,
                "text_byte_per_dom": None,
                "unique_links": None,
            }
    except Exception as exc:  # unreadable screenshot -> null block + warn
        print(f"WARN: unreadable screenshot {path}: {exc}", file=sys.stderr)
        return None


# --------------------------------------------------------------------------
# Correctness
# --------------------------------------------------------------------------
def correctness(html: str) -> dict:
    low = html.lower()
    error_markers = sum(low.count(m.lower()) for m in ERROR_MARKERS)
    # broken links: href="#", href="", slug-less /u/, template-leak in href.
    broken = (len(re.findall(r'href="#"', html))
              + len(re.findall(r'href=""', html))
              + len(re.findall(r'href="[^"]*/u/"', html))
              + len(re.findall(r'href="[^"]*\{\{', html)))
    template_leaks = (html.count("{{") + html.count("}}")
                      + low.count("<no value>") + low.count("&lt;no value&gt;"))
    return {
        "error_markers": error_markers,
        "broken_links": broken,
        "template_leaks": template_leaks,
    }


# --------------------------------------------------------------------------
# Dedup & Top-Stats — THE HEADLINE
# --------------------------------------------------------------------------
def _norm_display(s: str) -> str:
    return re.sub(r"\s+", " ", (s or "").strip()).lower()


def _email_localpart(email: str) -> str:
    email = (email or "").strip().lower()
    return email.split("@", 1)[0] if "@" in email else email


def _parse_rows(html: str) -> list:
    """Extract each operator row's (operator_id, display, email, identities,
    is_placeholder_display) from the rendered directory."""
    rows = []
    for chunk in PEOPLE_ROW_RE.findall(html):
        op = re.search(r'data-operator-id="(\d+)"', chunk)
        op_id = op.group(1) if op else None
        disp = re.search(r'people-row-name"[^>]*?>([^<]+)</a>', chunk, re.S)
        display = disp.group(1).strip() if disp else ""
        em = re.search(r'mailto:([^"]+)"', chunk)
        email = em.group(1).strip() if em else ""
        # Linked identities already folded into this row (kind + handle).
        idents = []
        for k, v in re.findall(
                r'people-identity-kind">([^<]+)</span>\s*([^<]+?)\s*<a', chunk):
            idents.append({"kind": k.strip(), "handle": v.strip()})
        rows.append({
            "operator_id": op_id,
            "display": display,
            "email": email,
            "identities": idents,
            "placeholder_display": bool(PLACEHOLDER_DISPLAY_RE.search(display)),
        })
    return rows


def _cluster_rows(rows: list) -> list:
    """Union-find over rows: two rows join a cluster when they share a
    normalized email localpart OR a normalized display name (the step spec's
    'lowercased email localpart UNION normalized display name'). Returns the
    list of clusters (each a list of row indices) with size > 1."""
    n = len(rows)
    parent = list(range(n))

    def find(x):
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a, b):
        ra, rb = find(a), find(b)
        if ra != rb:
            parent[rb] = ra

    # Bucket rows by each join key; union within buckets. Skip empty keys and
    # keys that are structurally shared placeholders (empty localpart etc.).
    by_local, by_disp = {}, {}
    for i, r in enumerate(rows):
        lp = _email_localpart(r["email"])
        if lp:
            by_local.setdefault(lp, []).append(i)
        dn = _norm_display(r["display"])
        # A placeholder display (slack-Uxxx@…, op-N) is not a human name — do
        # NOT use it as a same-human key (it would false-cluster unrelated
        # placeholders). Real names / real emails only.
        if dn and not r["placeholder_display"]:
            by_disp.setdefault(dn, []).append(i)
    for bucket in list(by_local.values()) + list(by_disp.values()):
        for j in bucket[1:]:
            union(bucket[0], j)

    groups = {}
    for i in range(n):
        groups.setdefault(find(i), []).append(i)
    return [sorted(idx) for idx in groups.values() if len(idx) > 1]


def _display_surname(display: str):
    """Surname from a real 'First Last' display name only (>=4 chars).
    None for single-word handles, emails-as-display, placeholders."""
    d = _norm_display(display)
    if "@" in d:
        return None
    words = [w for w in re.split(r"[^a-z]+", d) if w]
    if len(words) >= 2 and len(words[-1]) >= 4:
        return words[-1]
    return None


def _fuzzy_dupe_candidates(rows: list) -> list:
    """Supplementary signal BEYOND the strict spec clustering: catch a human
    split across DIFFERENT email domains / handle spellings the exact
    email-localpart rule misses. Two matching rules, both precise:
      1. Equal display surnames (both rows have a real 'First Last', same last
         token) — e.g. two 'Rivers' rows.
      2. A row's real display surname S (>=5 chars) is a SUFFIX of another
         row's email localpart — e.g. 'rivers' is a suffix of 'srivers'
         (sam.rivers@acme.example vs srivers@contractor-a.example).
    Suffix (not substring) matching is deliberate: it catches abbreviated
    handles like srivers while rejecting 'marc' ⊂ 'marcus'. Excludes exact
    localpart matches (strict clustering owns those) and placeholder rows.
    Human-review candidates, NOT asserted merges."""
    cands = []
    real = [r for r in rows if not r["placeholder_display"]]
    for a in range(len(real)):
        ra = real[a]
        la = _email_localpart(ra["email"])
        sa = _display_surname(ra["display"])
        for b in range(a + 1, len(real)):
            rb = real[b]
            lb = _email_localpart(rb["email"])
            if la and la == lb:
                continue  # exact-localpart: strict clustering owns it
            sb = _display_surname(rb["display"])
            reason = None
            if sa and sb and sa == sb:
                reason = f"surname={sa}"
            elif sa and len(sa) >= 5 and lb.endswith(sa) and lb != sa:
                reason = f"{sa}⊂localpart:{lb}"
            elif sb and len(sb) >= 5 and la.endswith(sb) and la != sb:
                reason = f"{sb}⊂localpart:{la}"
            if reason:
                cands.append({
                    "reason": reason,
                    "members": [
                        {"operator_id": ra["operator_id"],
                         "display": ra["display"], "email": ra["email"]},
                        {"operator_id": rb["operator_id"],
                         "display": rb["display"], "email": rb["email"]},
                    ],
                })
    return cands


def dedup(html: str) -> dict:
    rows = _parse_rows(html)
    clusters_idx = _cluster_rows(rows)
    clusters = []
    for idx in clusters_idx:
        members = [{
            "operator_id": rows[i]["operator_id"],
            "display": rows[i]["display"],
            "email": rows[i]["email"],
        } for i in idx]
        clusters.append({"size": len(idx), "members": members})
    dupe_rows_excess = sum(c["size"] - 1 for c in clusters)

    # Page-level aggregate header (.people-summary chip strip).
    has_header = "people-summary" in html
    stats_present = [
        label for cls, label in STATS_FIELD_CLASSES.items() if cls in html
    ]
    # Read header numeric values for the report (best-effort text parse).
    header_values = {}
    m = re.search(r'<div class="people-summary[^>]*>(.*?)</div>', html, re.S)
    if m:
        strip = re.sub(r"<[^>]+>", " ", m.group(1))
        strip = re.sub(r"\s+", " ", strip).strip()
        header_values["raw"] = strip

    # Identity-collapse evidence: total linked identities across all rows vs
    # rows rendered. rows < identities => the page IS collapsing aliases.
    total_identities = sum(len(r["identities"]) for r in rows)
    placeholder_rows = sum(1 for r in rows if r["placeholder_display"])

    return {
        "rows_rendered": len(rows),
        "total_linked_identities": total_identities,
        "identity_collapse_active": total_identities > len(rows),
        "placeholder_display_rows": placeholder_rows,
        "dupe_cluster_count": len(clusters),
        "dupe_rows_excess": dupe_rows_excess,
        # Every cross-row cluster is unmerged by construction (see module
        # docstring): the render already folds linked aliases into one row.
        "unmerged_dupe_clusters": clusters,
        # Supplementary: same-human-across-domains the strict rule misses.
        "fuzzy_dupe_candidates": _fuzzy_dupe_candidates(rows),
        "has_stats_header": has_header,
        "stats_fields_present": stats_present,
        "stats_header_text": header_values.get("raw", ""),
    }


# --------------------------------------------------------------------------
# Relevance
# --------------------------------------------------------------------------
_AGO_UNIT_SECONDS = {
    "s": 1, "sec": 1, "second": 1, "seconds": 1,
    "m": 60, "min": 60, "minute": 60, "minutes": 60,
    "h": 3600, "hr": 3600, "hour": 3600, "hours": 3600,
    "d": 86400, "day": 86400, "days": 86400,
    "w": 604800, "week": 604800, "weeks": 604800,
    "mo": 2592000, "month": 2592000, "months": 2592000,
    "y": 31536000, "year": 31536000, "years": 31536000,
}


def _ago_to_seconds(text: str):
    """Parse the per-row last-active chip into seconds. The template renders a
    humanized 'ago' form WITHOUT the word 'ago' and with the unit glued to the
    number: 'active just now', 'active 1m', 'active 6h', 'active 1d',
    'active 2w'. 'just now' -> 0 seconds (most recent)."""
    t = re.sub(r"\s+", " ", text).strip().lower()
    if "just now" in t:
        return 0
    m = re.search(r'active\s+(\d+)\s*([a-z]+)(?:\s+ago)?', t)
    if not m:
        return None
    n = int(m.group(1))
    unit = m.group(2).rstrip(".")
    if unit in _AGO_UNIT_SECONDS:
        return n * _AGO_UNIT_SECONDS[unit]
    return None


def relevance(html: str) -> dict:
    # Declared sort (subtitle) — the page states its own ordering.
    subtitle = ""
    m = re.search(r'<p class="meta">([^<]*sorted[^<]*)</p>', html,
                  re.IGNORECASE)
    if m:
        subtitle = re.sub(r"\s+", " ", m.group(1)).strip()
    declared_active_sort = "recent activity" in subtitle.lower()

    # Verify: parse per-row last-active-ago in DOM order and check it is
    # (mostly) non-decreasing in seconds -> descending recency.
    rows = PEOPLE_ROW_RE.findall(html)
    ago_seconds = []
    for chunk in rows:
        act = re.search(r'people-stat-active"[^>]*>(.*?)</span>', chunk, re.S)
        secs = _ago_to_seconds(act.group(1)) if act else None
        ago_seconds.append(secs)
    known = [s for s in ago_seconds if s is not None]
    inversions = 0
    prev = None
    for s in known:
        if prev is not None and s < prev:
            inversions += 1
        prev = s
    dom_matches_active_sort = (len(known) >= 2 and
                               inversions <= max(1, len(known) // 10))

    if declared_active_sort and dom_matches_active_sort:
        sort_order = "active-recency (declared + DOM-verified)"
    elif declared_active_sort:
        sort_order = ("active-recency (declared; DOM not strictly monotonic — "
                      f"{inversions} inversions over {len(known)} timed rows)")
    else:
        sort_order = "unknown/undeclared"

    # Segmentation: is the list split humans vs agents/bots, or one flat list?
    # The template renders a single <ul class="people-list"> with no group
    # headings; detect any segmentation wrapper/heading.
    people_lists = len(re.findall(r'<ul class="[^"]*people-list', html))
    seg_headings = len(re.findall(
        r'<h[23][^>]*>[^<]*(humans?|agents?|bots?|people|team)[^<]*</h[23]>',
        html, re.IGNORECASE))
    segments_present = people_lists > 1 or seg_headings > 1

    return {
        "sort_order": sort_order,
        "declared_sort": subtitle,
        "sort_dom_inversions": inversions,
        "sort_timed_rows": len(known),
        "segments_present": segments_present,
        "people_list_count": people_lists,
    }


# --------------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", default="http://localhost:8095")
    ap.add_argument("--page", default="/people")
    ap.add_argument("--iteration", required=True)
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    people = html_dir / "people.html"
    if not people.exists():
        print(f"FATAL: {people} missing", file=sys.stderr)
        sys.exit(2)
    html = people.read_text(errors="replace")

    # Density: html metrics + per-viewport pixel metrics, merged.
    hm = density_html(people)
    dens = {}
    for vp in ("desktop", "narrow"):
        png = cap / "screenshots" / vp / "people.png"
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
        "page": args.page,
        "iteration": args.iteration,
        "density": dens,
        "correctness": correctness(html),
        "dedup": dedup(html),
        "relevance": relevance(html),
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")
    print(json.dumps(out, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
