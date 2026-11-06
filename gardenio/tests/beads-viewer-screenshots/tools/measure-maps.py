#!/usr/bin/env python3
"""Compute objective maps-beauty metrics for the CLUB audit harness.

Layered on top of measure-density.py: ingests a maps-beauty captures dir
(html/ + screenshots/desktop|narrow) and emits a metrics.json roll-up across
four dimensions — Beauty/Density, Gardener-fidelity, Linkage-fidelity, and
Correctness — for the /maps fleet map and /maps/{id} per-gardener radial.

Per gd-UvgtIJMALI (Step 3: Measure). The judge skips null screenshot blocks,
so an unreadable capture is recorded as null + a warning, never a hard fail.
"""
import argparse
import json
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path

try:
    from PIL import Image
except ImportError:
    print("Pillow required: pip install Pillow", file=sys.stderr)
    sys.exit(2)

# Full-page screenshots can exceed Pillow's default decompression-bomb guard.
Image.MAX_IMAGE_PIXELS = None

try:
    import numpy as np
except ImportError:
    np = None

# ---------------------------------------------------------------------------
# Beauty/Density — reuse the measure-density.py regexes verbatim.
# ---------------------------------------------------------------------------
TAG_RE = re.compile(rb"<[a-zA-Z][^>]*>")
HREF_RE = re.compile(rb'href="([^"]+)"')
DATA_FIELD_RE = re.compile(rb"<(td|dd|li|dt)\b", re.IGNORECASE)
TEXT_STRIP_RE = re.compile(rb"<[^>]+>")

# Strip <style> / <script> blocks so "correctness" greps only see rendered body.
STYLE_RE = re.compile(r"<style[^>]*>.*?</style>", re.IGNORECASE | re.DOTALL)
SCRIPT_RE = re.compile(r"<script[^>]*>.*?</script>", re.IGNORECASE | re.DOTALL)

# Fleet map verdict vocabulary (data-verdict on each <a class="fleet-node">).
VERDICT_KEYS = ["progress", "stalled", "stuck", "neutral", "needs-attention"]


def html_density(path: Path) -> dict:
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


def png_density(path: Path) -> dict:
    with Image.open(path) as im:
        im = im.convert("RGB")
        w, h = im.size
        total = w * h
        if not total:
            return {"pixel_count": 0, "near_white_ratio": None, "mean_luma": None}
        if np is not None:
            arr = np.asarray(im, dtype=np.uint8)
            near_white = int(np.all(arr >= 240, axis=2).sum())
            luma = 0.2126 * arr[..., 0] + 0.7152 * arr[..., 1] + 0.0722 * arr[..., 2]
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


def beauty_block(slug: str, html_dir: Path, desktop_dir: Path, narrow_dir: Path,
                 warnings: list) -> dict:
    """{desktop, narrow} density per screenshot, plus shared html density."""
    entry = {"html": None, "desktop": None, "narrow": None}
    hp = html_dir / f"{slug}.html"
    if hp.exists():
        try:
            entry["html"] = html_density(hp)
        except Exception as e:  # noqa: BLE001
            warnings.append(f"{slug}.html density failed: {e}")
    for vp, vp_dir in (("desktop", desktop_dir), ("narrow", narrow_dir)):
        p = vp_dir / f"{slug}.png"
        if not p.exists():
            warnings.append(f"missing screenshot {vp}/{slug}.png — block null")
            continue
        try:
            entry[vp] = png_density(p)
        except Exception as e:  # noqa: BLE001
            warnings.append(f"{vp}/{slug}.png unreadable: {e} — block null")
            entry[vp] = None
    return entry


# ---------------------------------------------------------------------------
# Helpers operating on rendered body (style/script stripped).
# ---------------------------------------------------------------------------
def rendered_body(html: str) -> str:
    return SCRIPT_RE.sub("", STYLE_RE.sub("", html))


