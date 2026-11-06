#!/usr/bin/env python3
"""Compute objective PACES metrics for the plans-ux-audit harness.

Layered on top of measure-density.py's pixel primitives: ingests a plans-ux
captures dir (html/plans-index.html + html/plan-detail.html and
screenshots/{light,dark}/{desktop,narrow}/{plans-index,plan-detail}.png) plus
a ground-truth kd walk, and emits metrics.json across the five PACES
dimensions — Plan clarity, Assistant, Cockpit, Ergonomics, Skin — for the
/plans index and the /plans/{id} cockpit.

Rubric: gardenio/docs/plans-ux-rubric.md §2. Formula:
gardenio/docs/plans-ux-audit-formula.json step `measure`. Per gd-tiDMyL7Lu4 —
the P (Plan clarity) block additionally computes the data-density signals the
plan calls out: distinct data fields per card and the desktop whitespace
(near-white) ratio, so an iteration can prove density improved (more fields,
same footprint) rather than merely not-regressed.

DARK-SKIN PASS (ka-0000000260): the capture driver now renders BOTH the light
AND the dark skin (see capture-plans.mjs). Before this, the driver hardcoded
the light skin, so the S (Skin) dimension was structurally blind to dark-skin
regressions — the exact debt iteration-16 (ka-0000007937) had to pay off
retroactively when the whole board rendered as white cards on a black page.
measure_skin now scores the dark capture too: it records the dark screenshots
in the output block so the judge sees them (a dark capture that renders IS the
signal), and computes a dark-legibility proxy. The proxy keys on the SAME
near-white pixel ratio used for light density, but inverts its meaning: on a
healthy dark skin the near-white ratio is ~0 (dark canvas, dark cards); a
SPIKE above DARK_WHITE_ISLAND_THRESHOLD is the literal signature of the
iteration-16 regression (white #fff card islands on the black page) and trips
dark_white_island_flag — so re-introducing that class of regression scores as
a flagged Skin dimension instead of an invisible gap.

The light pass is byte-identical to before, so every pre-existing light metric
value is unchanged; only the screenshot PATHS moved under screenshots/light/
(read with a legacy fallback below). The dark pass is purely additive.

The judge skips null screenshot blocks, so an unreadable capture is recorded
as null + a warning, never a hard fail.
"""
import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

# Pillow/numpy degrade gracefully rather than sys.exit at import — importing
# this module must never abort the process (the hermetic unit test loads it by
# path on a CI runner that may lack Pillow, mirroring measure-formula-ux.py).
# png_metrics() below reports null + a warning when Image is None, which the
# judge already treats as a skipped screenshot block, never a hard fail.
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

# Strip <style> / <script> blocks so "correctness" greps only see rendered body.
STYLE_RE = re.compile(r"<style[^>]*>.*?</style>", re.IGNORECASE | re.DOTALL)
SCRIPT_RE = re.compile(r"<script[^>]*>.*?</script>", re.IGNORECASE | re.DOTALL)

# Per-card "distinct data field" markers (P-density). Each is one legible datum
# an operator can read off a single /plans card. Counted per rendered card so
# the roll-up is fields-per-card (density), not raw ink.
CARD_FIELD_CLASSES = [
    "plan-card-title",         # what it is
    "plan-card-id",            # stable id
    "chip-status",             # status
    "plan-lifecycle-chip",     # NEW (ka-0000000851): draft/ready/building
    "chip-type",               # project
    "plan-card-creator",       # whose
    "chip-attention",          # open decisions ("N to answer")
    "plan-esc-badge",          # blocked-by-escalation
    "plan-stall-badge",        # stalled
    "plan-card-updated",       # freshness
    "plan-progress-label",     # closed/total + %
    "plan-step-tally",         # NEW (ka-0000000851): ▶ in-progress / ⛔ blocked
    "plan-taskgrid",           # phase×task color grid
    "plan-card-step",          # now/next step
    "plan-agent-chip",         # unified gardener/assistant state
    "plan-card-crew",          # NEW (ka-0000004801): ● N crew · M working
    "plan-card-agent-link",    # terminal link
    "plan-card-dispatch",      # dispatch-next button (operator)
]

