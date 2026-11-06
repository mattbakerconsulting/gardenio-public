#!/usr/bin/env python3
"""Compute objective Formula-UX (CREWS) metrics for the formula-ux-audit.

Ingests a formula-ux captures dir (html/ + screenshots/desktop|narrow +
overflow.json) and emits a metrics.json roll-up across the five CREWS
dimensions — Catalog clarity, Recipe legibility, Execution honesty,
Workbench ergonomics, Skin — plus shared Correctness signals.

Sibling of measure-maps.py (CLUB) / measure-gardeners.py (CAMP); rubric:
gardenio/docs/formula-ux-rubric.md §2. The judge skips null blocks, so an
unreadable capture is recorded as null + a warning, never a hard fail.

Ground-truth notes (calibrated against the deployed handlers, not the
rubric's shorthand — see the miscalibration history in measure-maps.py):

  * The /formulas handler (formulas.go handleFormulas) lists type=formula
    beads with NO status filter, limit 500. The row-count truth therefore
    uses the same predicate (all statuses, high limit); the open-only count
    is recorded alongside as a diagnostic.
  * The live-molecule rollup queries statuses open+in_progress and joins on
    the molecule's formula_id field — truth mirrors that exactly.
  * Molecule step children may carry no source_step_id (trigger-engine
    pours); the view falls back to title matching with applied vars
    substituted (molecule_view.go), so the step-walk truth does the same.
  * Execution-honesty signals compare a FRESH fetch of the molecule page
    against a same-instant kd walk — comparing the (older) capture snapshot
    to current kd state would report drift as lies.
  * In-page hrefs carry the public-edge base path (/beads/...); at the
    in-cluster ClusterIP the app is mounted at the root, so the dead-link
    sweep strips that prefix (recorded in link_prefix_stripped).
"""
import argparse
import html as html_mod
import json
import re
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlsplit

try:
    from PIL import Image
except ImportError:
    Image = None

# Full-page screenshots can exceed Pillow's default decompression-bomb guard.
if Image is not None:
    Image.MAX_IMAGE_PIXELS = None

try:
    import numpy as np
except ImportError:
    np = None

STYLE_RE = re.compile(r"<style[^>]*>.*?</style>", re.IGNORECASE | re.DOTALL)
SCRIPT_RE = re.compile(r"<script[^>]*>.*?</script>", re.IGNORECASE | re.DOTALL)

PAGES = ("formulas-catalog", "formula-dag", "molecule-exec")

# Base path the viewer templates bake into hrefs when deployed behind the
# public edge (BEADS_VIEWER_URL=https://.../beads). Stripped for ClusterIP
# sweeps where the app is mounted at the root.
EDGE_BASE_PATH = "/beads"


def rendered_body(html: str) -> str:
    return SCRIPT_RE.sub("", STYLE_RE.sub("", html))


def read_html(path: Path, warnings: list):
    if not path.exists():
        warnings.append(f"missing html {path.name}")
        return None
    try:
        return path.read_text()
    except Exception as e:  # noqa: BLE001
        warnings.append(f"unreadable html {path.name}: {e}")
        return None


def kd_json(args_list, warnings, default):
    try:
        out = subprocess.run(["kd", *args_list], capture_output=True, text=True,
                             timeout=90, check=True).stdout
        return json.loads(out)
    except Exception as e:  # noqa: BLE001
        warnings.append(f"kd {' '.join(args_list[:3])}… failed: {e}")
        return default


def curl_status(url: str, timeout: int = 10):
    """HTTP status via curl (the audit pod has no requests lib guarantee)."""
    try:
        out = subprocess.run(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
             "--max-time", str(timeout), url],
            capture_output=True, text=True, timeout=timeout + 5, check=False,
        ).stdout.strip()
        return int(out) if out.isdigit() and out != "000" else None
    except Exception:  # noqa: BLE001
        return None


def curl_status_retry(url: str, attempts: int = 3, timeout: int = 20):
    """HTTP status with retries + a generous timeout for the link sweep.

    The aggregating index pages (/threads, /agents, /plans) fan out one
    daemon request per row and can take >10s over the agent-pod local
    dev-bypass boot, so a single short-timeout probe records them as dead
    (code None) even though they return 200 on a longer wait. Retry with a
    longer timeout so a slow-but-live control is never a phantom dead link
    (ka-0000001495). Returns (code, attempts_used); a code < 400 short-
    circuits.
    """
    code = None
    for i in range(1, attempts + 1):
        code = curl_status(url, timeout=timeout)
        if code is not None and code < 400:
            return code, i
    return code, attempts