def visible_text(body: str) -> str:
    return re.sub(r"<[^>]+>", " ", body)


def svg_blocks(html: str) -> list:
    return re.findall(r"<svg\b.*?</svg>", html, re.DOTALL)


# ---------------------------------------------------------------------------
# Gardener-fidelity — the /maps fleet map.
# ---------------------------------------------------------------------------
def gardener_fidelity(fleet_html: str, kd_truth: int) -> dict:
    body = rendered_body(fleet_html)
    # Each gardener is an <a class="fleet-node fleet-verdict-X ..."> with a
    # data-verdict attribute. The class list carries the verdict + an optional
    # fleet-needs-attention modifier, so anchor on the word-boundary after
    # "fleet-node" rather than requiring it to be the sole class. The opening
    # tag spans multiple lines, so match lazily up to the first '>'.
    nodes = re.findall(r'<a class="fleet-node\b[^"]*"[^>]*?>', body)
    fleet_node_count = len(nodes)
    verdicts = Counter()
    for node in nodes:
        m = re.search(r'data-verdict="([^"]+)"', node)
        if m:
            verdicts[m.group(1)] += 1
    # needs-attention is signalled by data-needs-attention="1" on a node. Count it
    # ONLY on the desktop SVG fleet-node anchors captured above — NOT a blanket
    # body-wide findall. The page carries the SAME gardeners TWICE: once as SVG
    # <a class="fleet-node ..."> disks and again as <a class="fleet-board-chip ...">
    # rows in the narrow (touch) health-board, and each emphasized gardener stamps
    # data-needs-attention="1" on BOTH. A body-wide count therefore double-counts
    # (2 emphasized gardeners -> 4), drifting from the board's own "N need attention"
    # headline and from the 2 nodes actually emphasized on the desktop map
    # (gd-qHZXAip98w). Anchoring on the already-captured SVG nodes keeps the metric
    # equal to what the desktop map renders.
    needs_attention = sum(1 for node in nodes if 'data-needs-attention="1"' in node)
    verdict_classes = {k: verdicts.get(k, 0) for k in VERDICT_KEYS if verdicts.get(k, 0)}
    # Always surface the canonical keys even when zero, for stable judging.
    for k in VERDICT_KEYS:
        verdict_classes.setdefault(k, 0)
    return {
        "fleet_node_count": fleet_node_count,
        "verdict_classes": dict(sorted(verdict_classes.items())),
        "needs_attention_emphasized": needs_attention,
        "kd_list_gardener_count": kd_truth,
        "node_vs_truth_delta": kd_truth - fleet_node_count,
    }


# ---------------------------------------------------------------------------
# Linkage-fidelity — the /maps/{id} per-gardener radial.
# ---------------------------------------------------------------------------
def _radial_counts(html: str) -> tuple:
    """(node_count, edge_count, origin_tally, orphan_nodes)."""
    body = rendered_body(html)
    # Nodes: each bead/invocation is a <g class="constellation-node ...">.
    node_groups = re.findall(r'<g class="constellation-node\b[^"]*"', body)
    node_count = len(node_groups)
    # Center label counts as a node only if it carries a bead — keep it separate.
    # Edges live in the <g class="constellation-edges"> group, but each edge is
    # itself wrapped/annotated such that a lazy `.*?</g>` stops at the first
    # nested </g> and undercounts. Anchor instead on every edge element that
    # carries a data-origin (the provenance marker only edges have), which is a
    # robust, group-independent count.
    eg = re.search(r'<g class="constellation-edges">(.*)</svg>', body, re.DOTALL)
    edge_txt = eg.group(1) if eg else body
    edge_elems = re.findall(r'<(?:line|path)\b[^>]*\bdata-origin="[^"]*"[^>]*>',
                            edge_txt)
    edge_count = len(edge_elems)
    origins = Counter()
    endpoints = set()
    for e in edge_elems:
        mo = re.search(r'data-origin="([^"]+)"', e)
        origins[mo.group(1) if mo else "unknown"] += 1
        for attr in ("data-source-bead", "data-target-bead"):
            ma = re.search(attr + r'="([^"]+)"', e)
            if ma:
                endpoints.add(ma.group(1))
    # Orphans: bead nodes whose href id never appears as an edge endpoint.
    node_ids = set()
    for m in re.finditer(r'<g class="constellation-node\b[^"]*".*?</g>', body, re.DOTALL):
        seg = m.group(0)
        # Node markers are <a href="/beads/kd-..."> inside each node <g>. The
        # earlier `/(kd-)` pattern (missing the `beads/` segment) matched
        # nothing, so node_ids stayed empty and orphan_nodes was pinned to 0
        # regardless of the map's actual connectivity. Match the real href so
        # the orphan metric reflects rendered-but-unconnected nodes.
        href = re.search(r'href="/beads/((gd|ka)-[A-Za-z0-9]+)"', seg)
        if href:
            node_ids.add(href.group(1))
    orphan_nodes = len(node_ids - endpoints) if node_ids else 0
    return node_count, edge_count, dict(sorted(origins.items())), orphan_nodes