# The three fields THIS iteration (epic ka-0000005352) added — tracked
# separately so the report can attribute the density delta.
NEW_THIS_ITERATION = ["plan-lifecycle-chip", "plan-step-tally", "plan-card-crew"]

# Dark-legibility threshold (ka-0000000260). On a healthy dark skin the
# near-white (>=240 on all channels) pixel ratio of the /plans board is
# near-zero: the page canvas is #0b0f14 and iteration-16 gave the cards an
# actual dark surface (#14181d). The iteration-16 regression was the OPPOSITE
# — every card rendered a pure-#fff rectangle, which pushes the near-white
# ratio into the double digits of percent. 3% is comfortably above the healthy
# floor (dark cards contribute ~0 near-white pixels; only rare bright glyphs
# do) yet far below a single white card island, so it cleanly separates
# "dark skin renders dark" from "white islands leaked back in".
DARK_WHITE_ISLAND_THRESHOLD = 0.03


def screenshot_path(cap: Path, skin: str, viewport: str, slug: str) -> Path:
    """Resolve a capture screenshot path with a legacy (pre-skin-axis) fallback.

    The capture driver now writes screenshots/{light,dark}/{desktop,narrow}/…;
    older capture dirs (and any hand-run of the pre-ka-0000000260 driver) wrote
    screenshots/{desktop,narrow}/… with no skin axis. For the LIGHT skin, fall
    back to the flat layout so we can still measure an old capture dir.
    """
    new = cap / "screenshots" / skin / viewport / f"{slug}.png"
    if new.exists() or skin != "light":
        return new
    return cap / "screenshots" / viewport / f"{slug}.png"


def read_text(path: Path) -> str:
    if not path.exists():
        return ""
    return path.read_text(encoding="utf-8", errors="replace")


def strip_code(html: str) -> str:
    return SCRIPT_RE.sub("", STYLE_RE.sub("", html))


def count(pattern: str, s: str, flags=0) -> int:
    return len(re.findall(pattern, s, flags))


def kd_json(args_list):
    """Run a kd command returning JSON; tolerate failure with []."""
    try:
        out = subprocess.check_output(args_list, text=True, timeout=60)
        return json.loads(out)
    except Exception as e:  # noqa: BLE001
        print(f"warn: {' '.join(args_list)} failed: {e}", file=sys.stderr)
        return None


def png_metrics(path: Path) -> dict:
    if not path.exists():
        return None
    if Image is None:
        print(f"warn: png_metrics({path}) skipped: Pillow not installed", file=sys.stderr)
        return None
    try:
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
                "width": w,
                "height": h,
                "pixel_count": total,
                "near_white_ratio": round(near_white / total, 4),
                "mean_luma": round(mean_luma, 4),
            }
    except Exception as e:  # noqa: BLE001
        print(f"warn: png_metrics({path}) failed: {e}", file=sys.stderr)
        return None