def curl_body(url: str, timeout: int = 20):
    try:
        r = subprocess.run(["curl", "-fsS", "--max-time", str(timeout), url],
                           capture_output=True, text=True, timeout=timeout + 5,
                           check=False)
        return r.stdout if r.returncode == 0 else None
    except Exception:  # noqa: BLE001
        return None


def curl_headers(url: str, timeout: int = 3):
    """Status + content-type for a streaming endpoint (SSE never closes, so
    read only the response headers under a hard timeout)."""
    try:
        r = subprocess.run(
            ["curl", "-s", "-o", "/dev/null", "-D", "-", "--max-time",
             str(timeout), url],
            capture_output=True, text=True, timeout=timeout + 5, check=False)
        head = r.stdout
        m = re.search(r"^HTTP/[\d.]+\s+(\d+)", head)
        ct = re.search(r"^content-type:\s*([^\r\n]+)", head,
                       re.IGNORECASE | re.MULTILINE)
        return (int(m.group(1)) if m else None,
                ct.group(1).strip() if ct else None)
    except Exception:  # noqa: BLE001
        return None, None


# ---------------------------------------------------------------------------
# Recipe ground truth
# ---------------------------------------------------------------------------
def parse_steps_field(fields: dict):
    raw = (fields or {}).get("steps")
    if raw is None:
        return []
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except Exception:  # noqa: BLE001
            return []
    return raw if isinstance(raw, list) else []


def parse_vars_field(fields: dict):
    raw = (fields or {}).get("vars")
    if raw is None:
        return []
    if isinstance(raw, str):
        try:
            raw = json.loads(raw)
        except Exception:  # noqa: BLE001
            return []
    return raw if isinstance(raw, list) else []


def step_shape_glyph(step: dict) -> str:
    """Mirror formula_dag.go's DSL-shape → glyph mapping."""
    if step.get("gate"):
        return "◆"
    if step.get("branch"):
        return "⑃"
    if step.get("loop"):
        return "↻"
    if step.get("children") or step.get("expand"):
        return "▤"
    return "⚙"


def substitute_vars(text: str, applied: dict) -> str:
    for k, v in (applied or {}).items():
        text = text.replace("{{" + k + "}}", str(v))
    return text


# ---------------------------------------------------------------------------
# C — Catalog clarity
# ---------------------------------------------------------------------------
CATALOG_ROW_SPLIT = re.compile(r'<li class="bead-row formula-row"')
ROW_ID_RE = re.compile(r'formula-row-title" href="[^"]*?/((gd|ka)-[A-Za-z0-9]+)"')
LIVE_COUNT_RE = re.compile(r'formula-live-count">\s*(\d+)\s+live')
FPIP_RE = re.compile(r'class="fpip fpip-[a-z_]+"')
FPIP_OVERFLOW_RE = re.compile(r'fpip-overflow[^>]*>\s*\+(\d+)')


def catalog_clarity(cat_html, kd_formula_ids, kd_open_formula_count,
                    kd_live_molecules_by_formula, warnings):
    if cat_html is None:
        return None
    body = rendered_body(cat_html)
    chunks = CATALOG_ROW_SPLIT.split(body)[1:]
    row_ids = []
    live_sum = 0
    pip_dishonest_rows = []
    for chunk in chunks:
        m = ROW_ID_RE.search(chunk)
        rid = m.group(1) if m else None
        if rid:
            row_ids.append(rid)
        lm = LIVE_COUNT_RE.search(chunk)
        live = int(lm.group(1)) if lm else 0
        live_sum += live
        if live > 0:
            pips = len(FPIP_RE.findall(chunk))
            om = FPIP_OVERFLOW_RE.search(chunk)
            overflow = int(om.group(1)) if om else 0
            if pips + overflow != live:
                pip_dishonest_rows.append(
                    {"id": rid, "live": live, "pips": pips, "overflow": overflow})
    row_set = set(row_ids)
    truth_set = set(kd_formula_ids)
    kd_live_total = sum(kd_live_molecules_by_formula.get(fid, 0)
                       for fid in row_set)
    return {
        "formula_row_count": len(row_ids),
        # Handler predicate: ALL statuses, limit 500 (formulas.go). Open-only
        # recorded as a diagnostic — the catalog intentionally lists retired
        # recipes too, which the judge weighs separately.
        "kd_formula_count": len(truth_set),
        "kd_open_formula_count": kd_open_formula_count,
        "row_vs_truth_delta": len(truth_set) - len(row_ids),
        "missing_rows": sorted(truth_set - row_set)[:10],
        "phantom_rows": sorted(row_set - truth_set)[:10],
        "live_count_sum": live_sum,
        "kd_open_molecule_count": kd_live_total,
        "live_vs_truth_delta": kd_live_total - live_sum,
        "pip_overflow_honest": 0 if pip_dishonest_rows else 1,
        "pip_dishonest_rows": pip_dishonest_rows[:10],
    }