def linkage_fidelity(gardener_html: str, d3_html: str, filtered_html: str) -> dict:
    node_count, edge_count, origins, orphans = _radial_counts(gardener_html)
    # Truncation banner: a *rendered* node/degree cap notice. The viewer emits
    # <p class="map-banner">Map drawing capped at N nodes …</p> when the BFS hits
    # the node cap. Match either the banner element or its rendered "capped"
    # copy so a class rename on one side doesn't silently drop the signal.
    body = rendered_body(gardener_html)
    banner = 1 if re.search(
        r'class="[^"]*map-banner[^"]*"|drawing capped at\s+\d+\s+nodes',
        body, re.IGNORECASE) else 0
    # Depth control: ?degree=3 should deepen the walk (more nodes than the base).
    d3_nodes = _radial_counts(d3_html)[0] if d3_html else None
    degree3_grows = 1 if (d3_nodes is not None and d3_nodes > node_count) else 0
    # Filter control: ?filter=status:in_progress should shrink the node set.
    f_nodes = _radial_counts(filtered_html)[0] if filtered_html else None
    filter_shrinks = 1 if (f_nodes is not None and f_nodes < node_count) else 0
    return {
        "map_node_count": node_count,
        "map_edge_count": edge_count,
        "edge_origin_classes": origins,
        "orphan_nodes": orphans,
        "truncation_banner": banner,
        "degree3_grows": degree3_grows,
        "filter_shrinks": filter_shrinks,
        # Diagnostic context so a 0/0 control failure is explainable, not silent.
        "_degree3_node_count": d3_nodes,
        "_filtered_node_count": f_nodes,
    }


# ---------------------------------------------------------------------------
# Correctness — both surfaces.
# ---------------------------------------------------------------------------
ERR_RE = re.compile(r"error|failed to|nil|panic", re.IGNORECASE)
LEAK_RE = re.compile(r"\{\{|\}\}|<no value>")


def correctness(htmls: dict) -> dict:
    template_leaks = 0
    error_markers = 0
    empty_svg = 0
    for name, html in htmls.items():
        if not html:
            continue
        body = rendered_body(html)
        txt = visible_text(body)
        template_leaks += len(LEAK_RE.findall(body))
        error_markers += len(ERR_RE.findall(txt))
        for svg in svg_blocks(html):
            # A node-bearing svg has <circle>/<g class="...node...">; blank ⇒ empty.
            has_nodes = bool(re.search(r'<circle|class="[^"]*node', svg))
            if not has_nodes:
                empty_svg = 1
    return {
        "template_leaks": template_leaks,
        "error_markers": error_markers,
        "empty_svg": empty_svg,
    }


