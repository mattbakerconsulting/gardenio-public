#!/usr/bin/env python3
"""Compute objective Users-CAMP metrics for the users-page audit harness.

Step 3 (Measure) of the users-camp formula. CAMP over the two USER surfaces:

  - index  = /people      (handlePeople,   people.go)     -> page slug "people"
  - detail = /u/{slug}    (handleUserHome, user_home.go)  -> page slug "user-home"

Reuses the density primitives from measure-density.py for the Polish vector and
layers on users-specific structural signals for the four CAMP dimensions:

  - Coverage    : directory rows vs kd operator ground truth, summary-chip band
  - Accuracy    : per-row stat chips vs kd truth, template/error leaks, raw
                  Slack-mention leak in recent-request snippets (T1722)
  - Management  : facet chips, identity chips, reachable sub-pages
  - Polish      : near-white ratio + overflow (delegated pixel metrics)

Plus the dimension the retired FLAT screenshot rubric was STRUCTURALLY BLIND to
(epic gd-g35z8CQNt5, decision gd-2ZYAZeFV5f):

  - Integrity   : every in-page #anchor RESOLVES to an id that exists, and every
                  registered /u/{slug}/* route is REACHABLE from rendered markup.

That blind spot is not hypothetical — it produced two real P1/P2 defects that a
pixel-perfect screenshot could never show:
  * ka-0000000915 — all 5 summary stat tiles pointed at anchor ids that had been
    deleted by the merged-spine rework. Five dead clicks, rendered beautifully.
    (fixed 16be17c76: every tile now targets the live #activity spine)
  * ka-0000004829 — /u/{slug}/inbox was orphaned; its only entry link sat behind
    a template gate that was never true. (fixed 0269d5fa3)

Both are cheap to detect from the captured DOM, which is exactly what
`integrity` below does — so a regression reds the audit instead of scoring 9/10.

No judgement is applied here — every value is an objective count or ratio.
Unreadable screenshots are recorded as null (the judge skips nulls) and a
warning is printed; they never BLOCK.

Signals are grounded in the ACTUAL captured markup, not assumed class names:
  * /people rows are ``<li class="bead-row people-row" data-operator-id=...>``;
    the totals band is ``<span class="chip chip-type people-summary-*">`` and
    per-row stats are ``people-stat-*`` / identities ``people-identity``.
  * /u/{slug} summary tiles are ``<li class="user-home-stat">`` wrapping an
    ``<a href=... data-facet-link=...>``; the spine is ``<section id="activity">``
    with ``<li class="bead-row activity-row" data-facet=...>`` events, date rails
    as ``<li class="timeline-rail">``, and facet chips ``button.activity-facet``
    carrying ``data-facet``.

Usage:
  measure-users-camp.py --captures <dir> --out metrics.json \
    --target-url http://localhost:8095 --iteration 2026-07-26T1900 \
    --user-slug avery.locke
"""
import argparse
import html
import json
import re
import subprocess
import sys
from pathlib import Path

import importlib.util as _ilu

# --- reuse the density primitives ---------------------------------------------
# The import is LAZY + guarded, mirroring measure-flat.py: measure-density.py
# hard-exits at import time when Pillow is absent, but a missing PIXEL dep must
# only null out near_white_ratio/mean_luma here. The DOM half of this audit — and
# the whole Integrity vector, which is the reason this lane exists — needs no
# Pillow at all, so an eager import would turn "no Pillow" into "no audit".
_DENSITY = None


def _density():
    """Load measure-density.py once; return None when its deps are unavailable."""
    global _DENSITY
    if _DENSITY is None:
        try:
            _md_path = Path(__file__).resolve().parent / "measure-density.py"
            _spec = _ilu.spec_from_file_location("measure_density", _md_path)
            _md = _ilu.module_from_spec(_spec)
            _spec.loader.exec_module(_md)
            _DENSITY = _md
        except BaseException:  # SystemExit included — the tool sys.exit(2)s on no-Pillow
            _DENSITY = False
    return _DENSITY or None

TAG_INNER = re.compile(r"<[^>]+>")
STYLE_SCRIPT = re.compile(r"<(style|script)\b[^>]*>.*?</\1>", re.S | re.I)

# Page slugs this tool measures. INDEX_PAGE is the users directory, DETAIL_PAGE
# the per-user homepage; both are captured by the canonical Playwright harness
# (tests/canonical-pages.spec.js captures 'people' always and 'user-home' when
# SCREENSHOT_USER_SLUG is set). SELF_PAGE is the OWNER (IsViewerSelf=true) render
# of the detail page, captured as 'user-home-self' when SCREENSHOT_USER_SELF_EMAIL
# is also set (bug gd-zFNvLriG4D) — optional, so the self-variant signal is null
# when that capture is absent.
INDEX_PAGE = "people"
DETAIL_PAGE = "user-home"
SELF_PAGE = "user-home-self"


def _read(path: Path) -> str:
    return path.read_text(errors="replace") if path.exists() else ""


def _body(raw: str) -> str:
    """Strip <style>/<script> so signals reflect rendered markup, not CSS.

    This matters more here than on other audit pages: user_home_activity.html
    ships an inline <script> whose facet-filter code mentions data-facet and
    href='#activity' in string literals. Counting those as rendered anchors
    would inflate every Integrity signal, so the strip is load-bearing.
    """
    return STYLE_SCRIPT.sub("", raw)


def _visible_text(raw: str) -> str:
    """Rendered (tag-stripped) text only — for word-level smell tests."""
    return html.unescape(TAG_INNER.sub(" ", _body(raw)))


# --- Polish / Density ---------------------------------------------------------
def polish_for(captures: Path, slug: str, warnings: list) -> dict:
    md = _density()
    html_path = captures / "html" / f"{slug}.html"
    entry = {"html": None, "desktop": None, "narrow": None}
    if html_path.exists():
        entry["html"] = md.html_metrics(html_path) if md else None
        if not md:
            warnings.append(
                f"density helper unavailable (Pillow missing?) — DOM/pixel Polish "
                f"metrics null for {slug}; Integrity signals are unaffected"
            )
    else:
        warnings.append(f"missing html capture: {slug}.html")
    for vp in ("desktop", "narrow"):
        png = captures / "screenshots" / vp / f"{slug}.png"
        if not png.exists():
            warnings.append(f"missing screenshot: {vp}/{slug}.png")
            continue
        if not md:
            continue  # already warned above; null pixel block
        try:
            entry[vp] = md.png_metrics(png)
        except Exception as e:  # unreadable screenshot -> null, warn, no block
            warnings.append(f"unreadable screenshot {vp}/{slug}.png: {e}")
            entry[vp] = None
    return entry


# The per-viewport blocks that carry the PIXEL-derived Polish signals
# (near_white_ratio / mean_luma) the judge anchors on. The "html" sub-block is
# DOM-derived and does not gate the visual score, so it is deliberately excluded.
_POLISH_PIXEL_VIEWPORTS = ("desktop", "narrow")


def polish_pixels_all_null(polish: dict) -> bool:
    """True when EVERY per-viewport pixel block is null across BOTH surfaces.

    Polish is the dimension that GATES this lane's terminal goal (all four CAMP
    dims >= 8 for two consecutive iterations), and it is scored from the PNG
    pixels, not the DOM. Under step-role dispatch the capture step's PNGs are
    gitignored and never reach the measure pod, so this whole dimension can go
    silently dark while every other signal reads clean — the exact failure in
    bug gd-DQLUhVH4mc, where the confirmation run that would have closed the lane
    landed Polish entirely null yet reported "CLEAN — no FAIL-FLAGS".

    A null that gates the goal must FAIL LOUDLY, not warn quietly. So a total
    pixel blackout (0 of 4 blocks measured) is a FAIL-FLAG, while a PARTIAL loss
    stays a per-PNG warning (the judge skips individual nulls). The ``bool(blocks)``
    guard means an empty/absent polish map (nothing was ever asked for) does NOT
    manufacture a false flag.
    """
    blocks = []
    for surface in polish.values():
        if not isinstance(surface, dict):
            continue
        for vp in _POLISH_PIXEL_VIEWPORTS:
            blocks.append(surface.get(vp))
    return bool(blocks) and all(b is None for b in blocks)


# --- Integrity: anchors resolve, routes reachable ------------------------------
# The defect class the FLAT screenshot rubric could not see. Both regexes run
# over the STYLE/SCRIPT-stripped body so JS string literals never count as
# rendered markup.
ID_RE = re.compile(r'\bid="([^"]+)"')
FRAG_HREF_RE = re.compile(r'href="#([^"]+)"')


def anchor_integrity(page_html: str) -> dict:
    """Every in-page ``href="#frag"`` must name an ``id`` that exists on the page.

    Returns the resolved/dangling split plus the dangling fragment names, so the
    judge can name the exact dead click and the file-issues step can quote it.
    A bare ``href="#"`` is a deliberate no-op affordance (JS-driven button), not
    a dead anchor, so it is excluded from the denominator.
    """
    body = _body(page_html)
    ids = set(ID_RE.findall(body))
    frags = [f for f in FRAG_HREF_RE.findall(body) if f and f != "#"]
    dangling = sorted({f for f in frags if f not in ids})
    resolved = len(frags) - sum(1 for f in frags if f in dangling)
    return {
        "fragment_link_count": len(frags),
        "resolved_fragment_links": resolved,
        "dangling_fragment_links": len(frags) - resolved,
        "dangling_fragments": dangling,
        # 1.0 == every in-page anchor lands somewhere real. Anything < 1.0 is a
        # dead click on a rendered, pixel-perfect page.
        "anchor_resolution_ratio": (
            round(resolved / len(frags), 4) if frags else None
        ),
    }