# ---------------------------------------------------------------------------
# R — Recipe legibility
# ---------------------------------------------------------------------------
NODE_G_RE = re.compile(
    r'<(?:g|a) class="fdag-node\b([^"]*)"[^>]*>(.*?)</(?:g|a)>', re.DOTALL)
GLYPH_RE = re.compile(r'class="fdag-glyph"[^>]*>([^<]*)<')
NODE_ID_RE = re.compile(r'class="fdag-id"[^>]*>([^<]*)<')
EDGE_RE = re.compile(r'<(?:path|line|polyline)\b[^>]*class="[^"]*fdag-edge[^"]*"')
VAR_DT_RE = re.compile(r"<dt><code>([^<]+)</code></dt>")


def recipe_legibility(dag_html, recipe_steps, recipe_vars, warnings):
    if dag_html is None:
        return None
    body = rendered_body(dag_html)
    nodes = NODE_G_RE.findall(body)
    internal, external = [], []
    for cls, inner in nodes:
        (external if "fdag-external" in cls else internal).append((cls, inner))
    step_by_id = {s.get("id") or s.get("step_id"): s for s in recipe_steps}
    glyph_mismatches = []
    for _, inner in internal:
        idm = NODE_ID_RE.search(inner)
        gm = GLYPH_RE.search(inner)
        sid = idm.group(1).strip() if idm else None
        glyph = gm.group(1).strip() if gm else None
        step = step_by_id.get(sid)
        if step is None:
            glyph_mismatches.append({"node": sid, "why": "node id not in recipe"})
            continue
        want = step_shape_glyph(step)
        if glyph != want:
            glyph_mismatches.append({"node": sid, "glyph": glyph, "want": want})
    recipe_edge_count = sum(len(s.get("depends_on") or []) for s in recipe_steps)
    edge_count = len(EDGE_RE.findall(body))
    # Fallback semantics: the flat list INSTEAD of a DAG (not the collapsible
    # step list that renders alongside a healthy DAG).
    has_dag = bool(internal)
    fallback_shown = 1 if ("formula-step-list" in body and not has_dag) else 0
    # Vars side-rail
    rail_idx = body.find("formula-side-rail")
    rail_vars = VAR_DT_RE.findall(body[rail_idx:rail_idx + 6000]) if rail_idx >= 0 else []
    # Pour / cook copy commands
    pour = re.search(r'data-copy="pour"\s+data-value="([^"]*)"', body)
    pour_val = html_mod.unescape(pour.group(1)) if pour else ""
    # Raw steps JSON parses
    raw_ok = 0
    rj = re.search(r'formula-raw-json.*?<pre><code>(.*?)</code></pre>', dag_html,
                   re.DOTALL)
    if rj:
        try:
            json.loads(html_mod.unescape(rj.group(1)))
            raw_ok = 1
        except Exception as e:  # noqa: BLE001
            warnings.append(f"raw steps JSON does not parse: {e}")
    return {
        "fdag_node_count": len(internal),
        "recipe_step_count": len(recipe_steps),
        "node_vs_truth_delta": len(recipe_steps) - len(internal),
        "fdag_edge_count": edge_count,
        "recipe_edge_count": recipe_edge_count,
        "edge_vs_truth_delta": recipe_edge_count - edge_count,
        "glyph_mismatches": glyph_mismatches,
        "external_count": len(external),
        "fallback_shown": fallback_shown,
        "var_rail_count": len(rail_vars),
        "recipe_var_count": len(recipe_vars),
        "var_rail_delta": len(recipe_vars) - len(rail_vars),
        "pour_cmd_valid": 1 if pour_val.startswith("kd formula pour ") else 0,
        "raw_json_valid": raw_ok,
        # Diagnostics for the judge (not scored mechanically): a pure recipe
        # page stamping fdag-current / empty step-status classes.
        "_current_marker_on_recipe": 1 if "fdag-current" in body else 0,
        "_empty_status_class_nodes": len(re.findall(r'class="fdag-node step- ', body))
        + len(re.findall(r'class="fdag-node step-"', body)),
    }