# ---------------------------------------------------------------------------
def kd_gardener_count() -> int:
    """Ground truth: the LIVE gardener fleet the /maps board draws as .fleet-disk
    nodes — open, ENABLED, type=trigger beads that are role:gardener by LABEL OR
    by the role=gardener FIELD.

    This MUST match the board's live predicate (fleetGardenerIsLive in
    maps_fleet.go: Status=="active" && Kind=="trigger", where "active" means
    open AND enabled). If the two predicates drift, node_vs_truth_delta reports
    phantom gardeners that the board is in fact NOT drawing. Three earlier
    miscalibrations produced exactly that false alarm:

      * No --limit, so `kd list` truncated to the default page (~20 rows) and
        undercounted gardeners whenever the open-trigger set was larger.
      * Counted open-trigger gardeners REGARDLESS of `enabled`, while the board
        only draws enabled ones (disabled gardeners drop to the history lane).
      * Matched the role:gardener LABEL only, while the board's loadGardenerRows
        (gardeners.go, ka-0000001950) matches role:gardener by LABEL *OR* by the
        role=gardener FIELD — so a gardener created the "wrong way" (role set as a
        field, never mirrored to a label) is drawn on the board but missed by a
        label-only ground-truth query. That field-only fleet read as -4 phantom
        gardeners in the maps-beauty audit (ka-0000000919). The create chokepoint
        now mirrors role→label (gb trigger.go:triggerBeadLabels), but the
        measurer must accept BOTH predicates to stay aligned with the board for
        any field-only gardeners created before that fix.

    All three are fixed here: a high limit defeats truncation, the `enabled`
    filter aligns with the board, and the role test accepts label OR field.
    """
    try:
        out = subprocess.run(
            # --limit 1000 defeats the default-page truncation; the live gardener
            # fleet is operator-bounded and far below this.
            ["kd", "list", "--type=trigger", "--status=open", "--limit", "1000", "--json"],
            capture_output=True, text=True, timeout=60, check=True,
        ).stdout
        rows = json.loads(out)
        return sum(
            1 for r in rows
            if _is_gardener_role(r)
            and _gardener_enabled((r.get("fields") or {}).get("enabled"))
        )
    except Exception as e:  # noqa: BLE001
        print(f"WARN: kd gardener ground-truth failed: {e}", file=sys.stderr)
        return 0


def _is_gardener_role(row) -> bool:
    """Match the board's gardener predicate: role:gardener by LABEL *or* by the
    role=gardener FIELD. loadGardenerRows (gardeners.go) unions a label query and a
    role=gardener field query, deduping by id, so a missing label can never hide
    a live gardener from /gardeners — and the audit ground truth must use the same
    union or it reports field-only gardeners as phantom (ka-0000000919)."""
    if "role:gardener" in (row.get("labels") or []):
        return True
    return (row.get("fields") or {}).get("role") == "gardener"


def _gardener_enabled(v) -> bool:
    """Match the board's enabled predicate EXACTLY. The board derives Enabled as
    `b.Fields["enabled"] != "false"` (gardener.go) — a trigger gardener is
    default-ON, so only an explicit "false" disables it; an absent/empty field
    is enabled. Mirror that here (the field may decode as a JSON bool or a
    stringly-typed value), so the ground-truth count never diverges from the
    board on a gardener that simply omits the `enabled` field."""
    if v is None:
        return True  # absent ⇒ default-on, same as the board
    if isinstance(v, bool):
        return v
    return str(v).strip().lower() != "false"