def route_reachability(detail_html: str, slug: str, subroutes: list) -> dict:
    """Are the per-user sub-pages actually LINKED from the rendered detail page?

    ``subroutes`` are path suffixes under /u/{slug} that a human is supposed to
    be able to reach by clicking (e.g. ["inbox"]). A route that 200s but appears
    in zero rendered hrefs is orphaned — reachable only by typing the URL, which
    is how ka-0000004829 hid in plain sight. Both the encoded and raw slug forms
    are checked because the template pipes the slug through urlquery.
    """
    body = _body(detail_html)
    hrefs = set(re.findall(r'href="([^"]+)"', body))
    linked, orphaned = [], []
    for sub in subroutes:
        want = f"/u/{slug}/{sub}"
        hit = any(want in h for h in hrefs)
        (linked if hit else orphaned).append(sub)
    return {
        "subroutes_checked": len(subroutes),
        "subroutes_linked": len(linked),
        "orphaned_subroutes": sorted(orphaned),
        "route_reachability_ratio": (
            round(len(linked) / len(subroutes), 4) if subroutes else None
        ),
    }


# The summary tiles' facet contract. The OLD static check asked only "does a
# chip with this tile's facet key EXIST", and it EXCUSED zero-count tiles ("a
# zero tile needs no chip — nothing to filter to"). Both choices were blind
# spots that let a pixel-perfect, DOM-well-formed page score facet_match_ratio
# 1.0 while it was broken two ways (bug gd-XVZNaYe9nZ):
#
#   1. A zero-count tile is still CLICKABLE. Clicking it hit activate()'s
#      !target branch and RESET the filter to '' — showing the ENTIRE stream,
#      the opposite of the tile's promise (ka-0000003914, 139/170 tiles).
#   2. Chip existence != correct filtering. "Merged MRs" and "Open MRs" share
#      facet key `mrs`, so a chip named `mrs` exists and BOTH tiles "matched" —
#      while one surfaces rows that contradict its own label (ka-0000008262).
#
# This STATIC function now emits DESCRIPTIVE structural context ONLY. It no
# longer defines the gate ratio and it no longer manufactures a false 1.0:
#   * `duplicate_tile_facet_keys` — two tiles on ONE page sharing a facet key.
#     100% static, always available (no browser), and the authoritative detector
#     for ka-0000008262.
#   * `tiles_missing_facet_chip` — facet keys of tiles (ZERO-COUNT IN SCOPE)
#     whose facet renders no chip. Descriptive only: a zero tile with no chip is
#     the EXPECTED healthy DOM (the defect is what the CLICK does, which a static
#     scan cannot see), so this is NOT a fail flag — it is context for the judge.
#
# The GATE moved to the CLICK-OUTCOME measurement (tile_click_outcomes below),
# which drives the real tile in a browser and is fix-independent by
# construction. Read that block, not this one, to decide if a tile lies.
STAT_TILE_BLOCK_RE = re.compile(r'<li[^>]*class="[^"]*user-home-stat[^"]*".*?</li>', re.S | re.I)
FACET_LINK_RE = re.compile(r'data-facet-link="([^"]*)"')
FACET_CHIP_RE = re.compile(r'data-facet="([^"]*)"')
TILE_NUM_RE = re.compile(r'user-home-stat-num"[^>]*>\s*([0-9,]+)')
# The tile's visible label text (span.user-home-stat-label), so a duplicate-key
# finding can name the two tiles a human sees ("Merged MRs" vs "Open MRs").
TILE_LABEL_RE = re.compile(
    r'user-home-stat-label"[^>]*>(.*?)</span>', re.S | re.I
)


def facet_integrity(detail_html: str) -> dict:
    body = _body(detail_html)
    chip_facets = set(FACET_CHIP_RE.findall(body))
    all_tiles = 0            # every rendered summary stat tile
    linked_tiles = 0         # tiles carrying a (non-empty) data-facet-link
    zero_linked = 0          # zero-count tiles that are STILL a facet link
    missing_chip = set()     # facet keys (any count) whose facet renders no chip
    by_facet: dict = {}      # facet_link -> [tile labels] for duplicate detection
    for block in STAT_TILE_BLOCK_RE.findall(body):
        all_tiles += 1
        fm = FACET_LINK_RE.search(block)
        facet = fm.group(1) if (fm and fm.group(1)) else None
        num = TILE_NUM_RE.search(block)
        count = int(num.group(1).replace(",", "")) if num else 0
        if not facet:
            continue  # inert tile (no facet link) — cannot lie about a filter
        linked_tiles += 1
        if count <= 0:
            zero_linked += 1
        lm = TILE_LABEL_RE.search(block)
        label = _clean_text(lm.group(1)) if lm else facet
        by_facet.setdefault(facet, []).append(label)
        if facet not in chip_facets:
            missing_chip.add(facet)
    # Two tiles on ONE page sharing a facet key: the click cannot honor the
    # merged-vs-open distinction the two labels promise (ka-0000008262).
    duplicate_keys = [
        {"facet": f, "labels": labels}
        for f, labels in sorted(by_facet.items())
        if len(labels) > 1
    ]
    return {
        "tile_count": all_tiles,
        "tile_facet_links": linked_tiles,
        "zero_count_tile_facet_links": zero_linked,
        "inert_tiles": all_tiles - linked_tiles,
        "spine_facet_chips": len(chip_facets - {""}),
        # Descriptive static context — NOT a fail flag (see the header comment):
        # a zero tile with no chip is the expected healthy DOM.
        "tiles_missing_facet_chip": sorted(missing_chip),
        # Authoritative STATIC detector for ka-0000008262 (no browser needed).
        "duplicate_tile_facet_keys": duplicate_keys,
    }


# --- Integrity: what does clicking each tile ACTUALLY do to the stream? -------
# The reason the old facet_match_ratio was blind (bug gd-XVZNaYe9nZ): a tile is a
# CLICK affordance, and only DRIVING the click sees what it does. The companion
# Playwright driver (tools/measure-tile-click-outcome.mjs) clicks every tile on
# /u/{slug} (zero-count IN scope) in a real browser and records the outcome; this
# reader folds its JSON into the measure step. Driving the real (possibly-fixed)
# tile is what keeps the signal FIX-INDEPENDENT: the two candidate product fixes
# for ka-0000003914 change the JS in different directions (make zero tiles inert
# vs. show a no-match state), and a static simulation of today's activate() would
# false-fail one of them.
#
# HEALTHY-SAFE INVARIANT. The driver scores each click against the population of
# the FACET it targets (the chip's own N), never against the tile's own printed
# count — because tile_count != facet population by design (the `mrs` facet folds
# two tiles, dedup drops lower-precedence duplicates, un-timestamped rows are
# dropped from the stream but still counted on the tile — user_home_activity.go).
# A correct click filters the stream to exactly its facet's rows; the
# silent-unfilter defect shows the FULL stream for a facet whose population is 0.
#
# tile_click_outcome_ratio = tiles whose post-click visible count is consistent
# with the facet they target / ALL rendered tiles. THIS is the number the
# integrity gate reads (< 1.0 ⇒ Management <= 4 and Accuracy <= 6, same ceiling
# as anchor_resolution_ratio). Null when the driver JSON is absent (the browser
# step did not run) — a WARN, never a zero-population fail.
def tile_click_outcomes(path: Path, warnings: list):
    """Read the Playwright tile-click driver's JSON and derive the gate metrics.

    Returns None when the file is absent/unreadable (the click outcome went
    UNMEASURED this run — a WARN so the next iteration runs the driver, never a
    fail), mirroring the null convention of the other integrity ratios.
    """
    if not path.exists():
        warnings.append(
            f"tile-click outcomes missing ({path.name}) — the per-tile CLICK "
            f"outcome went UNMEASURED this run; run tools/measure-tile-click-"
            f"outcome.mjs so tile_click_outcome_ratio is not null (bug "
            f"gd-XVZNaYe9nZ)"
        )
        return None
    try:
        data = json.loads(path.read_text() or "{}") or {}
    except Exception as e:
        warnings.append(f"tile-click outcomes unreadable ({path.name}): {e}")
        return None
    tiles = data.get("tiles") or []
    total = len(tiles)
    clickable = [t for t in tiles if t.get("clickable")]
    ok = [t for t in tiles if t.get("outcome_ok")]
    silent = [t for t in tiles if t.get("silently_unfilters")]
    mismatch = [t for t in clickable if not t.get("outcome_ok")]
    # The facet keys whose click outcome was wrong — the runtime analogue of the
    # old static "unmatched_tile_facets" (the click lands but the promised filter
    # never applies). Empty on BOTH valid product fixes.
    unmatched = sorted(
        {t.get("facet_link") for t in mismatch if t.get("facet_link")}
    )
    sample = [
        {
            "slug": t.get("slug"),
            "label": t.get("label"),
            "tile_count": t.get("tile_count"),
            "visible_after_click": t.get("visible_after_click"),
            "total": t.get("total"),
        }
        for t in silent[:BOT_ROW_SAMPLE_CAP]
    ]
    # Union the driver's per-slug duplicate-facet-key findings (a cross-check of
    # the authoritative static detector; the template-fixed tile set makes them
    # agree, but scanning all users proves the defect is not slug-specific).
    dup = data.get("duplicate_tile_facet_keys") or []
    ratio = round(len(ok) / total, 4) if total else None
    return {
        "source": path.name,
        "slugs_scanned": len(data.get("slugs") or []),
        "tiles_measured": total,
        "clickable_tiles": len(clickable),
        # THE gate ratio — over ALL rendered tiles, zero-count IN scope.
        "tile_click_outcome_ratio": ratio,
        # Backward-compatible alias so the gardener terminal_goal and the report
        # trend code keep reading one name; it now means the real click outcome,
        # not a static chip-existence count that could report a false 1.0.
        "facet_match_ratio": ratio,
        "tiles_silently_unfiltering": len(silent),
        "tiles_silently_unfiltering_sample": sample,
        "tiles_outcome_mismatch": len(mismatch),
        # Runtime "the promised filter never applies" set (fix-independent).
        "unmatched_tile_facets": unmatched,
        "duplicate_tile_facet_keys": dup,
        "driver_errors": data.get("errors") or [],
    }