def measure_plan_clarity(index_html: str, plan_epic_count, desktop_index_png):
    body = strip_code(index_html)
    # One <li class="plan-card"> per plan (id="plan-card-…" is the stable key).
    card_count = count(r'<li[^>]*class="[^"]*plan-card[^"]*"', body)
    # Open-status cards only — the default board also renders a bounded
    # recent-closed "Done" lane, so the P card-vs-truth check compares the
    # OPEN scope (what "every plan in scope renders one card" means) rather
    # than counting the Done-lane cards as phantom.
    open_card_count = count(r'chip chip-status chip-status-open', body)
    closed_card_count = count(r'chip chip-status chip-status-closed', body)
    cards_with_progress = count(r'class="plan-progress-label"', body)
    cards_with_next_step = count(r'class="plan-card-step', body)

    # P-density: distinct data-field classes present, and the per-card average.
    field_totals = {}
    for cls in CARD_FIELD_CLASSES:
        field_totals[cls] = count(r'class="[^"]*\b' + re.escape(cls) + r'\b', body)
    total_fields = sum(field_totals.values())
    distinct_fields_present = sum(1 for v in field_totals.values() if v > 0)
    fields_per_card = round(total_fields / card_count, 2) if card_count else 0
    new_field_cards = {c: field_totals.get(c, 0) for c in NEW_THIS_ITERATION}

    return {
        "plan_card_count": card_count,
        "open_card_count": open_card_count,
        "closed_card_count": closed_card_count,
        "kd_list_plan_epic_count": plan_epic_count,
        "card_vs_truth_delta": (plan_epic_count - open_card_count) if plan_epic_count is not None else None,
        "cards_with_progress": cards_with_progress,
        "cards_with_next_step": cards_with_next_step,
        # ---- P-density (gd-tiDMyL7Lu4) ----
        "distinct_field_types_present": distinct_fields_present,
        "distinct_field_types_possible": len(CARD_FIELD_CLASSES),
        "total_field_instances": total_fields,
        "fields_per_card": fields_per_card,
        "field_instance_breakdown": field_totals,
        "new_fields_this_iteration": new_field_cards,
        "desktop_index_near_white_ratio": (desktop_index_png or {}).get("near_white_ratio"),
        "desktop_index_mean_luma": (desktop_index_png or {}).get("mean_luma"),
    }


def measure_assistant(index_html: str, detail_html: str):
    idx = strip_code(index_html)
    det = strip_code(detail_html)
    chip_live = count(r'plan-agent-chip[^"]*(?:is-live|--live|-live)', idx)
    chip_any = count(r'class="[^"]*plan-agent-chip', idx)
    m = re.search(r'<section[^>]*id="plan-agent"[^>]*data-state="([^"]*)"', det)
    cockpit_state = m.group(1) if m else None
    return {
        "assistant_chip_instances_index": chip_any,
        "assistant_chip_live_index": chip_live,
        "cockpit_lane_state": cockpit_state,
        "spawn_form_present": 1 if re.search(r'class="[^"]*plan-assistant-form', det) else 0,
        "dispatch_next_present_index": 1 if re.search(r'class="[^"]*plan-card-dispatch-btn', idx) else 0,
        "open_link_present": 1 if re.search(r'class="[^"]*plan-agent-terminal', det) or re.search(r'class="[^"]*plan-card-agent-link', idx) else 0,
    }


def measure_cockpit(detail_html: str, child_issue_count):
    det = strip_code(detail_html)
    step_count = count(r'<li[^>]*class="[^"]*plan-tl-step', det)
    is_done = count(r'plan-tl-step[^"]*is-done', det)
    is_active = count(r'plan-tl-step[^"]*is-active', det)
    m = re.search(r'<section[^>]*id="plan-narrative"[^>]*>(.*?)</section>', det, re.DOTALL)
    narrative_body = re.sub(r"<[^>]+>", "", m.group(1)).strip() if m else ""
    return {
        "timeline_step_count": step_count,
        "direct_issue_child_count": child_issue_count,
        "step_vs_truth_delta": (child_issue_count - step_count) if child_issue_count is not None else None,
        "is_done_count": is_done,
        "is_active_count": is_active,
        "narrative_rendered": 1 if len(narrative_body) > 40 else 0,
        "narrative_chars": len(narrative_body),
    }


def measure_ergonomics(detail_html: str):
    det = strip_code(detail_html)
    controls = 0
    for pat in [r'plan-step-add', r'hx-post="[^"]*/close"', r'hx-post="[^"]*/reopen"',
                r'phase-reorder', r'plan-step-mark', r'hx-post="[^"]*/dispatch']:
        controls += count(pat, det)
    sse = 1 if re.search(r'data-plan-events', det) or re.search(r'/plans/[^"/]+/events', det) else 0
    return {
        "operator_control_hits": controls,
        "sse_anchor_present": sse,
    }