# ---------------------------------------------------------------------------
# E — Execution honesty (fresh fetch vs same-instant kd walk)
# ---------------------------------------------------------------------------
PROGRESS_RE = re.compile(r'molecule-progress-label">\s*(\d+)/(\d+)\s+done')
TR_RE = re.compile(r"<tr\b[^>]*>.*?</tr>", re.DOTALL)
CHIP_RE = re.compile(r"molecule-step-status\s+status-([\w-]+)")
STEP_BEAD_RE = re.compile(r'href="[^"]*?/((gd|ka)-[A-Za-z0-9]+)"\s+title="[^"]*"><code>')

# Bead status → acceptable chip statuses (molecule_view.go aggregate +
# dependency-aware ready/pending).
CHIP_OK = {
    "closed": {"done", "partial"},
    "in_progress": {"in_progress"},
    "open": {"pending", "ready", "blocked"},
    "deferred": {"deferred", "pending", "blocked"},
}

# Steps whose children are all in one of these terminal states unblock their
# downstream steps in the dependency-aware frontier walk — the view's
# done||partial gate (molecule_view.go:234, aggregateStepStatus).
FRONTIER_TERMINAL = ("done", "partial")


def map_step_children(children, recipe_steps, applied_vars):
    """Group molecule child beads by the recipe step they realize, mirroring the
    view's join (molecule_view.go): prefer the child's fields.source_step_id,
    fall back to substituted-title match only when the id is absent.

    Extracted for testability. The source_step_id path MUST win whenever it is
    present — the title fallback breaks when a poured child's title has drifted
    from its recipe template (observed on E2E-Spec-Fixer's push-and-verify),
    which would drop that child and undercount the frontier, false-flagging the
    headline Execution dimension against an honest view. Children are only
    fetched via `kd list --parent` (full rows carry .fields), never
    `kd dep children --json` (thin projection — no .fields, forces the fallback
    for every child).
    """
    titles = {}
    for s in recipe_steps:
        t = substitute_vars(s.get("title", ""), applied_vars)
        titles[t] = s.get("id") or s.get("step_id")
    step_children: dict = {}
    for ch in children:
        fields = ch.get("fields") or {}
        sid = fields.get("source_step_id") or titles.get(ch.get("title", ""))
        if sid:
            step_children.setdefault(sid, []).append(ch)
    return step_children


def molecule_frontier_ids(recipe_steps, step_children, applied_vars,
                          done_states=("closed",)):
    """Recipe step ids the molecule view highlights as the execution frontier,
    mirroring molecule_view.go:274-284 ("the in_progress rows; when nothing
    runs yet, the dependency-ready rows"). Extracted from execution_honesty so
    the ka-0000004230 frontier rule has a unit-testable seam — a molecule
    paused BETWEEN agent runs (some children closed, some open, 0 in_progress)
    has a real dependency-ready frontier the view honestly highlights, which an
    in_progress-only count would miss and false-flag as execution:frontier_delta!=0.
    """
    done_states = set(done_states)

    def step_agg(sid):
        beads = step_children.get(sid)
        if not beads:
            return "not_poured"
        statuses = [c.get("status") for c in beads]
        if any(st == "in_progress" for st in statuses):
            return "in_progress"
        if all(st in done_states for st in statuses):
            return "done"
        return "open"

    step_ids = [s.get("id") or s.get("step_id") for s in recipe_steps]
    agg_by_id = {sid: step_agg(sid) for sid in step_ids}
    frontier_ids = {sid for sid in step_ids if agg_by_id[sid] == "in_progress"}
    if not frontier_ids:
        # ready = open steps whose every resolvable recipe dependency is done
        dep_by_id = {}
        for s in recipe_steps:
            sid = s.get("id") or s.get("step_id")
            dep_by_id[sid] = [substitute_vars(d, applied_vars)
                              for d in (s.get("depends_on") or [])]
        for sid in step_ids:
            if agg_by_id.get(sid) != "open":
                continue
            deps = dep_by_id.get(sid, [])
            # A dep we can't resolve to a row stays optimistic (view does the
            # same for inherited/external deps).
            if all(agg_by_id.get(d, "done") in FRONTIER_TERMINAL for d in deps):
                frontier_ids.add(sid)
    return frontier_ids