# --- Integrity: the two endpoints of a link AGREE about who they describe ------
# The three ratios above ask "does this link resolve / is this route reachable /
# do the facet counts match" — never "do the two endpoints of the /u/{slug} link
# agree about the ENTITY they both name". That last question is a distinct
# CROSS-SURFACE consistency defect, structurally invisible to BOTH the screenshot
# rubric (nothing looks wrong in EITHER page alone — you must read both side by
# side) AND the other integrity ratios (every link resolved, every route was
# reachable, the facet counts matched — the page was pixel-perfect). It was real:
# iteration 2026-07-28T0226 scored anchor/route/facet 1.0-clean and STILL had
# /people render `Matthew.Baker@acme.example` as the card heading where
# /u/matthew.baker rendered `Matt Baker` — one person, two names, ~9 of 34 cards
# affected (ka-0000006552, fixed ce48a23f2). It was caught only because a judge
# happened to read both surfaces together; that is luck, not a detector. This
# ratio is the tripwire so the next handlePeople/displayName change that
# reintroduces the divergence reds the audit instead of scoring 1.0-clean again.
#
# The card heading is the `people-row-name` anchor's text ({{$row.Display}}); its
# href is the /u/{slug} link to compare against. The detail heading is the
# <h1>{{.DisplayName}} of that slug's page. The two resolve identity through
# DIFFERENT code paths (operatorGroup.displayName in people.go vs
# displayNameFromUserBead in user_home.go), so the ratio is a genuine consistency
# check, not a tautology — either path can drift and this catches it.
WS_RE = re.compile(r"\s+")
# The name anchor carries BOTH `people-row-name` (class) and the /u/{slug}
# `href` on ONE opening tag; capture the whole attribute run so href is found
# regardless of attribute order, then the inner heading text separately.
PEOPLE_NAME_LINK_RE = re.compile(
    r'<a\b([^>]*\bclass="[^"]*people-row-name[^"]*"[^>]*)>(.*?)</a>', re.S | re.I
)
HREF_ATTR_RE = re.compile(r'href="([^"]+)"', re.I)
SLUG_IN_HREF_RE = re.compile(r"/u/([^\"#?/]+)")
# The detail heading. <h1>{{.DisplayName}}{{if .IsViewerSelf}} <span
# class="meta">(you)</span>{{end}}</h1> — strip the self "(you)" badge so a
# self-render never reads as a mismatch against the peer-rendered card heading.
DETAIL_H1_RE = re.compile(r"<h1[^>]*>(.*?)</h1>", re.S | re.I)
H1_META_SPAN_RE = re.compile(
    r'<span[^>]*class="[^"]*\bmeta\b[^"]*"[^>]*>.*?</span>', re.S | re.I
)


def _clean_text(raw: str) -> str:
    """Tag-strip, unescape, collapse whitespace, trim — NO casefold.

    Mirrors ``_visible_text``'s strip-then-unescape order so both endpoints get
    identical treatment. Casefolding is deferred to the comparison so the raw
    (case-preserving) form can be emitted for a human to read in the mismatch
    list. Deliberately does NOT reduce an email to its local part: a card heading
    that is a raw email where the detail <h1> is a resolved display name is the
    EXACT miss this detector exists to catch, so `Matthew.Baker@acme.example` must
    stay a full string that fails to equal `Matt Baker`.
    """
    return WS_RE.sub(" ", html.unescape(TAG_INNER.sub(" ", raw))).strip()


def people_card_identities(idx_html: str) -> list:
    """Every /people card's (heading text, /u/{slug} link) pair.

    ``idx_html`` is the raw index capture; splits on ``PEOPLE_ROW_RE`` (same
    per-row block split as :func:`people_bot_rows`) so the heading and its href
    come from the SAME card. Returns one dict per card whose name anchor targets
    a /u/{slug}: ``heading`` (cleaned display text), ``slug``, ``href`` (the path
    to fetch). Cards whose heading link is not a /u/{slug} are skipped — there is
    no detail page to agree with.
    """
    body = _body(idx_html)
    starts = [m.start() for m in PEOPLE_ROW_RE.finditer(body)]
    starts.append(len(body))
    cards = []
    for i in range(len(starts) - 1):
        block = body[starts[i] : starts[i + 1]]
        m = PEOPLE_NAME_LINK_RE.search(block)
        if not m:
            continue
        attrs, inner = m.group(1), m.group(2)
        hm = HREF_ATTR_RE.search(attrs)
        if not hm:
            continue
        href = html.unescape(hm.group(1))
        sm = SLUG_IN_HREF_RE.search(href)
        if not sm:
            continue
        cards.append(
            {"heading": _clean_text(inner), "slug": sm.group(1), "href": href}
        )
    return cards


def detail_h1_text(detail_html: str):
    """The cleaned <h1> display heading of a /u/{slug} page, or None.

    Strips the self-only ``(you)`` badge (``H1_META_SPAN_RE``) so a self-render
    never manufactures a phantom mismatch. Returns None when the page has no
    <h1> — an auth wall or a non-rendered page — so that slug is recorded as
    UNVERIFIABLE (excluded from the ratio) rather than counted as a mismatch.
    """
    body = _body(detail_html)
    m = DETAIL_H1_RE.search(body)
    if not m:
        return None
    return _clean_text(H1_META_SPAN_RE.sub("", m.group(1)))


def cross_surface_identity(idx_html: str, fetch, cap, warnings: list) -> dict:
    """Do /people card headings agree with the <h1> of the /u/{slug} they link to?

    ``fetch(href) -> html|None`` returns the detail-page DOM for a card's href
    (injected so the unit tests stay hermetic; :func:`_make_detail_fetcher`
    supplies the real same-origin GET). ``cap`` bounds the fan-out on a large
    roster; the overflow is recorded, never silently dropped.

    ``cross_surface_identity_match_ratio`` = matching cards / cards COMPARED
    (heading text == detail <h1>, casefolded). Computed over the compared set —
    a card whose detail page could not be fetched/parsed is UNVERIFIABLE and
    excluded, so a total fetch outage reads as null ('unmeasurable'), never a
    zero-population fail, matching the other three ratios' null convention. The
    mismatching (card_heading, slug, detail_h1) triples are emitted verbatim so
    the file-issues step files a precise bug without re-deriving them.
    """
    cards = people_card_identities(idx_html)
    total = len(cards)
    out = {
        "cards_with_slug_link": total,
        "fetch_cap": cap,
        "cards_capped": 0,
        "cards_compared": 0,
        "matching_cards": 0,
        "identity_mismatches": [],
        "unverifiable_slugs": [],
        "cross_surface_identity_match_ratio": None,
    }
    if not cards:
        return out
    sampled = cards
    if cap is not None and total > cap:
        sampled = cards[:cap]
        out["cards_capped"] = total - cap
        warnings.append(
            f"cross_surface_identity: roster {total} cards exceeds fan-out cap "
            f"{cap}; compared the first {cap}, {total - cap} cards uncompared"
        )
    seen: dict = {}  # slug -> detail <h1> text (cache; avoids re-fetching a slug)
    compared = 0
    matching = 0
    mismatches = []
    unverifiable = []
    for c in sampled:
        slug = c["slug"]
        if slug in seen:
            h1 = seen[slug]
        else:
            det = fetch(c["href"])
            h1 = detail_h1_text(det) if det else None
            seen[slug] = h1
        if h1 is None:
            unverifiable.append(slug)
            continue
        compared += 1
        if c["heading"].casefold() == h1.casefold():
            matching += 1
        else:
            mismatches.append(
                {"card_heading": c["heading"], "slug": slug, "detail_h1": h1}
            )
    out["cards_compared"] = compared
    out["matching_cards"] = matching
    out["identity_mismatches"] = sorted(mismatches, key=lambda p: p["slug"])
    out["unverifiable_slugs"] = sorted(set(unverifiable))
    out["cross_surface_identity_match_ratio"] = (
        round(matching / compared, 4) if compared else None
    )
    if unverifiable:
        warnings.append(
            "cross_surface_identity: detail page unfetchable/unparseable for "
            f"{len(out['unverifiable_slugs'])} slug(s) "
            f"({','.join(out['unverifiable_slugs'][:5])}) — excluded from ratio"
        )
    return out


def _make_detail_fetcher(target_url: str):
    """Build the real per-card detail-page fetcher: a same-origin authenticated GET.

    The detail pages are cheap, same-origin, already-authenticated GETs behind
    the existing port-forward the harness stands up (``--target-url`` is the
    viewer). The href from the card is a path (basePath-aware), so it is joined
    to the target's ORIGIN — never re-prefixed — to survive a non-empty basePath.
    Returns None on any curl failure so the slug is recorded UNVERIFIABLE, not
    counted as a mismatch.
    """
    import urllib.parse

    sp = urllib.parse.urlsplit(target_url)
    origin = f"{sp.scheme}://{sp.netloc}"
    token = _bearer()

    def fetch(href: str):
        path = href.split("#", 1)[0].split("?", 1)[0]
        if not path.startswith("/"):
            path = "/" + path
        cmd = ["curl", "-fsS", "--max-time", "20"]
        if token:
            cmd += ["-H", f"Authorization: Bearer {token}"]
        cmd.append(origin + path)
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
            return res.stdout if res.returncode == 0 else None
        except Exception:
            return None

    return fetch


# --- /people (index) structural signals ---------------------------------------
PEOPLE_ROW_RE = re.compile(r'<li[^>]*class="[^"]*people-row[^"]*"', re.I)
PEOPLE_SUMMARY_CHIP_RE = re.compile(
    r'<span[^>]*class="[^"]*people-summary-([a-z]+)[^"]*"[^>]*>(.*?)</span>', re.S | re.I
)
HEADER_COUNT_RE = re.compile(r"<h1>\s*Users\s*<span[^>]*>\s*\((\d+)\)", re.I)
IDENTITY_CHIP_RE = re.compile(r'class="[^"]*people-identity[^"]*"', re.I)
STAT_CHIP_RE = re.compile(r'class="[^"]*people-stat-([a-z]+)[^"]*"', re.I)

