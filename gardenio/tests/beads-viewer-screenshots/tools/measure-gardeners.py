#!/usr/bin/env python3
"""Compute objective Gardeners-page metrics for the CAMP audit harness.

Step 3 (Measure) of the gardeners-camp formula. Reuses the density primitives
from measure-density.py for the Polish/Density vector and layers on
gardeners-specific structural signals for the four CAMP dimensions:

  - Coverage     : rows rendered vs kd ground truth, filter-chip arithmetic
  - Accuracy     : iteration cards vs advertised runs, verdict / TLDR presence
  - Management   : filter chips, next-fire, control bar, lazy-runs, sort
  - Correctness  : template leaks, error markers, empty table

It ingests the captures dir produced by Step 2 (Capture) and emits a single
metrics.json. No judgement is applied here — every value is an objective count
or ratio. Unreadable screenshots are recorded as null (the judge skips nulls)
and a warning is printed; they never BLOCK.

Usage:
  measure-gardeners.py --captures <dir> --out metrics.json \
    --target-url http://localhost:8095 --iteration 2026-06-06T1244 \
    --detail-gardener gd-XXXX
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
_md_path = Path(__file__).resolve().parent / "measure-density.py"
_spec = _ilu.spec_from_file_location("measure_density", _md_path)
_md = _ilu.module_from_spec(_spec)
_spec.loader.exec_module(_md)
html_metrics = _md.html_metrics
png_metrics = _md.png_metrics

TAG_INNER = re.compile(r"<[^>]+>")
STYLE_SCRIPT = re.compile(r"<(style|script)\b[^>]*>.*?</\1>", re.S | re.I)


def _read(path: Path) -> str:
    return path.read_text(errors="replace") if path.exists() else ""


def _body(raw: str) -> str:
    """Strip <style>/<script> so signals reflect rendered markup, not CSS."""
    return STYLE_SCRIPT.sub("", raw)


def _visible_text(raw: str) -> str:
    """Rendered (tag-stripped) text only — for word-level smell tests."""
    return html.unescape(TAG_INNER.sub(" ", _body(raw)))


# --- Polish / Density ---------------------------------------------------------
def polish_for(captures: Path, slug: str, warnings: list) -> dict:
    html_path = captures / "html" / f"{slug}.html"
    entry = {"html": None, "desktop": None, "narrow": None}
    if html_path.exists():
        entry["html"] = html_metrics(html_path)
    else:
        warnings.append(f"missing html capture: {slug}.html")
    for vp in ("desktop", "narrow"):
        png = captures / "screenshots" / vp / f"{slug}.png"
        if png.exists():
            try:
                entry[vp] = png_metrics(png)
            except Exception as e:  # unreadable screenshot -> null, warn, no block
                warnings.append(f"unreadable screenshot {vp}/{slug}.png: {e}")
                entry[vp] = None
        else:
            warnings.append(f"missing screenshot: {vp}/{slug}.png")
    return entry


# --- chip parsing -------------------------------------------------------------
CHIP_RE = re.compile(r'<a[^>]*class="gardeners-chip[^"]*"[^>]*>(.*?)</a>', re.S)


def parse_chips(idx_html: str) -> dict:
    """Return {chip_label: count} parsed from the filter chip strip.

    Each chip renders its label text followed by a <span class="cnt">N</span>.
    """
    chips = {}
    for inner in CHIP_RE.findall(idx_html):
        cnt_m = re.search(r'class="cnt"[^>]*>\s*([0-9,]+)', inner)
        count = int(cnt_m.group(1).replace(",", "")) if cnt_m else None
        # label = visible text minus the count
        label = html.unescape(TAG_INNER.sub(" ", inner))
        if count is not None:
            label = label.replace(str(count), "", 1)
        label = re.sub(r"\s+", " ", label).strip()
        if label:
            chips[label] = count
    return chips


def _chip_lookup(chips: dict, *needles) -> int | None:
    for label, count in chips.items():
        low = label.lower()
        if all(n in low for n in needles):
            return count
    return None


# The "N hidden — why" affordance (bug ka-0000007605): the All chip is the LIVE
# operational fleet, which deliberately excludes E2E fixtures and schedule-mirror
# artifacts. The page surfaces that delta as a <details class="gardeners-hidden">
# whose summary reads "N hidden from this roster". Parsing N lets Coverage verify
# the All-chip-vs-true-population delta is EXPLAINED on the page rather than a
# silent hole — rendered_rows + hidden_count should reconcile to kd ground truth.
HIDDEN_RE = re.compile(r"([0-9,]+)\s+hidden from this roster")


def parse_hidden_count(idx_html: str) -> int | None:
    """The N from the 'N hidden from this roster' disclosure, or None when the
    page renders no such affordance (i.e. the roster IS the full population)."""
    m = HIDDEN_RE.search(_body(idx_html))
    if not m:
        return None
    return int(m.group(1).replace(",", ""))


# --- kd ground truth ----------------------------------------------------------
def _is_gardener(bead: dict) -> bool:
    """A gardener is canonically a `role:gardener`-LABELLED bead, OR one carrying
    `role=gardener` as a FIELD (the "created the wrong way" case). The beads-viewer
    /gardeners page (loadGardenerRows in gardeners.go) unions BOTH and dedupes by ID,
    so the ground-truth count MUST too — a field-only gardener (label missing) is
    real and rendered, but a label-only query silently drops it (the -20 undercount
    that false-trips the Coverage triage gate)."""
    if "role:gardener" in (bead.get("labels") or []):
        return True
    return (bead.get("fields") or {}).get("role") == "gardener"


def kd_gardener_count(warnings: list) -> int | None:
    """Ground-truth gardener count matching the page's loadGardenerRows: union of
    type=trigger and type=schedule beads that are gardeners by label OR field,
    across ALL statuses, deduped by ID."""
    seen: set = set()
    got_any = False
    for typ in ("trigger", "schedule"):
        try:
            # --limit 5000 defeats kd's default page size of 20, which would
            # otherwise truncate the ground-truth count.
            out = subprocess.run(
                ["kd", "list", "--type", typ, "--limit", "5000", "--json"],
                capture_output=True, text=True, timeout=60,
            )
            if out.returncode != 0 or not out.stdout.strip():
                warnings.append(f"kd list --type={typ} failed rc={out.returncode}")
                continue
            data = json.loads(out.stdout)
            for b in data:
                bid = b.get("id")
                if bid and bid not in seen and _is_gardener(b):
                    seen.add(bid)
            got_any = True
        except Exception as e:
            warnings.append(f"kd list --type={typ} error: {e}")
    return len(seen) if got_any else None


def kd_runs_spawned(detail_gardener: str, warnings: list) -> int | None:
    if not detail_gardener:
        return None
    try:
        out = subprocess.run(
            ["kd", "show", detail_gardener, "--json"],
            capture_output=True, text=True, timeout=60,
        )
        if out.returncode != 0 or not out.stdout.strip():
            warnings.append(f"kd show {detail_gardener} failed rc={out.returncode}")
            return None
        bead = json.loads(out.stdout)
        cap = (bead.get("fields") or {}).get("gardener")
        if isinstance(cap, str):
            cap = json.loads(cap)
        if isinstance(cap, dict):
            return cap.get("runs_spawned")
    except Exception as e:
        warnings.append(f"kd show {detail_gardener} runs_spawned error: {e}")
    return None


# --- row / verdict helpers ----------------------------------------------------
CAP_ROW_RE = re.compile(r'class="cap-row')
VERDICT_RE = re.compile(r"gardener-verdict gardener-verdict-(\w+)")
VERDICT_KINDS = ("progress", "stalled", "stuck", "neutral", "needs-attention")


def count_rows(raw: str) -> int:
    return len(CAP_ROW_RE.findall(raw))


# Last-run TLDR span as rendered by templates/gardeners.html:
#   <span class="cap-lastrun-tldr" title="...">{{.LastRunTLDR}}</span>
LASTRUN_TLDR_RE = re.compile(r'class="cap-lastrun-tldr"[^>]*>(.*?)</span>', re.S)


def count_lastrun_tldr(raw: str) -> int:
    """Count NON-EMPTY last-run TLDR spans across the whole gardeners index."""
    n = 0
    for inner in LASTRUN_TLDR_RE.findall(_body(raw)):
        if html.unescape(TAG_INNER.sub(" ", inner)).strip():
            n += 1
    return n


def verdict_tally(raw: str) -> dict:
    body = _body(raw)
    counts = {k: 0 for k in VERDICT_KINDS}
    for kind in VERDICT_RE.findall(body):
        if kind in counts:
            counts[kind] += 1
    return counts


# --- main ---------------------------------------------------------------------
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True, help="captures dir (html/, screenshots/)")
    ap.add_argument("--out", required=True, help="output metrics.json path")
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--detail-gardener", default="", help="gardener id for /gardener/{id}")
    args = ap.parse_args()

    warnings: list = []
    cap = Path(args.captures)
    html_dir = cap / "html"

    idx_raw = _read(html_dir / "gardeners-index.html")
    det_raw = _read(html_dir / "gardener-detail.html")
    na_raw = _read(html_dir / "gardeners-needs-attention.html")
    if not idx_raw:
        warnings.append("gardeners-index.html missing or empty")

    idx_body = _body(idx_raw)
    det_body = _body(det_raw)
    idx_text = _visible_text(idx_raw)
    det_text = _visible_text(det_raw)

    # ---------- Polish / Density ----------
    polish = {
        "gardeners-index": polish_for(cap, "gardeners-index", warnings),
        "gardener-detail": polish_for(cap, "gardener-detail", warnings),
    }

    # ---------- Coverage ----------
    rendered_row_count = count_rows(idx_raw)
    kd_total = kd_gardener_count(warnings)
    chips = parse_chips(idx_raw)
    chip_all = _chip_lookup(chips, "all")
    chip_active = _chip_lookup(chips, "active")
    chip_disabled = _chip_lookup(chips, "disabled")
    chip_closed = _chip_lookup(chips, "closed")
    # Active + Disabled + Closed is the mutually-exclusive lifecycle partition.
    chip_sum_consistent = 0
    if None not in (chip_all, chip_active, chip_disabled, chip_closed):
        chip_sum_consistent = 1 if (chip_active + chip_disabled + chip_closed) == chip_all else 0
    needs_attention_rendered = count_rows(na_raw)

    # Hidden-roster reconciliation (bug ka-0000007605). The All chip is the LIVE
    # fleet; the page deliberately excludes E2E fixtures + schedule-mirror
    # artifacts and now accounts for them via the "N hidden from this roster"
    # disclosure. row_vs_truth_delta therefore is EXPECTED to be nonzero (the
    # excluded class), so the load-bearing Coverage signal is no longer "delta==0"
    # but "delta is fully explained on the page": rendered + hidden == kd ground
    # truth. hidden_reconciles==1 means every gardener bead is accounted for —
    # either rendered as a row or disclosed as a deliberate exclusion — so there
    # is no silent hole.
    hidden_count = parse_hidden_count(idx_raw)
    hidden_reconciles = None
    if kd_total is not None:
        accounted = rendered_row_count + (hidden_count or 0)
        hidden_reconciles = 1 if accounted == kd_total else 0

    coverage = {
        "rendered_row_count": rendered_row_count,
        "kd_total_gardener_count": kd_total,
        "row_vs_truth_delta": (kd_total - rendered_row_count) if kd_total is not None else None,
        "hidden_count": hidden_count,
        "hidden_reconciles": hidden_reconciles,
        "chip_counts": chips,
        "chip_sum_consistent": chip_sum_consistent,
        "needs_attention_rendered": needs_attention_rendered,
    }

    # ---------- Accuracy ----------
    detail_iteration_cards = len(re.findall(r"gardener-iter(?![-\w])", det_body))
    spec_runs_spawned = kd_runs_spawned(args.detail_gardener, warnings)
    iteration_count_drift = 1 if re.search(
        r"counter[- ]drift|reconcile|count mismatch|advertised \d+", det_body, re.I
    ) else 0
    verdict_present = 1 if re.search(r"gardener-verdict gardener-verdict-\w+", det_body) else 0

    # Last-run TLDR is rendered page-wide as <span class="cap-lastrun-tldr">…</span>
    # (one per gardener that has a completed run). Count NON-EMPTY spans across the
    # whole index — keying presence off a SINGLE detail-gardener's row was a false
    # negative: a page that renders 19 TLDR spans reads 0 whenever that one chosen
    # gardener happens to lack a completed run. Page-wide count is detail-independent.
    last_run_tldr_count = count_lastrun_tldr(idx_raw)

    accuracy = {
        "detail_iteration_cards": detail_iteration_cards,
        "spec_runs_spawned": spec_runs_spawned,
        "iteration_count_drift": iteration_count_drift,
        "verdict_present": verdict_present,
        "last_run_tldr_present": 1 if last_run_tldr_count > 0 else 0,
        "last_run_tldr_count": last_run_tldr_count,
        "verdict_classes": verdict_tally(idx_raw),
    }

    # ---------- Management ----------
    filter_chip_count = len(CHIP_RE.findall(idx_raw))
    next_fire_present = len(re.findall(r'class="cap-next', idx_body))
    # operator control bar (enable/disable/lifecycle form or hx-mutation) on detail
    control_bar_present = 1 if re.search(
        r'gardener-control|control-bar|hx-(post|put|patch|delete)|'
        r'<(form|button)[^>]*gardener[^>]*(enable|disable)',
        det_body, re.I,
    ) else 0
    # Capture scope: the whole operator surface on /gardener/{id} is gated behind
    # {{if .IsOperator}} (the control bar + the #gardener-config-host edit slot).
    # If the capture token is viewer-scoped (the default gardenio-info-density-
    # audit client, realm role gardenio-viewer) the operator markup is correctly
    # withheld and control_bar_present can NEVER be 1 — that is a measurement
    # gap, not a missing feature (bug ka-0000007161). The config-host div is the
    # most reliable operator-scope tell: it renders for every operator,
    # independent of the gardener's enabled/closed state. Capture as operator via
    #   fetch-beads-viewer-prod.sh --operator   (KC_CLIENT_ID=gardenio-camp-audit)
    # so this flips to 1 and control_bar_present becomes meaningful.
    capture_is_operator = 1 if re.search(
        r'id="gardener-config-host"|gardener-controls-label|/gardener/[^"/]+/(enable|disable|retire)',
        det_body, re.I,
    ) else 0
    if not capture_is_operator:
        warnings.append(
            "gardener-detail captured WITHOUT operator scope (no operator markup "
            "present): control_bar_present is unmeasurable from a viewer token — "
            "re-capture with fetch-beads-viewer-prod.sh --operator "
            "(KC_CLIENT_ID=gardenio-camp-audit). See bug ka-0000007161."
        )
    runs_lazy_link = 1 if re.search(r'gardener-runs-lazy|hx-get="[^"]*/runs"', det_body) else 0
    # running-first ordering: a running gardener tbody (cap-tbody cap-running) sorts
    # ahead of the rest, or an explicit sort affordance (aria-sort/data-sort) exists.
    first_tbody = re.search(r'<tbody[^>]*class="cap-tbody[^"]*"', idx_body)
    first_row_running = bool(first_tbody and "cap-running" in (first_tbody.group(0)))
    sort_indicator = 1 if (
        first_row_running
        or re.search(r'aria-sort=|data-sort=|running-first', idx_body, re.I)
    ) else 0

    management = {
        "filter_chip_count": filter_chip_count,
        "next_fire_present": next_fire_present,
        "control_bar_present": control_bar_present,
        "capture_is_operator": capture_is_operator,
        "runs_lazy_link": runs_lazy_link,
        "sort_indicator": sort_indicator,
    }

    # ---------- Correctness ----------
    def leaks(*texts):
        n = 0
        for tx in texts:
            n += tx.count("{{") + tx.count("}}") + tx.count("<no value>")
        return n

    def errmarks(*texts):
        n = 0
        for tx in texts:
            n += len(re.findall(r"\b(error|failed to|nil|panic)\b", tx, re.I))
        return n

    # template leaks check raw body (catch {{ in attrs too); errors in visible text
    template_leaks = leaks(idx_body, det_body)
    error_markers = errmarks(idx_text, det_text)
    empty_table = 1 if (rendered_row_count == 0 and (kd_total or 0) > 0) else 0

    correctness = {
        "template_leaks": template_leaks,
        "error_markers": error_markers,
        "empty_table": empty_table,
    }

    out = {
        "target_url": args.target_url,
        "detail_gardener": args.detail_gardener,
        "iteration": args.iteration,
        "polish": polish,
        "coverage": coverage,
        "accuracy": accuracy,
        "management": management,
        "correctness": correctness,
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
    if empty_table:
        flags.append("empty_table=1")
    if chip_sum_consistent == 0:
        flags.append("chip_sum_consistent=0")
    if flags:
        print("FAIL-FLAGS: " + ", ".join(flags), file=sys.stderr)
    for w in warnings:
        print(f"WARN: {w}", file=sys.stderr)


if __name__ == "__main__":
    main()