def execution_honesty(target_url, molecule_id, recipe_steps, applied_vars,
                      warnings):
    if not target_url or not molecule_id:
        warnings.append("no target-url/molecule-id — execution block null")
        return None
    live_html = curl_body(f"{target_url}/{molecule_id}")
    if live_html is None:
        warnings.append("fresh molecule page fetch failed — execution block null")
        return None
    body = rendered_body(live_html)

    # Same-instant kd walk: children matched to recipe steps by
    # source_step_id, falling back to substituted-title match (the same join
    # the view performs).
    #
    # Use `kd list --parent` (FULL bead rows incl. .fields.source_step_id), NOT
    # `kd dep children --json` — the latter is a deliberately THIN projection
    # (id/type/status/title only, no .fields per its --help), so every child's
    # source_step_id read as null and the join silently degraded to fragile
    # title-matching. That undercounts the frontier whenever a poured child's
    # title has drifted from its recipe template (e.g. E2E-Spec-Fixer's
    # push-and-verify: template "…Push, open the MR, verify spec runs in CI…"
    # vs poured "…Push, verify spec runs in CI shards…"), which false-flags
    # execution:frontier_delta!=0 against an honest view. source_step_id is the
    # view's canonical join key (molecule_view.go) and never drifts.
    children = kd_json(["list", "--parent", molecule_id, "--limit", "500",
                        "--json"], warnings, [])
    step_children = map_step_children(children, recipe_steps, applied_vars)
    kd_step_total = len(recipe_steps)
    done_states = {"closed"}
    kd_step_done = sum(
        1 for sid in (s.get("id") or s.get("step_id") for s in recipe_steps)
        if step_children.get(sid)
        and all(c.get("status") in done_states for c in step_children[sid]))

    # Dependency-aware frontier truth, mirroring molecule_view.go:274-284
    # ("in_progress rows; when nothing runs yet, the ready rows"). A molecule
    # BETWEEN agent runs (some children closed, some open, 0 in_progress) has a
    # real dependency-ready frontier that the view honestly highlights — an
    # in_progress-only count reads 0 there and false-flags E (ka-0000004230).
    kd_frontier = len(molecule_frontier_ids(recipe_steps, step_children,
                                            applied_vars, done_states))

    pm = PROGRESS_RE.search(body)
    progress_done = int(pm.group(1)) if pm else None
    progress_total = int(pm.group(2)) if pm else None

    # Per-row chips vs bead status, from the fresh page's steps table.
    chip_mismatches = []
    frontier_rows = 0
    not_poured_rows = 0
    tbl = re.search(r'<table[^>]*molecule-steps-table.*?</table>', live_html,
                    re.DOTALL)
    rows = TR_RE.findall(tbl.group(0)) if tbl else []
    status_by_bead = {c.get("id"): c.get("status") for c in children}
    for row in rows[1:]:  # skip header
        if "molecule-row-frontier" in row:
            frontier_rows += 1
        if "molecule-not-poured" in row:
            not_poured_rows += 1
        chip = CHIP_RE.search(row)
        bead = STEP_BEAD_RE.search(row)
        if chip and bead:
            bead_status = status_by_bead.get(bead.group(1))
            if bead_status and chip.group(1) not in CHIP_OK.get(bead_status, set()):
                chip_mismatches.append(
                    {"bead": bead.group(1), "chip": chip.group(1),
                     "bead_status": bead_status})

    anchor = re.search(r'data-molecule-events="([^"]+)"', live_html)
    sse_status, sse_ct = curl_headers(f"{target_url}/molecule/{molecule_id}/events")
    cost_status = curl_status(f"{target_url}/molecule/{molecule_id}/cost")

    return {
        "progress_done": progress_done,
        "progress_total": progress_total,
        "kd_step_done": kd_step_done,
        "kd_step_total": kd_step_total,
        "progress_vs_truth_delta": (
            None if progress_done is None
            else [kd_step_done - progress_done, kd_step_total - progress_total]),
        "status_chip_mismatches": chip_mismatches,
        "frontier_rows": frontier_rows,
        "frontier_truth": kd_frontier,
        "frontier_delta": kd_frontier - frontier_rows,
        "not_poured_rows": not_poured_rows,
        "sse_anchor_present": 1 if anchor else 0,
        "sse_anchor_href": anchor.group(1) if anchor else None,
        "sse_endpoint_ok": 1 if (sse_status == 200 and sse_ct
                                 and "text/event-stream" in sse_ct) else 0,
        "cost_link_ok": 1 if (cost_status is not None and cost_status < 400) else 0,
        "step_match_mode": "title-fallback" if not any(
            (c.get("fields") or {}).get("source_step_id") for c in children)
        else "source_step_id",
    }