# --- INDEPENDENT humans-only assertion (bug ka-0000001715) --------------------
# row_vs_truth_delta is STRUCTURALLY BLIND to a bot rendered as a human, because
# the page and its ground-truth query share ONE {oidc,slack} classifier — a bot
# identity that is kind=oidc with an email-shaped subject cancels out of the
# subtraction (see the PEOPLE_HUMAN_KINDS comment below). So a bot row can never
# move that number, and iterations 1151/1352 scored Coverage 9/10 with delta==0
# while the directory listed GitLab bot tokens and test fixtures the judge could
# read with its own eyes (ka-0000004034).
#
# bot_rows_rendered is the missing SECOND, independently-derived measurement: it
# classifies each RENDERED row by the OBJECTIVE non-human markers in the row's
# own identifier strings — NOT by operator kind — so it cannot cancel against the
# shared filter. The markers are definitional, not heuristic:
#   - an @noreply.* / *.noreply.* email domain (slack-U…@noreply.kbeads.local,
#     …@noreply.gitlab.com, …@users.noreply.github.com) — no human logs in as one;
#   - an RFC-2606 reserved fixture domain (example.com/org/net — e2e-fixture@…);
#   - a `_bot_` service-account token (GitLab `group_<n>_bot_<hash>@…`).
# These are exactly the sentinel/fixture domains ka-0000004034 screens, so the
# metric REPRODUCES the defect today (bot_rows_rendered > 0) and drops to 0 once
# that fix removes the noreply/fixture rows.
#
# DELIBERATELY NOT flagged: a service account on a REAL domain
# (info-density-audit@acme.example, gardenio@acme.example). No objective address
# pattern separates those from a human; deciding they are "junk" is a curation
# call that belongs in the judge's issues[], not in an objective harness signal
# that must also be able to reach 0. Widening the markers to catch them would
# make bot_rows_rendered false-positive on real employees and never reconcile
# with any address-based viewer fix.
BOT_ROW_MARKER_RE = re.compile(
    r"@noreply\."                    # …@noreply.gitlab.com, slack-U…@noreply.kbeads.local
    r"|\.noreply\."                  # …@users.noreply.github.com (noreply as a sub-label)
    r"|@example\.(?:com|org|net)\b"  # RFC-2606 fixture domains (e2e-fixture@example.com)
    r"|_bot_",                       # GitLab bot service accounts (group_<n>_bot_<hash>)
    re.I,
)

# The row's own IDENTIFIER strings — canonical email, folded identity handles,
# display name. bot_rows_rendered scans ONLY these, never the whole row block:
# a human's recent-bead title or thread-request preview lives in the same <li>
# and could legitimately quote "_bot_" or a noreply address, so scanning the
# block would false-flag a real person's row on their own content.
PEOPLE_ROW_MAILTO_RE = re.compile(r"mailto:([^\"?]+)", re.I)
PEOPLE_IDENTITY_TITLE_RE = re.compile(
    r'people-identity[^>]*\btitle="[^"]*:\s*([^"]+)"', re.I
)
PEOPLE_ROW_NAME_RE = re.compile(
    r'class="[^"]*people-row-name[^"]*"[^>]*>(.*?)</a>', re.S | re.I
)
# Cap the sample so a pathological page can never emit an unbounded blob; a bead
# title only needs a handful of offending values to quote.
BOT_ROW_SAMPLE_CAP = 20


def people_bot_rows(body: str) -> dict:
    """Count RENDERED /people rows whose identifier strings are objectively non-human.

    ``body`` is the STYLE/SCRIPT-stripped index DOM (``_body`` output). Splits it
    into per-row blocks on ``PEOPLE_ROW_RE`` so the result counts ROWS, not marker
    hits, then within each block scans only the row's own identifiers (email /
    folded identity handles / display name) against ``BOT_ROW_MARKER_RE``.

    Returns ``bot_rows_rendered`` (count) + ``bot_rows_sample`` (sorted, deduped,
    capped offending values, so a bead title can quote them). This is the
    correctness half of Coverage — independent of ``row_vs_truth_delta``'s
    consistency check, which shares the page's own {oidc,slack} classifier and so
    is blind to a misclassification both sides make (bug ka-0000001715).
    """
    starts = [m.start() for m in PEOPLE_ROW_RE.finditer(body)]
    starts.append(len(body))
    bot_rows = 0
    samples: set = set()
    for i in range(len(starts) - 1):
        block = body[starts[i] : starts[i + 1]]
        idents = set()
        for e in PEOPLE_ROW_MAILTO_RE.findall(block):
            idents.add(html.unescape(e).strip())
        for h in PEOPLE_IDENTITY_TITLE_RE.findall(block):
            idents.add(html.unescape(h).strip())
        for n in PEOPLE_ROW_NAME_RE.findall(block):
            idents.add(html.unescape(TAG_INNER.sub(" ", n)).strip())
        hits = sorted(v for v in idents if v and BOT_ROW_MARKER_RE.search(v))
        if hits:
            bot_rows += 1
            samples.update(hits)
    return {
        "bot_rows_rendered": bot_rows,
        "bot_rows_sample": sorted(samples)[:BOT_ROW_SAMPLE_CAP],
    }


# --- INDEPENDENT service-identity assertion (bug ka-0000004673) ---------------
# The COMPANION to bot_rows_rendered, for exactly the class it DELIBERATELY does
# not reach. bot_rows_rendered keys off objective non-human ADDRESS markers
# (noreply/fixture domains, _bot_ tokens); its BOT_ROW_MARKER_RE comment says it
# will not flag a service account on a REAL domain, because a wider regex would
# false-positive real employees on that domain and never reconcile with an
# address-based fix. So the four marker-invisible service/CI identities the
# deny-list fix removed (ka-0000004673, MR 6172) leave NO signal in
# bot_rows_rendered, and none in row_vs_truth_delta either — the page and its
# ground-truth query share the same {oidc,slack} classifier, so a deny-listed row
# that still renders is counted as a human on BOTH sides and cancels out of the
# subtraction (the same "delta stays 0 while junk renders" blind spot ka-0000001715
# and ka-0000004034 documented). Following "measure every important quantity two
# independent ways" (ka-0000005924), the deny-list needs its OWN page-level signal:
# service_rows_rendered reproduces the defect pre-fix (> 0, the four junk rows) and
# reads exactly 0 post-fix, independent of the shared classifier.
#
# LOCKSTEP: SERVICE_ROW_SUBJECTS mirrors serviceOperatorSubjects in
# kbeads/internal/server/http_operators.go EXACTLY. A subject on one side but not
# the other re-opens the same delta-blind gap the marker lockstep (ka-0000004034)
# closed — the harness would read 0 while the un-mirrored subject still renders,
# or flag a row the server already screens. Keys are lowercased; the scan folds
# each rendered identifier to lowercase before the membership test, mirroring the
# server's strings.ToLower(strings.TrimSpace(...)) lookup.
#
# EXACT match, NOT substring (the crucial contrast with the marker regex): this is
# a whole-identifier set-membership test, so it can never false-positive a real
# employee whose address merely CONTAINS a listed local-part — jordan.lake@acme.example
# stands untouched even though joel.approver@acme.example is listed, exactly as the
# server's map lookup does.
SERVICE_ROW_SUBJECTS = frozenset({
    "gardenio@acme.example",              # agent fleet's own identity / browse-only Jira account
    "gardenio2@acme.example",             # second Jira API-token identity (RWX-era deploy)
    "info-density-audit@acme.example",  # RWX-era info-density-audit OIDC claim
    "joel.approver@acme.example",        # CI approval fixture (SCIF approver@ convention)
})


def people_service_rows(body: str) -> dict:
    """Count RENDERED /people rows whose identifier is a known service/CI subject.

    Companion to :func:`people_bot_rows` for the class its address markers cannot
    reach: service/CI identities provisioned on a REAL corporate domain
    (ka-0000004673). Same per-row split and same identifier fields (canonical
    email / folded identity handles / display name), but the classifier is EXACT
    membership in the ``SERVICE_ROW_SUBJECTS`` deny-list (mirroring
    ``serviceOperatorSubjects`` server-side, case-folded) rather than a marker
    regex — so it cannot false-positive a real employee on the same domain and
    drops to exactly 0 once the deny-list fix removes the four rows.

    Returns ``service_rows_rendered`` (count) + ``service_rows_sample`` (sorted,
    deduped, capped offending subjects, so a bead title can quote them). Like
    bot_rows it counts ROWS, not identifier hits, and scans only the row's own
    identifiers — never the whole block — so a human quoting a service address in
    a recent-bead title does not flag.
    """
    starts = [m.start() for m in PEOPLE_ROW_RE.finditer(body)]
    starts.append(len(body))
    service_rows = 0
    samples: set = set()
    for i in range(len(starts) - 1):
        block = body[starts[i] : starts[i + 1]]
        idents = set()
        for e in PEOPLE_ROW_MAILTO_RE.findall(block):
            idents.add(html.unescape(e).strip())
        for h in PEOPLE_IDENTITY_TITLE_RE.findall(block):
            idents.add(html.unescape(h).strip())
        for n in PEOPLE_ROW_NAME_RE.findall(block):
            idents.add(html.unescape(TAG_INNER.sub(" ", n)).strip())
        hits = sorted(v for v in idents if v and v.lower() in SERVICE_ROW_SUBJECTS)
        if hits:
            service_rows += 1
            samples.update(hits)
    return {
        "service_rows_rendered": service_rows,
        "service_rows_sample": sorted(samples)[:BOT_ROW_SAMPLE_CAP],
    }