def read_html(path: Path, warnings: list):
    if not path.exists():
        warnings.append(f"missing html {path.name}")
        return None
    try:
        return path.read_text()
    except Exception as e:  # noqa: BLE001
        warnings.append(f"unreadable html {path.name}: {e}")
        return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True, help="captures dir (html/, screenshots/)")
    ap.add_argument("--out", required=True, help="output metrics.json path")
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--maps-gardener", default="", help="gardener bead id rendered at /maps/{id}")
    ap.add_argument("--gd-gardener-count", type=int, default=None,
                    help="override ground-truth gardener count (skips kd call)")
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    desktop_dir = cap / "screenshots" / "desktop"
    narrow_dir = cap / "screenshots" / "narrow"

    warnings: list = []

    # --- Beauty/Density per page ---
    beauty = {}
    for slug in ("maps-fleet", "maps-gardener"):
        beauty[slug] = beauty_block(slug, html_dir, desktop_dir, narrow_dir, warnings)

    # --- Load HTML surfaces ---
    fleet_html = read_html(html_dir / "maps-fleet.html", warnings)
    gardener_html = read_html(html_dir / "maps-gardener.html", warnings)
    d3_html = read_html(html_dir / "maps-gardener-d3.html", warnings)
    filtered_html = read_html(html_dir / "maps-gardener-filtered.html", warnings)

    kd_truth = args.kd_gardener_count if args.kd_gardener_count is not None else kd_gardener_count()

    gardeners = (gardener_fidelity(fleet_html, kd_truth) if fleet_html
                else {"fleet_node_count": 0, "verdict_classes": {},
                      "needs_attention_emphasized": 0, "kd_list_gardener_count": kd_truth,
                      "node_vs_truth_delta": kd_truth})
    linkage = (linkage_fidelity(gardener_html, d3_html, filtered_html) if gardener_html
               else {"map_node_count": 0, "map_edge_count": 0, "edge_origin_classes": {},
                     "orphan_nodes": 0, "truncation_banner": 0,
                     "degree3_grows": 0, "filter_shrinks": 0})
    correct = correctness({
        "maps-fleet": fleet_html,
        "maps-gardener": gardener_html,
    })

    # --- Fail flags ---
    fail_flags = []
    if correct["template_leaks"] > 0:
        fail_flags.append("template_leaks>0")
    if correct["error_markers"] > 0:
        fail_flags.append("error_markers>0")
    if correct["empty_svg"] == 1:
        fail_flags.append("empty_svg==1")
    # Node↔truth drift. delta = live_gardeners - drawn_nodes, where BOTH sides now
    # use the same live predicate (open + enabled + type=trigger + role:gardener;
    # see kd_gardener_count and fleetGardenerIsLive). The board draws exactly the
    # live fleet and demotes disabled/closed gardeners to the history lane, so a
    # faithful board scores delta == 0:
    #   * Positive delta ⇒ live gardeners MISSING from the board (a real bug).
    #   * Negative delta ⇒ the board draws PHANTOMS beyond the live fleet (a real
    #     bug — e.g. a disabled/closed gardener leaking onto the disk grid).
    # Both directions are faults once they exceed a small capture/gd-list race
    # tolerance; only a same-instant snapshot is exactly 0, so allow ±tolerance.
    delta = gardeners["node_vs_truth_delta"]
    tolerance = 1  # one gardener can flip open↔closed between the capture and the kd read
    if delta > tolerance:
        fail_flags.append(f"node_vs_truth_delta=+{delta} (live gardeners missing from map)")
    elif delta < -tolerance:
        fail_flags.append(f"node_vs_truth_delta={delta} (phantom gardeners drawn on map)")

    out = {
        "target_url": args.target_url,
        "maps_gardener": args.maps_gardener,
        "iteration": args.iteration,
        "beauty": beauty,
        "gardeners": gardeners,
        "linkage": linkage,
        "correctness": correct,
        "fail_flags": fail_flags,
        "warnings": warnings,
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")
    if warnings:
        print(f"  {len(warnings)} warning(s):", file=sys.stderr)
        for w in warnings:
            print(f"    - {w}", file=sys.stderr)
    if fail_flags:
        print(f"  FAIL FLAGS: {', '.join(fail_flags)}", file=sys.stderr)


if __name__ == "__main__":
    main()