# ---------------------------------------------------------------------------
# W — Workbench ergonomics
# ---------------------------------------------------------------------------
HREF_RE = re.compile(r'href="([^"]+)"')


def workbench(htmls, dag_html, target_url, warnings, link_cap=200):
    hrefs = set()
    for h in htmls.values():
        if h:
            hrefs.update(HREF_RE.findall(rendered_body(h)))
    in_viewer = []
    for h in sorted(hrefs):
        h = html_mod.unescape(h)
        if h.startswith("#") or h.startswith("mailto:"):
            continue
        if h.startswith("http"):
            if target_url and h.startswith(target_url):
                h = h[len(target_url):] or "/"
            else:
                continue  # external — out of sweep scope
        # Strip the public-edge base path for ClusterIP sweeps.
        if h == EDGE_BASE_PATH:
            h = "/"
        elif h.startswith(EDGE_BASE_PATH + "/"):
            h = h[len(EDGE_BASE_PATH):]
        in_viewer.append(h)
    in_viewer = sorted(set(in_viewer))
    swept = in_viewer[:link_cap]
    dead = []
    auth_dependent = []
    max_retries_used = 0
    for h in swept:
        if target_url:
            code, used = curl_status_retry(f"{target_url}{h}")
            max_retries_used = max(max_retries_used, used)
        else:
            code = None
        if code is None or code >= 400:
            # The sweep runs unauthenticated at the ClusterIP: routes that
            # exist only at the OIDC edge (/oauth2/*) or that 401 without a
            # session identity are vantage artifacts, not dead controls —
            # recorded separately so the judge can eyeball them.
            if h.startswith("/oauth2/") or code == 401:
                auth_dependent.append({"href": h, "code": code})
            else:
                dead.append({"href": h, "code": code})
    # Copy buttons on the formula page
    copy_btns = 0
    if dag_html:
        copy_btns = len(re.findall(r'class="copy-btn"[^>]*data-value="[^"]+"',
                                   rendered_body(dag_html)))
    # Filter round-trip: every row on ?formula_type=workflow carries that type.
    filter_roundtrip = None
    if target_url:
        fh = curl_body(f"{target_url}/formulas?formula_type=workflow")
        if fh:
            rows = re.findall(r'<li class="bead-row formula-row" data-formula-type="([^"]*)"',
                              rendered_body(fh))
            filter_roundtrip = 1 if rows and all(r == "workflow" for r in rows) else 0
    # Raw JSON validity is measured in the R block (same signal, one source).
    return {
        "links_swept": len(swept),
        "links_total_distinct": len(in_viewer),
        "link_cap": link_cap,
        "link_prefix_stripped": EDGE_BASE_PATH,
        "dead_links": len(dead),
        "dead_link_samples": dead[:10],
        "auth_dependent_links": auth_dependent[:10],
        "max_retries_used": max_retries_used,
        "copy_btn_count": copy_btns,
        "filter_roundtrip": filter_roundtrip,
    }


# ---------------------------------------------------------------------------
# S — Skin
# ---------------------------------------------------------------------------
def png_near_white(path: Path):
    if Image is None:
        return None
    with Image.open(path) as im:
        im = im.convert("RGB")
        w, h = im.size
        if not w * h:
            return None
        if np is not None:
            arr = np.asarray(im, dtype=np.uint8)
            return round(float(np.all(arr >= 240, axis=2).sum()) / (w * h), 4)
        px = im.load()
        near = sum(1 for y in range(h) for x in range(w)
                   if all(c >= 240 for c in px[x, y]))
        return round(near / (w * h), 4)