# --- raw Slack-mention leak in /people "recent requests" (bug ka-0000003119) --
# T1722 shipped 11 raw `<@U…>` tokens as literal text inside the .people-request-text
# preview snippets ("<@U0PERSON001> welcome to the team!" on Austin Tobitt's card).
# MR 6264 wired those snippets through resolveSlackMentions (people_mentions.go),
# so a token whose id is in the slack-alias feed now renders "@Name". The gardener
# has been RE-VERIFYING this by hand every iteration ("grep /people for the raw
# token pattern"); this signal makes that check INHERITED so a regression reds the
# audit instead of relying on a human remembering to grep.
#
# The subtlety the raw grep MISSES — and why a naive "any <@U…> ⇒ fail" flag would
# false-red: the viewer's resolver is deliberately conservative. resolveSlackMentions
# "never invents a name" — an id not in the alias feed (an EXTERNAL Slack user who
# was never an operator here) is left exactly as written. That is correct behavior,
# not the T1722 defect. So a raw token only means a REGRESSION when its id IS
# resolvable (present in the alias feed) yet still rendered raw. This classifier
# splits the two:
#   * raw_mentions_unresolved_resolvable — id IS in the resolver ⇒ the T1722 fix
#     regressed for that id (FAIL-FLAG). Empty on a healthy page.
#   * raw_mentions_unresolved_unknown    — id is NOT in the resolver ⇒ external /
#     unknown user, left raw BY DESIGN (informational; never a flag).
#
# Matcher LOCKSTEP with slackMentionRe (people_mentions.go): id is `[UW][A-Z0-9]+`
# with an optional `|label`, UPPERCASE-only so the lowercase placeholder junk
# `<@Uxxx>`/`<@Uxxxx>` in squawks/templated copy is left untouched exactly as the
# viewer leaves it. Scans ONLY .people-request-text snippet bodies — the single
# surface people.go routes through resolveSlackMentions — never the whole row, so a
# resolved `@Name` elsewhere or an id quoted in some other field cannot false-flag.
PEOPLE_REQUEST_TEXT_RE = re.compile(
    r'<span[^>]*class="[^"]*people-request-text[^"]*"[^>]*>(.*?)</span>', re.S | re.I
)
# Mirrors slackMentionRe: capture group 1 is the id, an optional `|label` is
# ignored. Applied to the HTML-UNESCAPED snippet text so `&lt;@U…&gt;` in the
# captured DOM matches the same `<@U…>` the browser renders.
SLACK_MENTION_TOKEN_RE = re.compile(r"<@([UW][A-Z0-9]+)(?:\|[^>]*)?>")


def raw_slack_mentions(idx_html: str, resolver_ids) -> dict:
    """Classify raw `<@U…>` tokens surviving in /people recent-request snippets.

    ``idx_html`` is the raw index capture; ``resolver_ids`` is the set of Slack
    user ids the viewer's mention resolver CAN resolve (the alias-feed subjects
    with a non-empty display name — see slack_mention_resolver_ids). A raw token
    whose id is in that set is a T1722 regression (the resolver had a name and did
    not use it); one whose id is not is an external/unknown user left raw by design.

    Returns counts + samples for both classes plus the raw scan totals. When
    ``resolver_ids`` is None (alias feed unreachable) the split is unmeasurable:
    ``resolver_available`` is False and every id is bucketed as unknown so the
    signal degrades to informational rather than manufacturing phantom regressions.
    """
    body = _body(idx_html)
    resolvable_hits: set = set()
    unknown_hits: set = set()
    total = 0
    have_resolver = resolver_ids is not None
    for raw_snippet in PEOPLE_REQUEST_TEXT_RE.findall(body):
        text = html.unescape(raw_snippet)
        for m in SLACK_MENTION_TOKEN_RE.finditer(text):
            total += 1
            uid = m.group(1)
            if have_resolver and uid in resolver_ids:
                resolvable_hits.add(uid)
            else:
                unknown_hits.add(uid)
    return {
        "resolver_available": have_resolver,
        "resolver_id_count": len(resolver_ids) if have_resolver else None,
        "raw_mention_tokens_total": total,
        "raw_mentions_unresolved_resolvable": len(resolvable_hits),
        "raw_mentions_unresolved_resolvable_sample": sorted(resolvable_hits)[
            :BOT_ROW_SAMPLE_CAP
        ],
        "raw_mentions_unresolved_unknown": len(unknown_hits),
        "raw_mentions_unresolved_unknown_sample": sorted(unknown_hits)[
            :BOT_ROW_SAMPLE_CAP
        ],
    }


def _first_int(text: str):
    m = re.search(r"([0-9][0-9,]*)", html.unescape(TAG_INNER.sub(" ", text)))
    return int(m.group(1).replace(",", "")) if m else None


def index_signals(idx_html: str) -> dict:
    body = _body(idx_html)
    rows = len(PEOPLE_ROW_RE.findall(body))
    chips = {}
    for kind, inner in PEOPLE_SUMMARY_CHIP_RE.findall(body):
        chips[kind] = _first_int(inner)
    hdr = HEADER_COUNT_RE.search(body)
    header_count = int(hdr.group(1)) if hdr else None
    stat_kinds = {}
    for kind in STAT_CHIP_RE.findall(body):
        stat_kinds[kind] = stat_kinds.get(kind, 0) + 1
    bot = people_bot_rows(body)
    service = people_service_rows(body)
    return {
        "rendered_row_count": rows,
        "header_count": header_count,
        # INDEPENDENT humans-only assertion — see people_bot_rows / BOT_ROW_MARKER_RE.
        # NOT routed through PEOPLE_HUMAN_KINDS, so it catches the bot-as-human
        # misclassification row_vs_truth_delta is blind to (bug ka-0000001715).
        "bot_rows_rendered": bot["bot_rows_rendered"],
        "bot_rows_sample": bot["bot_rows_sample"],
        # INDEPENDENT service-identity assertion — see people_service_rows /
        # SERVICE_ROW_SUBJECTS. The companion signal for the marker-invisible
        # service/CI accounts on real domains that bot_rows_rendered deliberately
        # cannot reach and row_vs_truth_delta cancels out (bug ka-0000004673).
        "service_rows_rendered": service["service_rows_rendered"],
        "service_rows_sample": service["service_rows_sample"],
        # The h1 "(N)" and the people-summary "people" chip are two independent
        # renderings of the same population; disagreement is an Accuracy defect
        # the operator would read as "the page can't count its own users".
        "header_vs_rows_delta": (
            header_count - rows if header_count is not None else None
        ),
        "summary_chips": chips,
        "summary_chip_count": len(chips),
        "identity_chip_count": len(IDENTITY_CHIP_RE.findall(body)),
        "stat_chip_kinds": stat_kinds,
        "rows_with_userhome_link": len(
            set(re.findall(r'href="(/u/[^"#?]+)"', body))
        ),
    }


# --- /u/{slug} (detail) structural signals ------------------------------------
STAT_TILE_RE = re.compile(r'<li[^>]*class="[^"]*user-home-stat[^"]*"', re.I)
ACTIVITY_ROW_RE = re.compile(r'<li[^>]*class="[^"]*activity-row[^"]*"', re.I)
RAIL_RE = re.compile(r'<li[^>]*class="[^"]*timeline-rail[^"]*"', re.I)
SPINE_RE = re.compile(r'<section[^>]*id="activity"', re.I)
EMPTY_SECTION_RE = re.compile(r'class="[^"]*user-home-section[^"]*is-empty', re.I)
FACET_CHIP_BTN_RE = re.compile(r'<button[^>]*class="[^"]*activity-facet[^"]*"', re.I)
# The Activity spine header prints "Activity (N)" (user_home_activity.html:
# <h2>Activity <span class="meta">(N)</span></h2>). Capturing N lets us assert,
# from a SINGLE capture, that the header the operator reads equals the number of
# rows the page actually enumerates — the /u/{slug} analogue of the /people
# header_vs_rows_delta. This closes the gap that let bug ka-0000006350 be filed:
# the tool recorded activity_row_count but had NO header-vs-rows check for the
# detail page, so a header/row mismatch could only be caught by a human
# eyeballing two SEPARATE captures — which manufactured a phantom off-by-one
# (measure pod's row count at T1 vs judge's re-capture at T2).
ACTIVITY_HEADER_RE = re.compile(r"Activity\s*<span[^>]*>\s*\((\d+)\)", re.I)


def detail_signals(det_html: str) -> dict:
    body = _body(det_html)
    activity_rows = len(ACTIVITY_ROW_RE.findall(body))
    hm = ACTIVITY_HEADER_RE.search(body)
    activity_header = int(hm.group(1)) if hm else None
    return {
        "stat_tile_count": len(STAT_TILE_RE.findall(body)),
        "activity_row_count": activity_rows,
        # activity_header_count is the integer the "Activity (N)" header prints;
        # activity_header_vs_rows_delta = header - enumerable rows, from ONE
        # capture. The invariant is structural (activityView.Total = len(deduped)
        # in user_home_activity.go, one <li class="activity-row"> per event), so a
        # NONZERO delta means the header is computed off a different population
        # than the rows — a real Accuracy defect (bug ka-0000006350). None when the
        # header is absent (empty-activity page renders no "(N)").
        "activity_header_count": activity_header,
        "activity_header_vs_rows_delta": (
            activity_header - activity_rows if activity_header is not None else None
        ),
        "timeline_rail_count": len(RAIL_RE.findall(body)),
        # The merged-spine invariant (ka-0000002041): exactly ONE #activity
        # section, not a scatter of per-section cards.
        "unified_spine_present": 1 if SPINE_RE.search(body) else 0,
        "spine_section_count": len(SPINE_RE.findall(body)),
        "facet_chip_count": len(FACET_CHIP_BTN_RE.findall(body)),
        "empty_section_count": len(EMPTY_SECTION_RE.findall(body)),
    }