def measure_skin(render_metrics: dict, desktop_pngs: dict, dark_render_metrics: dict, dark_desktop_pngs: dict):
    idx = render_metrics.get("plans-index__narrow", {})
    det = render_metrics.get("plan-detail__narrow", {})
    det_desktop = render_metrics.get("plan-detail__desktop", {})

    # ---- Dark skin (ka-0000000260) ----
    # dark_render_metrics are the entries the capture driver suffixes "__dark".
    d_idx = dark_render_metrics.get("plans-index__narrow__dark", {})
    d_det = dark_render_metrics.get("plan-detail__narrow__dark", {})
    d_det_desktop = dark_render_metrics.get("plan-detail__desktop__dark", {})
    d_idx_png = dark_desktop_pngs.get("plans-index") or {}
    d_det_png = dark_desktop_pngs.get("plan-detail") or {}
    d_idx_white = d_idx_png.get("near_white_ratio")
    d_det_white = d_det_png.get("near_white_ratio")
    # The dark-legibility proxy: on the dark skin, near-white pixels should be
    # near-zero. A spike is the white-#fff-island regression (iteration-16).
    # A rendered dark capture existing at all is ALSO a signal (the judge sees
    # the PNG); this flag makes the white-island failure machine-checkable so it
    # can never silently re-accrue.
    dark_captured = bool(d_idx_png) or bool(d_det_png)
    dark_white_ratios = [r for r in (d_idx_white, d_det_white) if r is not None]
    dark_white_island_flag = (
        any(r > DARK_WHITE_ISLAND_THRESHOLD for r in dark_white_ratios)
        if dark_white_ratios else None
    )
    # Confirm the dark canvas actually rendered dark — mean_luma should be low
    # (< 0.5) on a dark skin; a high value means the dark capture didn't take.
    d_idx_luma = d_idx_png.get("mean_luma")
    d_det_luma = d_det_png.get("mean_luma")

    return {
        "overflow_index_narrow": idx.get("overflow"),
        "overflow_detail_narrow": det.get("overflow"),
        "grid_tracks_narrow": det.get("grid_tracks"),
        "grid_tracks_desktop": det_desktop.get("grid_tracks"),
        "desktop_index_near_white_ratio": (desktop_pngs.get("plans-index") or {}).get("near_white_ratio"),
        "desktop_detail_near_white_ratio": (desktop_pngs.get("plan-detail") or {}).get("near_white_ratio"),
        # ---- Dark skin (additive) ----
        "dark_captured": dark_captured,
        "dark_overflow_index_narrow": d_idx.get("overflow"),
        "dark_overflow_detail_narrow": d_det.get("overflow"),
        "dark_grid_tracks_narrow": d_det.get("grid_tracks"),
        "dark_grid_tracks_desktop": d_det_desktop.get("grid_tracks"),
        "dark_data_skin_index": d_idx.get("skin"),
        "dark_data_skin_detail": d_det.get("skin"),
        "dark_desktop_index_near_white_ratio": d_idx_white,
        "dark_desktop_detail_near_white_ratio": d_det_white,
        "dark_desktop_index_mean_luma": d_idx_luma,
        "dark_desktop_detail_mean_luma": d_det_luma,
        "dark_white_island_threshold": DARK_WHITE_ISLAND_THRESHOLD,
        "dark_white_island_flag": dark_white_island_flag,
    }