def skin(cap_dir: Path, warnings):
    overflow = {}
    op = cap_dir / "overflow.json"
    if op.exists():
        try:
            overflow = json.loads(op.read_text())
        except Exception as e:  # noqa: BLE001
            warnings.append(f"overflow.json unreadable: {e}")
    out = {
        "overflow_catalog_narrow": overflow.get("formulas-catalog"),
        "overflow_dag_narrow": overflow.get("formula-dag"),
        "overflow_molecule_narrow": overflow.get("molecule-exec"),
    }
    for slug in PAGES:
        p = cap_dir / "screenshots" / "desktop" / f"{slug}.png"
        if p.exists():
            try:
                out[f"near_white_{slug.replace('-', '_')}"] = png_near_white(p)
            except Exception as e:  # noqa: BLE001
                warnings.append(f"{slug}.png unreadable: {e}")
                out[f"near_white_{slug.replace('-', '_')}"] = None
        else:
            out[f"near_white_{slug.replace('-', '_')}"] = None
    return out


# ---------------------------------------------------------------------------
# Correctness
# ---------------------------------------------------------------------------
# A LEAK is an unrendered Go template action ({{.Field}}, {{if …}},
# {{range …}}, {{template …}}, <no value>) — NOT a formula var placeholder.
# Recipe steps legitimately display unbound {{VAR}} vars in their ids,
# titles, and descriptions ({{TICKET}}: …, {{version}} …); those are recipe
# content the page is SUPPOSED to show, even outside pre/code blocks.
#
# So the leak signature is a real Go-template ACTION, identified by its leading
# token, NOT by "any {{". A Go action after {{ starts with `.` (field), `$`
# (variable), or a known keyword (if/range/with/template/block/define/end/else).
# A formula var placeholder is a bare identifier ({{version}}, {{TICKET}}), so
# it must NOT match — the previous `[.a-z]` signature wrongly caught the
# lowercase-initial `{{version}}` (ka-0000003537 false-positive), while
# uppercase `{{TICKET}}` slipped through, making the leak count depend on the
# audited recipe's var casing rather than on real leaks.
LEAK_RE = re.compile(
    r"\{\{-?\s*(?:[.$]|/?(?:if|range|with|template|block|define|end|else)\b)"
    r"|<no value>")
ERR_RE = re.compile(r"failed to|panic|Execution data unavailable", re.IGNORECASE)
PRE_CODE_RE = re.compile(r"<(pre|code)\b[^>]*>.*?</\1>", re.DOTALL)
SVG_RE = re.compile(r"<svg\b.*?</svg>", re.DOTALL)
TITLE_ATTR_RE = re.compile(r'title="[^"]*"')


def correctness(htmls):
    """Leak/error scan over page CHROME only. The formula surfaces
    legitimately render recipe payloads (step titles carrying {{VAR}}
    placeholders, raw steps JSON, descriptions with the word 'failed') inside
    pre/code blocks, the DAG SVG, and title tooltips — those are content, not
    leaks, so they are stripped before scanning."""
    leaks = 0
    errors = 0
    for h in htmls.values():
        if not h:
            continue
        body = rendered_body(h)
        chrome = PRE_CODE_RE.sub("", body)
        chrome = SVG_RE.sub("", chrome)
        chrome = TITLE_ATTR_RE.sub('title=""', chrome)
        txt = re.sub(r"<[^>]+>", " ", chrome)
        leaks += len(LEAK_RE.findall(txt))
        errors += len(ERR_RE.findall(txt))
    return {"template_leaks": leaks, "error_markers": errors}