# --- self/owner variant (bug gd-zFNvLriG4D) -----------------------------------
# The owner (IsViewerSelf=true) rendering of /u/{slug} carries affordances the
# peer rendering does not. Every prior CAMP iteration captured /u/{slug} under
# dev-auth-bypass with NO viewer identity, so IsViewerSelf was always false and
# these affordances went ungraded — the "self variant never graded" gap this bug
# names. The capture step now takes a SECOND detail render as page
# 'user-home-self' with the owner's OIDC email stamped on X-Auth-Request-Email
# (dev_auth.go promotes that header into the verified-identity context on the
# bypass path), so IsViewerSelf renders true and the owner affordances appear.
#
# The affordances are gated on {{if .IsViewerSelf}} in user_home.html /
# user_status_message_block.html. They are DEFINITIONAL, not heuristic:
#   * the "(you)" self-badge in the <h1> heading, and
#   * the self-only status-message editor button (set/edit), whose hx-get posts
#     to /u/{slug}/status-message/edit.
# A correct page shows these in the SELF capture and NONE of them in the peer
# capture. Two independent failure directions, both real defects:
#   - self_affordance_count == 0 in the self capture  ⇒ the owner rendering is
#     broken or the self path never resolves (the owner cannot edit their own
#     profile) — the coverage gap this bug exists to close.
#   - self_affordances_leaked_to_peer > 0             ⇒ an owner-only write
#     control renders for a NON-owner — an authorization-shaped UI leak far more
#     serious than a cosmetic miss (the server still rejects the write, but the
#     control should never render).
SELF_YOU_BADGE_RE = re.compile(r'<span[^>]*class="[^"]*\bmeta\b[^"]*"[^>]*>\s*\(you\)\s*</span>', re.I)
SELF_STATUS_EDIT_RE = re.compile(
    r'user-status-message-(?:set|edit)-button', re.I
)


def _self_affordances(page_html: str) -> dict:
    """Count the IsViewerSelf-gated owner affordances rendered on a detail page.

    ``page_html`` is a raw /u/{slug} capture (self OR peer). Returns the
    per-affordance counts plus a total, over the STYLE/SCRIPT-stripped body so a
    JS string literal mentioning "(you)" never counts as rendered markup.
    """
    body = _body(page_html)
    you_badge = len(SELF_YOU_BADGE_RE.findall(body))
    status_edit = len(SELF_STATUS_EDIT_RE.findall(body))
    return {
        "you_badge": you_badge,
        "status_edit_button": status_edit,
        "self_affordance_count": you_badge + status_edit,
    }


def self_variant_signals(self_html: str, peer_html: str) -> dict:
    """Grade the owner variant of /u/{slug} against the peer variant.

    ``self_html`` is the owner-email-injected capture (page 'user-home-self');
    ``peer_html`` is the dev-bypass no-identity capture (page 'user-home'). The
    invariant, from the {{if .IsViewerSelf}} template gates, is:
    owner affordances render in self, and NONE leak to peer.

    Returns null when the self capture is absent (the variant was not captured —
    e.g. SCREENSHOT_USER_SELF_EMAIL unset), so a missing capture reads as
    'ungraded', NEVER as a zero-affordance fail.
    """
    if not self_html:
        return None
    s = _self_affordances(self_html)
    p = _self_affordances(peer_html) if peer_html else {"self_affordance_count": 0}
    return {
        # Owner affordances present in the SELF capture (want > 0).
        "self_you_badge": s["you_badge"],
        "self_status_edit_button": s["status_edit_button"],
        "self_affordance_count": s["self_affordance_count"],
        # Owner affordances that LEAKED into the peer capture (want 0) — an
        # owner-only control rendering for a non-owner. A UI authz leak.
        "self_affordances_leaked_to_peer": p["self_affordance_count"],
        # True once the self render actually exercised the owner path: at least
        # one owner affordance appeared in self AND none leaked to peer. This is
        # the single boolean the judge reads for "the self variant was graded".
        "self_variant_ok": bool(
            s["self_affordance_count"] > 0 and p["self_affordance_count"] == 0
        ),
    }


# --- daemon ground truth ------------------------------------------------------
# /people is NOT a bead query — it renders the daemon's operator directory
# (handlePeople -> daemon.ListOperators), then applies two transforms before
# drawing a row. Ground truth MUST mirror both or Coverage false-fails:
#
#   1. filterHumanOperators (people.go): keep ONLY kind in {oidc, slack}. Bot and
#      sentinel kinds (system/fleet/gitlab/jira/agent/email) are deliberately out
#      of scope — counting them would demand ~dozens of phantom rows.
#   2. dedupOperators (people.go): collapse identities that share an identity key
#      into ONE canonical human row, preferring the oidc identity as primary.
#
# So: rendered rows should equal DEDUPED humans, and the "N identities merged"
# chip should equal the pre-dedup human identity count.
#
# CONSISTENCY, NOT CORRECTNESS (bug ka-0000001715). Because BOTH sides of
# row_vs_truth_delta apply this SAME human filter, the delta measures only
# whether the PAGE agrees with its own ground-truth query — it is the right tool
# for render/dedup drift and blind, by construction, to a misclassification the
# two sides SHARE. A bot identity that is kind=oidc with an email-shaped subject
# is counted as a human by the page AND by the truth query, so it cancels out and
# the delta stays 0. A zero delta here means "the roster agrees with itself", NOT
# "the roster is humans-only". Humans-only CORRECTNESS is asserted separately and
# independently by bot_rows_rendered (see BOT_ROW_MARKER_RE / bot_rows below),
# which pattern-matches the rendered identifier strings rather than routing
# through this filter.
PEOPLE_HUMAN_KINDS = {"oidc", "slack"}


def _identity_key(op: dict) -> str:
    """Mirror peopleIdentityKey (people.go) EXACTLY — all three fallbacks.

    Transcribed from the viewer rather than approximated, because Coverage's
    whole value is that a nonzero row_vs_truth_delta means the PAGE is wrong. If
    this key differs from the page's, the delta measures the difference between
    two dedup rules instead, and the signal silently becomes noise.

    The viewer's order is: canonical email → an @-bearing SUBJECT (kind=oidc rows
    carry the email only in Subject on some rows) → lowercased Display name →
    "" (which the caller replaces with a per-id key so unkeyable rows stay their
    own singleton and are never over-merged).

    Note it deliberately does NOT special-case noreply.kbeads.local: the
    human-kind filter ({oidc, slack}) has already removed the bot rows that
    carry those addresses, so excluding them here would diverge from the page.
    """
    email = (op.get("email") or "").strip().lower()
    if email:
        return "email:" + email
    subject = (op.get("subject") or "").strip().lower()
    if "@" in subject:
        return "email:" + subject
    display = (op.get("display") or "").strip().lower()
    if display:
        return "name:" + display
    return "id:" + str(op.get("id"))


def daemon_operator_truth(base_url: str, warnings: list) -> dict:
    """Fetch /v1/operators and reproduce the page's human-filter + dedup.

    Returns {"identities": N, "humans": M} — or nulls when the daemon is
    unreachable. A null truth means 'unmeasurable' to the judge; it must never
    read as a zero-population fail (that would red every run on a tunnel blip).
    """
    unknown = {"identities": None, "humans": None}
    url = base_url.rstrip("/") + "/v1/operators?limit=500"
    cmd = ["curl", "-fsS", "--max-time", "30"]
    token = _bearer()
    if token:
        cmd += ["-H", f"Authorization: Bearer {token}"]
    cmd.append(url)
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        if out.returncode != 0:
            warnings.append(
                f"GET /v1/operators failed (rc={out.returncode}): {out.stderr.strip()[:200]}"
            )
            return unknown
        ops = (json.loads(out.stdout or "{}") or {}).get("operators") or []
    except Exception as e:
        warnings.append(f"operator ground truth unavailable: {e}")
        return unknown
    humans = [o for o in ops if (o.get("kind") or "") in PEOPLE_HUMAN_KINDS]
    return {
        "identities": len(humans),
        "humans": len({_identity_key(o) for o in humans}),
    }


def slack_mention_resolver_ids(base_url: str, warnings: list):
    """Fetch the exact id set the viewer's mention resolver CAN resolve.

    Mirrors buildSlackMentionResolver (people_mentions.go): the kind=slack alias
    feed (GET /v1/admin/operators/slack-aliases), keyed by ``subject``, keeping
    ONLY rows with a genuine non-empty ``display`` — the viewer skips the resync
    sentinel rows (empty display) rather than emit a synthetic handle, so a token
    for a sentinel-only id renders raw on the page too and must NOT be judged a
    resolvable regression here. Returns the set of resolvable subjects, or None
    when the feed is unreachable (the split then degrades to informational — a
    null is 'unmeasurable', never a zero-population fail on a tunnel blip).
    """
    url = base_url.rstrip("/") + "/v1/admin/operators/slack-aliases"
    cmd = ["curl", "-fsS", "--max-time", "30"]
    token = _bearer()
    if token:
        cmd += ["-H", f"Authorization: Bearer {token}"]
    cmd.append(url)
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        if out.returncode != 0:
            warnings.append(
                "GET /v1/admin/operators/slack-aliases failed "
                f"(rc={out.returncode}): {out.stderr.strip()[:200]}"
            )
            return None
        aliases = (json.loads(out.stdout or "{}") or {}).get("aliases") or []
    except Exception as e:
        warnings.append(f"slack mention resolver feed unavailable: {e}")
        return None
    ids = set()
    for a in aliases:
        subject = (a.get("subject") or "").strip()
        display = (a.get("display") or "").strip()
        if subject and display:
            ids.add(subject)
    return ids


def _bearer() -> str:
    import os

    for env in ("BEADS_VIEWER_BEARER", "BEADS_AUTH_TOKEN", "GARDENIO_TOKEN", "KBEADS_TOKEN"):
        v = os.environ.get(env)
        if v:
            return v
    return ""


# --- correctness --------------------------------------------------------------
LEAK_RE = re.compile(r"\{\{|\}\}|<no value>")