def measure_correctness(index_html: str, detail_html: str):
    out = {}
    for name, html in (("index", index_html), ("detail", detail_html)):
        body = strip_code(html)
        # Template leaks are a raw-body concern (an unrendered {{...}} leaks
        # verbatim into markup), so grep the tag-bearing body.
        out[f"template_leaks_{name}"] = count(r"\{\{|\}\}|<no value>", body)
        # Error markers must be RENDERED text, not markup: strip every tag
        # (and thus every inline attribute / event handler) so a JS fallback
        # string baked into an hx-on handler — e.g. "Message failed to send"
        # — is not miscounted as a live server error (gd-tiDMyL7Lu4 finding).
        visible = re.sub(r"<[^>]+>", " ", body)
        out[f"error_markers_{name}"] = count(r"\b(panic|failed to|<nil>)\b", visible, re.IGNORECASE)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--plan-id", required=True)
    ap.add_argument("--plan-epic-count", type=int, default=None,
                    help="ground-truth open plan-labeled epic count (grid+open scope)")
    ap.add_argument("--child-issue-count", type=int, default=None,
                    help="ground-truth direct issue-child count of the plan")
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    index_html = read_text(html_dir / "plans-index.html")
    detail_html = read_text(html_dir / "plan-detail.html")

    render_metrics = {}
    rm_path = cap / "render-metrics.json"
    if rm_path.exists():
        render_metrics = json.loads(read_text(rm_path))
    # render-metrics.json interleaves light + dark keys; the dark keys carry a
    # "__dark" suffix (see capture-plans.mjs). Split so the light reads below
    # stay byte-identical and measure_skin gets the dark entries separately.
    light_render_metrics = {k: v for k, v in render_metrics.items() if not k.endswith("__dark")}
    dark_render_metrics = {k: v for k, v in render_metrics.items() if k.endswith("__dark")}

    # Light screenshots — screenshot_path() falls back to the legacy flat
    # (pre-skin-axis) layout for old capture dirs so this still measures them.
    desktop_pngs = {
        "plans-index": png_metrics(screenshot_path(cap, "light", "desktop", "plans-index")),
        "plan-detail": png_metrics(screenshot_path(cap, "light", "desktop", "plan-detail")),
    }
    narrow_pngs = {
        "plans-index": png_metrics(screenshot_path(cap, "light", "narrow", "plans-index")),
        "plan-detail": png_metrics(screenshot_path(cap, "light", "narrow", "plan-detail")),
    }
    # Dark screenshots (ka-0000000260) — additive; null when a dark capture is
    # absent (e.g. an old capture dir), which the judge treats as a skipped
    # block, never a hard fail.
    dark_desktop_pngs = {
        "plans-index": png_metrics(screenshot_path(cap, "dark", "desktop", "plans-index")),
        "plan-detail": png_metrics(screenshot_path(cap, "dark", "desktop", "plan-detail")),
    }
    dark_narrow_pngs = {
        "plans-index": png_metrics(screenshot_path(cap, "dark", "narrow", "plans-index")),
        "plan-detail": png_metrics(screenshot_path(cap, "dark", "narrow", "plan-detail")),
    }

    metrics = {
        "iteration": args.iteration,
        "target_url": args.target_url,
        "plan_id": args.plan_id,
        "plan": measure_plan_clarity(index_html, args.plan_epic_count, desktop_pngs["plans-index"]),
        "assistant": measure_assistant(index_html, detail_html),
        "cockpit": measure_cockpit(detail_html, args.child_issue_count),
        "ergonomics": measure_ergonomics(detail_html),
        "skin": measure_skin(light_render_metrics, desktop_pngs, dark_render_metrics, dark_desktop_pngs),
        "correctness": measure_correctness(index_html, detail_html),
        "screenshots": {
            "light": {"desktop": desktop_pngs, "narrow": narrow_pngs},
            "dark": {"desktop": dark_desktop_pngs, "narrow": dark_narrow_pngs},
            # Back-compat: keep the flat desktop/narrow keys pointing at the
            # light capture so any downstream reader of the old shape (and the
            # existing report embeds) keeps working. The skin-split light/dark
            # blocks above are the new canonical shape.
            "desktop": desktop_pngs,
            "narrow": narrow_pngs,
        },
    }

    Path(args.out).write_text(json.dumps(metrics, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