# ---------------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", required=True,
                    help="live viewer base URL (fresh E fetch + link sweep)")
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--formula-id", required=True,
                    help="recipe rendered on the formula-dag capture")
    ap.add_argument("--molecule-id", required=True,
                    help="molecule rendered on the molecule-exec capture")
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    warnings: list = []

    cat_html = read_html(html_dir / "formulas-catalog.html", warnings)
    dag_html = read_html(html_dir / "formula-dag.html", warnings)
    mol_html = read_html(html_dir / "molecule-exec.html", warnings)

    # --- kd ground truth ---
    all_formulas = kd_json(
        ["list", "--type=formula", "--limit", "500", "--json"], warnings, [])
    open_formulas = kd_json(
        ["list", "--type=formula", "--status=open", "--limit", "500", "--json"],
        warnings, [])
    formula_ids = [f.get("id") for f in all_formulas if f.get("id")]
    live_molecules = {}
    for status in ("open", "in_progress"):
        for m in kd_json(["list", "--type=molecule", f"--status={status}",
                          "--limit", "500", "--json"], warnings, []):
            fid = (m.get("fields") or {}).get("formula_id", "").strip()
            if fid:
                live_molecules[fid] = live_molecules.get(fid, 0) + 1

    recipe = kd_json(["show", args.formula_id, "--json"], warnings, {})
    recipe_steps = parse_steps_field(recipe.get("fields"))
    recipe_vars = parse_vars_field(recipe.get("fields"))

    molecule = kd_json(["show", args.molecule_id, "--json"], warnings, {})
    mol_fields = molecule.get("fields") or {}
    applied_vars = mol_fields.get("applied_vars") or {}
    if isinstance(applied_vars, str):
        try:
            applied_vars = json.loads(applied_vars)
        except Exception:  # noqa: BLE001
            applied_vars = {}
    audit_recipe = kd_json(["show", mol_fields.get("formula_id", args.formula_id),
                            "--json"], warnings, {})
    audit_recipe_steps = parse_steps_field(audit_recipe.get("fields"))

    metrics = {
        "iteration": args.iteration,
        "target_url": args.target_url,
        "formula_id": args.formula_id,
        "molecule_id": args.molecule_id,
        "catalog": catalog_clarity(cat_html, formula_ids, len(open_formulas),
                                   live_molecules, warnings),
        "recipe": recipe_legibility(dag_html, recipe_steps, recipe_vars,
                                    warnings),
        "execution": execution_honesty(args.target_url, args.molecule_id,
                                       audit_recipe_steps, applied_vars,
                                       warnings),
        "workbench": workbench(
            {"formulas-catalog": cat_html, "formula-dag": dag_html,
             "molecule-exec": mol_html},
            dag_html, args.target_url, warnings),
        "skin": skin(cap, warnings),
        "correctness": correctness(
            {"formulas-catalog": cat_html, "formula-dag": dag_html,
             "molecule-exec": mol_html}),
    }

    # --- Fail flags (rubric §2 auto-caps' objective side) ---
    flags = []
    c = metrics["catalog"] or {}
    if c.get("row_vs_truth_delta"):
        flags.append("catalog:row_vs_truth_delta!=0")
    if c.get("live_vs_truth_delta"):
        flags.append("catalog:live_vs_truth_delta!=0")
    if c.get("pip_overflow_honest") == 0:
        flags.append("catalog:pip_overflow_dishonest")
    r = metrics["recipe"] or {}
    if r.get("node_vs_truth_delta"):
        flags.append("recipe:node_vs_truth_delta!=0")
    if r.get("edge_vs_truth_delta"):
        flags.append("recipe:edge_vs_truth_delta!=0")
    if r.get("glyph_mismatches"):
        flags.append("recipe:glyph_mismatches")
    e = metrics["execution"] or {}
    if e and e.get("progress_vs_truth_delta") not in (None, [0, 0]):
        flags.append("execution:progress_vs_truth_delta!=0")
    if e and e.get("status_chip_mismatches"):
        flags.append("execution:status_chip_mismatches")
    if e and e.get("frontier_delta"):
        flags.append("execution:frontier_delta!=0")
    if e and not e.get("sse_endpoint_ok"):
        flags.append("execution:sse_endpoint_not_ok")
    w = metrics["workbench"] or {}
    if w.get("dead_links"):
        flags.append("workbench:dead_links>0")
    s = metrics["skin"] or {}
    for key in ("overflow_catalog_narrow", "overflow_dag_narrow",
                "overflow_molecule_narrow"):
        v = s.get(key)
        if v is not None and v > 1:
            flags.append(f"skin:{key}>{1}")
    x = metrics["correctness"]
    if x["template_leaks"]:
        flags.append("correctness:template_leaks>0")
    if x["error_markers"]:
        flags.append("correctness:error_markers>0")

    metrics["fail_flags"] = flags
    metrics["warnings"] = warnings

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(metrics, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {out}", file=sys.stderr)
    print(json.dumps({"fail_flags": flags, "warnings": warnings}, indent=2))


if __name__ == "__main__":
    main()