# Error-marker scanning is DELIBERATELY narrower here than on the gardeners page.
# Both user surfaces render user/agent-authored CONTENT verbatim — the activity
# spine shows bead titles and full message bodies, and /people shows status
# messages. Live validation against /u/matthew.baker proved the point: a bead
# body legitimately quoting "Error: listing beads: HTTP 502" tripped the generic
# scan. That is the page doing its job (transparency is this epic's headline
# goal), not a page defect, and a signal that fires on healthy pages every run is
# one the judge learns to ignore.
#
# So instead of bare words, match only strings that indicate the TEMPLATE itself
# failed — Go's render-failure surface. These cannot appear in prose the way
# "error" can.
ERR_RE = re.compile(
    r"<no value>"
    r"|\bTemplate execution failed\b"
    r"|\bcan't evaluate field\b"
    r"|\bexecuting \"[^\"]+\" at\b"
    r"|\bruntime error:\b"
    r"|\binvalid memory address or nil pointer\b",
    re.I,
)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True, help="captures dir (html/, screenshots/)")
    ap.add_argument("--out", required=True, help="output metrics.json path")
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--user-slug", default="", help="slug audited as the detail page")
    ap.add_argument(
        "--daemon-url",
        default="http://localhost:8080",
        help="beads daemon base URL for operator ground truth (GET /v1/operators)",
    )
    ap.add_argument(
        "--subroutes",
        default="inbox",
        help="comma-separated /u/{slug}/<sub> routes that must be reachable by click",
    )
    ap.add_argument(
        "--identity-fetch-cap",
        type=int,
        default=60,
        help=(
            "max /u/{slug} detail pages the cross_surface_identity ratio fetches "
            "(bounds fan-out on a large roster; the overflow is recorded, never "
            "silently dropped). 0 or negative disables the cap."
        ),
    )
    ap.add_argument(
        "--tile-outcomes",
        default="",
        help=(
            "path to the Playwright tile-click driver's JSON "
            "(tools/measure-tile-click-outcome.mjs output). Provides "
            "tile_click_outcome_ratio — the number the integrity gate reads for "
            "the per-tile CLICK outcome (bug gd-XVZNaYe9nZ). Defaults to "
            "<captures>/tile-click-outcomes.json; a missing file leaves the ratio "
            "null (WARN, never a zero-population fail)."
        ),
    )
    args = ap.parse_args()

    captures = Path(args.captures)
    warnings: list = []

    idx_html = _read(captures / "html" / f"{INDEX_PAGE}.html")
    det_html = _read(captures / "html" / f"{DETAIL_PAGE}.html")
    # SELF_PAGE is OPTIONAL (bug gd-zFNvLriG4D): the owner (IsViewerSelf=true)
    # capture only exists when the capture step ran with SCREENSHOT_USER_SELF_EMAIL.
    # A missing self capture means the owner variant went ungraded THIS run — a
    # WARN (so the next iteration knows to inject the owner email), never a fail.
    self_html = _read(captures / "html" / f"{SELF_PAGE}.html")
    if not idx_html:
        warnings.append(f"{INDEX_PAGE}.html missing or empty")
    if not det_html:
        warnings.append(f"{DETAIL_PAGE}.html missing or empty")
    if not self_html:
        warnings.append(
            f"{SELF_PAGE}.html missing — owner (self) variant of /u/{{slug}} NOT "
            f"graded this run; set SCREENSHOT_USER_SELF_EMAIL in the capture step "
            f"to grade it (bug gd-zFNvLriG4D)"
        )

    polish = {
        INDEX_PAGE: polish_for(captures, INDEX_PAGE, warnings),
        DETAIL_PAGE: polish_for(captures, DETAIL_PAGE, warnings),
    }

    idx = index_signals(idx_html) if idx_html else {}
    det = detail_signals(det_html) if det_html else {}

    truth = daemon_operator_truth(args.daemon_url, warnings)
    kd_total = truth["humans"]
    rendered = idx.get("rendered_row_count", 0)
    merged_chip = (idx.get("summary_chips") or {}).get("merged")
    coverage = {
        "rendered_row_count": rendered,
        "daemon_human_count": kd_total,
        "daemon_identity_count": truth["identities"],
        "row_vs_truth_delta": (kd_total - rendered) if kd_total is not None else None,
        # INDEPENDENT humans-only CORRECTNESS assertion (bug ka-0000001715). Unlike
        # row_vs_truth_delta — which shares the page's {oidc,slack} filter and so
        # only proves the page agrees with itself — this classifies rendered rows
        # by objective non-human markers (noreply/fixture domains, _bot_ tokens)
        # and CANNOT cancel against that shared filter. > 0 means the directory
        # lists bot/service/fixture rows as humans; it is a CEILING on Coverage
        # (cap 7) in the rubric, not an advisory note, so a dense screenshot cannot
        # argue Coverage back to 9 while bot tokens fill the roster.
        "bot_rows_rendered": idx.get("bot_rows_rendered", 0),
        "bot_rows_sample": idx.get("bot_rows_sample", []),
        # SECOND, INDEPENDENT correctness assertion for the DENY-LIST fix
        # (bug ka-0000004673). bot_rows_rendered above deliberately does NOT flag a
        # service account on a real domain (a wider marker would false-positive real
        # employees), and row_vs_truth_delta cancels a deny-listed row out because
        # the page and its truth query share the {oidc,slack} classifier. So the
        # deny-list fix had server tests but NO page-level signal until this: it is
        # exact set-membership in SERVICE_ROW_SUBJECTS (mirroring the server's
        # serviceOperatorSubjects in lockstep), so it reproduces the four junk rows
        # pre-fix and reads 0 post-fix. Also a CEILING on Coverage in the rubric —
        # a provisioned CI identity rendering as a colleague dilutes the "every
        # human, no junk" claim just as a bot token does.
        "service_rows_rendered": idx.get("service_rows_rendered", 0),
        "service_rows_sample": idx.get("service_rows_sample", []),
        # The "N identities merged" chip claims how many raw identities collapsed
        # into the rows above; it should equal the daemon's human-identity count.
        # A mismatch means the dedup story the page tells is not the dedup it did.
        "identities_chip": merged_chip,
        "identities_chip_vs_truth_delta": (
            truth["identities"] - merged_chip
            if (merged_chip is not None and truth["identities"] is not None)
            else None
        ),
        "header_count": idx.get("header_count"),
        "header_vs_rows_delta": idx.get("header_vs_rows_delta"),
        "summary_chips": idx.get("summary_chips", {}),
        "summary_chip_count": idx.get("summary_chip_count", 0),
        "rows_with_userhome_link": idx.get("rows_with_userhome_link", 0),
    }

    subroutes = [s.strip() for s in args.subroutes.split(",") if s.strip()]
    # The fourth integrity ratio needs the DETAIL page for EVERY linked slug, so
    # it fetches them here (the other three read only the two already-captured
    # DOMs). Same-origin, authenticated GETs behind the port-forward the harness
    # already stood up; the fetcher returns None on failure so a slug reads
    # UNVERIFIABLE, never a mismatch. A non-positive cap disables the bound.
    cap = args.identity_fetch_cap if args.identity_fetch_cap > 0 else None
    cross_identity = (
        cross_surface_identity(
            idx_html, _make_detail_fetcher(args.target_url), cap, warnings
        )
        if idx_html
        else None
    )
    # The per-tile CLICK outcome (bug gd-XVZNaYe9nZ). Read the Playwright
    # driver's JSON from disk — default <captures>/tile-click-outcomes.json —
    # the same fetch-from-disk contract every other detail signal uses. Null when
    # the driver did not run (WARN, never a zero-population fail).
    tile_outcomes_path = (
        Path(args.tile_outcomes)
        if args.tile_outcomes
        else captures / "tile-click-outcomes.json"
    )
    tiles_click = tile_click_outcomes(tile_outcomes_path, warnings)

    integrity = {
        INDEX_PAGE: anchor_integrity(idx_html) if idx_html else None,
        DETAIL_PAGE: anchor_integrity(det_html) if det_html else None,
        "routes": (
            route_reachability(det_html, args.user_slug, subroutes)
            if det_html and args.user_slug
            else None
        ),
        # STATIC structural context for the tiles (duplicate facet keys — the
        # authoritative detector for ka-0000008262 — plus descriptive counts).
        # NO LONGER the gate ratio: chip existence != correct filtering, so this
        # can no longer report a false facet_match_ratio 1.0.
        "facets": facet_integrity(det_html) if det_html else None,
        # THE facet gate: what clicking every tile ACTUALLY does to the stream,
        # driven in a real browser (fix-independent). tile_click_outcome_ratio
        # here is the number the judge's integrity gate reads. Null when the
        # driver JSON is absent.
        "tile_click_outcomes": tiles_click,
        # Cross-surface identity: /people card heading == <h1> of the /u/{slug}
        # it links to (ka-0000006552). The one integrity question the other three
        # ratios never ask — do the two ENDPOINTS of a link agree about the
        # entity they both name — and a ratio < 1.0 is the exact defect a
        # pixel-perfect, all-links-resolve page hides (bug gd-VJy99tfmnF).
        "cross_surface_identity": cross_identity,
    }

    accuracy = {
        "identity_chip_count": idx.get("identity_chip_count", 0),
        "stat_chip_kinds": idx.get("stat_chip_kinds", {}),
        "stat_tile_count": det.get("stat_tile_count", 0),
        "activity_row_count": det.get("activity_row_count", 0),
        # Single-capture header-vs-rows invariant for the Activity spine — the
        # /u/{slug} analogue of coverage.header_vs_rows_delta on /people. A
        # nonzero delta is an objective off-by-one the page shows against itself
        # (bug ka-0000006350); the judge must NOT compare row counts across two
        # separate captures, which is what manufactured the phantom defect.
        "activity_header_count": det.get("activity_header_count"),
        "activity_header_vs_rows_delta": det.get("activity_header_vs_rows_delta"),
        "unified_spine_present": det.get("unified_spine_present", 0),
        "spine_section_count": det.get("spine_section_count", 0),
    }

    # Owner (self) variant of /u/{slug} — the affordances gated on IsViewerSelf
    # that the peer capture never renders (bug gd-zFNvLriG4D). None when the self
    # capture is absent (variant ungraded), which the judge reads as 'not
    # exercised this run', not a fail.
    self_variant = self_variant_signals(self_html, det_html)

    management = {
        "facet_chip_count": det.get("facet_chip_count", 0),
        "timeline_rail_count": det.get("timeline_rail_count", 0),
        "empty_section_count": det.get("empty_section_count", 0),
        "summary_chip_count": idx.get("summary_chip_count", 0),
        # The owner MUST be able to act on their own profile — the self-only
        # write affordances (status editor + "(you)" badge) render for the owner
        # and for NO peer. self_variant_ok folds both directions into one
        # boolean; null when the variant was not captured (see self_variant).
        "self_variant_ok": (
            self_variant.get("self_variant_ok") if self_variant else None
        ),
    }

    idx_body, det_body = _body(idx_html), _body(det_html)
    idx_text, det_text = _visible_text(idx_html), _visible_text(det_html)
    template_leaks = len(LEAK_RE.findall(idx_body)) + len(LEAK_RE.findall(det_body))
    error_markers = len(ERR_RE.findall(idx_text)) + len(ERR_RE.findall(det_text))
    empty_directory = 1 if (rendered == 0 and (kd_total or 0) > 0) else 0

    # Raw Slack-mention leak in /people recent-request snippets (bug ka-0000003119 /
    # T1722). Makes the gardener's per-iteration hand grep an INHERITED signal that
    # distinguishes a genuine regression (resolvable id rendered raw) from an
    # external/unknown id the resolver leaves raw by design. Feed unreachable ⇒
    # resolver_ids None ⇒ signal degrades to informational (never a zero-pop fail).
    resolver_ids = slack_mention_resolver_ids(args.daemon_url, warnings) if idx_html else None
    raw_mentions = raw_slack_mentions(idx_html, resolver_ids) if idx_html else {}

    correctness = {
        "template_leaks": template_leaks,
        "error_markers": error_markers,
        "empty_directory": empty_directory,
        # T1722 regression guard — see raw_slack_mentions. The resolvable-but-raw
        # count is the only fail-worthy class; the unknown-id count is a by-design
        # informational note (external Slack users the resolver never had a name for).
        "raw_slack_mentions": raw_mentions,
    }

    out = {
        "target_url": args.target_url,
        "iteration": args.iteration,
        "user_slug": args.user_slug,
        "index_page": INDEX_PAGE,
        "detail_page": DETAIL_PAGE,
        "polish": polish,
        "coverage": coverage,
        "accuracy": accuracy,
        "management": management,
        "integrity": integrity,
        "correctness": correctness,
        # Owner (self) variant of /u/{slug}, or null when it was not captured
        # this run (bug gd-zFNvLriG4D). The detailed per-affordance counts live
        # here; management.self_variant_ok is the single boolean the judge reads.
        "self_variant": self_variant,
    }
    if warnings:
        out["warnings"] = warnings

    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")

    # surface fail flags + warnings on stderr (informational; never blocks)
    flags = []
    if template_leaks:
        flags.append(f"template_leaks={template_leaks}")
    if error_markers:
        flags.append(f"error_markers={error_markers}")
    if empty_directory:
        flags.append("empty_directory=1")
    # Raw Slack-mention regression (bug ka-0000003119 / T1722). ONLY the
    # resolvable-but-raw class flags: a token whose id is in the alias feed yet
    # still rendered raw means the resolveSlackMentions wiring regressed for that
    # id. An unknown/external id left raw is by design and never flags — that is
    # the distinction a naive `grep '<@U'` misses and would false-red on.
    rsm = correctness.get("raw_slack_mentions") or {}
    resolvable_raw = rsm.get("raw_mentions_unresolved_resolvable") or 0
    if resolvable_raw:
        sample = rsm.get("raw_mentions_unresolved_resolvable_sample") or []
        flags.append(
            f"raw_slack_mentions_resolvable={resolvable_raw} ({','.join(sample[:5])})"
        )
    # Bot/service/fixture rows rendered as humans (bug ka-0000001715). Objective
    # and independently verifiable (the sample is a grep-able list), so it flags
    # regardless of any subjective score — it is the Coverage ceiling the judge
    # reads. row_vs_truth_delta can be 0 while this is nonzero; that is the whole
    # point (the shared classifier hides them from the delta).
    bot_rows = coverage.get("bot_rows_rendered") or 0
    if bot_rows:
        sample = coverage.get("bot_rows_sample") or []
        flags.append(
            f"bot_rows_rendered={bot_rows} ({','.join(sample[:5])})"
        )
    # Service/CI identities on real domains rendered as humans (bug ka-0000004673).
    # The companion to bot_rows_rendered for the marker-invisible class; likewise
    # objective (the sample is a grep-able list of exact deny-list subjects) and
    # independent of row_vs_truth_delta, which cancels these rows out.
    service_rows = coverage.get("service_rows_rendered") or 0
    if service_rows:
        sample = coverage.get("service_rows_sample") or []
        flags.append(
            f"service_rows_rendered={service_rows} ({','.join(sample[:5])})"
        )
    for page in (INDEX_PAGE, DETAIL_PAGE):
        anc = integrity.get(page)
        if anc and anc.get("dangling_fragment_links"):
            flags.append(
                f"{page}.dangling_fragment_links={anc['dangling_fragment_links']}"
                f" ({','.join(anc['dangling_fragments'][:5])})"
            )
    routes = integrity.get("routes")
    if routes and routes.get("orphaned_subroutes"):
        flags.append("orphaned_subroutes=" + ",".join(routes["orphaned_subroutes"]))
    # Two tiles on one page sharing a facet key (ka-0000008262). STATIC — always
    # available, even without the browser driver — so it flags on the captured
    # DOM alone. Names the shared key + the tiles a human sees.
    facets = integrity.get("facets")
    if facets and facets.get("duplicate_tile_facet_keys"):
        dk = facets["duplicate_tile_facet_keys"]
        sample = ";".join(
            f"{d['facet']}=[{'/'.join(d['labels'])}]" for d in dk[:3]
        )
        flags.append(f"duplicate_tile_facet_keys={len(dk)} ({sample})")
    # The per-tile CLICK outcome (bug gd-XVZNaYe9nZ) — the gate signal. A tile
    # whose click does not filter the stream to its facet's population is a lie
    # the DOM/pixels cannot show. tiles_silently_unfiltering is the specific
    # ka-0000003914 shape (a zero tile's click reveals the whole stream). Both
    # are objective and fix-independent; they flag regardless of any score. Only
    # fires when the driver ran (tc is not None) — a missing driver JSON is a
    # WARN below, not a fail, so the ratio reads null == unmeasurable.
    tc = integrity.get("tile_click_outcomes")
    if tc is not None:
        if tc.get("tiles_silently_unfiltering"):
            n = tc["tiles_silently_unfiltering"]
            samp = tc.get("tiles_silently_unfiltering_sample") or []
            names = ",".join(
                f"{s.get('slug')}/{s.get('label')}" for s in samp[:5]
            )
            flags.append(f"tiles_silently_unfiltering={n} ({names})")
        # Any outcome mismatch that is NOT already counted as a silent unfilter
        # (e.g. a filter that shows the wrong non-zero count).
        extra = (tc.get("tiles_outcome_mismatch") or 0) - (
            tc.get("tiles_silently_unfiltering") or 0
        )
        if extra > 0:
            flags.append(f"tile_click_outcome_mismatch={tc['tiles_outcome_mismatch']}")
        if tc.get("unmatched_tile_facets"):
            flags.append(
                "unmatched_tile_facets=" + ",".join(tc["unmatched_tile_facets"])
            )
        if tc.get("driver_errors"):
            flags.append(
                f"tile_click_driver_errors={len(tc['driver_errors'])}"
            )
    # Cross-surface identity mismatch (bug ka-0000006552): a /people card heads
    # with one name where the /u/{slug} it links to heads with another. Objective
    # and grep-verifiable (each triple names the slug + both strings), so it flags
    # regardless of any subjective score — it is the Accuracy ceiling the judge
    # reads. Quote a few "card|slug|h1" triples so a bead title can lift them.
    xident = integrity.get("cross_surface_identity")
    if xident and xident.get("identity_mismatches"):
        pairs = xident["identity_mismatches"]
        sample = ";".join(
            f"{p['card_heading']}|{p['slug']}|{p['detail_h1']}" for p in pairs[:3]
        )
        flags.append(f"cross_surface_identity_mismatches={len(pairs)} ({sample})")
    if det and not det.get("unified_spine_present"):
        flags.append("unified_spine_present=0")
    # Polish gates this lane's terminal goal and is scored from PNG pixels. A
    # total pixel blackout (0 of 4 blocks measured) means the goal-gating
    # dimension went dark — a HARNESS failure that must red the run, not a
    # quiet warning that reports CLEAN (bug gd-DQLUhVH4mc). A partial loss stays
    # a per-PNG warning below; the judge skips individual nulls.
    if polish_pixels_all_null(polish):
        flags.append("polish_pixels_all_null=1 (no PNG reached the measure pod)")
    # A single-capture Activity header/row disagreement (bug ka-0000006350). The
    # `is not None` guard keeps a clean 0 (matching) and an absent header (empty
    # page) from ever flagging — only a genuine nonzero drift surfaces.
    ahd = accuracy.get("activity_header_vs_rows_delta")
    if ahd:
        flags.append(
            f"activity_header_vs_rows_delta={ahd} "
            f"(header {accuracy.get('activity_header_count')} vs "
            f"{accuracy.get('activity_row_count')} rows)"
        )
    # Self/owner-variant defects (bug gd-zFNvLriG4D). Only flag when the variant
    # was actually captured (self_variant is not None); a missing self capture is
    # a WARN above, not a fail — it means the variant went ungraded, which is a
    # coverage note for the next iteration, not a page defect. Two directions:
    #   * self capture rendered ZERO owner affordances — the owner path never
    #     resolved (IsViewerSelf never went true) or the owner rendering broke;
    #   * an owner-only control LEAKED into the peer capture — a UI authz leak.
    if self_variant is not None:
        if self_variant.get("self_affordance_count", 0) == 0:
            flags.append(
                "self_affordance_count=0 (owner variant of /u/{slug} rendered no "
                "IsViewerSelf-gated affordance — owner path did not resolve)"
            )
        leaked = self_variant.get("self_affordances_leaked_to_peer", 0)
        if leaked:
            flags.append(
                f"self_affordances_leaked_to_peer={leaked} (owner-only control "
                f"rendered for a non-owner — UI authorization leak)"
            )
    if flags:
        print("FAIL-FLAGS: " + ", ".join(flags), file=sys.stderr)
    for w in warnings:
        print(f"WARN: {w}", file=sys.stderr)


if __name__ == "__main__":
    main()
