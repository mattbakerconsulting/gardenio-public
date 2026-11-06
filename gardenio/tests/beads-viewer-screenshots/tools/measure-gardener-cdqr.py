#!/usr/bin/env python3
"""Compute objective CDQR metrics for the Gardener-Page audit harness.

Reuses the density primitives from measure-density.py (near_white_ratio,
mean_luma, dom_element_count, data_fields, ...) and layers on the
gardener-page-specific Correctness / Density / Relevance / Operability signals.

Emits the *canonical* CDQR metrics.json shape the judge step consumes:

  {
    "target_url", "gardener_id", "iteration",
    "density":     {"desktop": {...}, "narrow": {...}, "html": {...}},
    "correctness": {error_markers, empty_section_markers, broken_links,
                    template_leaks, htmx_unresolved, counter_consistency_ok,
                    bytime_ribbons, ...},
    "relevance":   {header_fields_present, header_fields_populated,
                    iteration_cards, live_count, terminated_count,
                    wrapup_coverage, progress_narrative, ...},
    "operability": {read_fields, write_affordances, config_editability,
                    routes_wired, operator_gating_present, rwc_loop_closed, ...}
  }

The gardener page evolved substantially since the 2026-06-01 iteration
(header redesign: `Iterations` distinct from `Engine count · lifetime tally
N spawned / M completed`; a `gardener-verdict` progress-narrative band; an
operator `gardener-controls` bar + inline `gardener-config` edit form; run
ribbons `Iteration N of MAX` with intact descending order instead of the old
`(by time)` heuristic). The signal extraction below greps the *current* DOM
markers, cross-references the `captures/operability/routes.txt` probe for the
Operability route-coverage fraction, and stays backward-tolerant so a future
render tweak degrades a signal to null rather than crashing.

Usage:
  measure-gardener-cdqr.py --captures <dir> --out metrics.json \
    --target-url <url> --gardener-id <id> --iteration <iso> \
    [--runs-html section-runs.html]
"""
import argparse
import html as _html
import json
import re
import sys
from pathlib import Path

import importlib.util as _ilu

_md_path = Path(__file__).resolve().parent / "measure-density.py"
_spec = _ilu.spec_from_file_location("measure_density", _md_path)
_md = _ilu.module_from_spec(_spec)
_spec.loader.exec_module(_md)
html_metrics = _md.html_metrics
png_metrics = _md.png_metrics

TAG_INNER = re.compile(r"<[^>]+>")


def _text(s: str) -> str:
    return _html.unescape(re.sub(r"\s+", " ", TAG_INNER.sub(" ", s))).strip()


def _count(pat: str, s: str, flags=re.I) -> int:
    return len(re.findall(pat, s, flags))


def _excise_balanced_div(html: str, anchor_pat: str) -> str:
    """Return `html` with the balanced <div>...</div> whose opening tag contains
    `anchor_pat` removed. Used to drop the Connected-beads data panel
    (<div id="cap-beads-host" ...>) before grepping the gardener page's OWN
    chrome for correctness smells.

    The panel embeds up to hundreds of *other* beads' titles verbatim. Those
    titles carry their own content — including unexpanded '{{...}}' template
    tokens from mis-authored beads elsewhere and their own 'Iteration N of M'
    strings — which are DATA the page faithfully renders, not gardener-page
    template mis-renders or run-chain breaks. Grepping the whole DOM therefore
    produced a false template_leaks spike and a false intact_run_chain break
    every iteration (documented 2026-07-03 / 2026-07-11). Scoping the
    correctness smells to the chrome (page minus this panel) fixes that at the
    source. If the panel is absent (older gardeners / render tweak), the input is
    returned unchanged.
    """
    m = re.search(anchor_pat, html)
    if not m:
        return html
    open_start = html.rfind("<div", 0, m.start())
    if open_start < 0:
        return html
    depth = 0
    for tok in re.finditer(r"<div\b|</div>", html[open_start:]):
        if tok.group(0) == "</div>":
            depth -= 1
            if depth == 0:
                end = open_start + tok.end()
                return html[:open_start] + html[end:]
        else:
            depth += 1
    return html  # unbalanced markup — leave as-is rather than truncate


def _stat_after(label: str, html: str):
    """Extract the first integer rendered after a label, tolerant of the
    header's <dt>label</dt><dd>value</dd> or "label N" inline layouts."""
    m = re.search(re.escape(label) + r"\s*</[^>]+>\s*<[^>]+>\s*([0-9,]+)", html, re.I)
    if m:
        return int(m.group(1).replace(",", ""))
    m = re.search(re.escape(label) + r"[^0-9<]{0,40}?([0-9][0-9,]*)", html, re.I)
    if m:
        return int(m.group(1).replace(",", ""))
    return None


def gardener_signals(page_html_path: Path, runs_html_path: Path | None,
                    routes_txt_path: Path | None) -> dict:
    full = page_html_path.read_text(errors="replace")
    body = full[full.find("<body>"):] if "<body>" in full else full
    runs = ""
    if runs_html_path and runs_html_path.exists():
        runs = runs_html_path.read_text(errors="replace")

    # Chrome = the gardener page minus the Connected-beads data panel. Correctness
    # smells (template leaks, run-chain ordering, broken links) are scored against
    # the chrome so the panel's embedded *other-bead* content (which the page only
    # renders, doesn't author) stops producing false Correctness fails. See
    # _excise_balanced_div. `_panel_excised` records whether the panel was found.
    chrome = _excise_balanced_div(body, r'id="cap-beads-host"')
    panel_excised = len(chrome) < len(body)

    # ---------------- rendered iteration cards ----------------
    page_cards = _count(r'<li class="bead-row gardener-iter\b', body)
    runs_cards = _count(r'<li class="bead-row gardener-iter\b', runs)
    iter_cards = page_cards + runs_cards
    terminated = _count(r"bead-row-terminated", body) + _count(r"bead-row-terminated", runs)
    live = (_count(r"bead-row-live|agent-state-live|agent-state-running|gardener-iter-live", body)
            + _count(r"bead-row-live|agent-state-live|agent-state-running", runs))
    didnotrun = _count(r"did[- ]?not[- ]?run|never ran|gardener-iter-didnotrun", body + runs)

    # ---------------- header counters (advertised) ----------------
    iterations_stat = _stat_after("Iterations", body)
    m = re.search(r"([0-9,]+)\s*spawned\s*/\s*([0-9,]+)\s*completed", body, re.I)
    runs_spawned = int(m.group(1).replace(",", "")) if m else _stat_after("Runs spawned", body)
    runs_completed = int(m.group(2).replace(",", "")) if m else _stat_after("Runs completed", body)
    run_budget = _stat_after("Run budget", body)
    max_runs = _stat_after("Max runs", body)

    m = re.search(r"Life-thread.{0,80}?\((\d+)\s*(?:iteration|run)s?\)", body, re.S | re.I)
    thread_heading = int(m.group(1)) if m else None

    # ---------------- run ordering ----------------
    # Score the run-chain against the CHROME (panel excised) so the Connected-beads
    # panel's embedded other-bead titles — some of which literally contain
    # "Iteration N of M" strings — can't corrupt the sequence. The page and the
    # /runs tail are two SEPARATE ordered fragments (the page shows the newest run
    # window, /runs shows the older tail); concatenating them created a false
    # ascending jump at the seam. Score each fragment's own monotonicity instead.
    bytime_ribbons = _count(r"gardener-run-bytime", chrome + runs)
    run_labels = re.findall(r'gardener-run-label[^>]*>(.*?)</', chrome + runs, re.S)
    bytime_labels = sum(1 for l in run_labels if "by time" in _text(l).lower())

    def _descending(text):
        ns = [int(n) for n in re.findall(r"Iteration\s+(\d+)\s+of\s+\d+", _text(text))]
        ok = bool(ns) and all(ns[i] >= ns[i + 1] for i in range(len(ns) - 1))
        return ns, ok

    page_iter_nums, page_ok = _descending(chrome)
    runs_iter_nums, runs_ok = _descending(runs)
    iter_nums = page_iter_nums  # canonical sample = the page window
    intact_run_chain = (page_ok and (runs_ok or not runs_iter_nums)
                        and bytime_labels == 0)

    # ---------------- per-run wrap-up (OUTCOME) coverage ----------------
    exit_chips = _count(r"gardener-iter-exit\b|gardener-iter-exit-ok|exit reason", body + runs)
    titles = re.findall(r'class="bead-row-title"[^>]*>(.*?)</a>', body + runs, re.S)
    substantive = sum(1 for t in titles if len(_text(t)) >= 40)
    wrapup_cards = max(exit_chips, substantive)
    wrapup_coverage = round(min(wrapup_cards, iter_cards) / iter_cards, 3) if iter_cards else None

    # ---------------- cross-run progress narrative ----------------
    verdict = re.search(r'class="gardener-verdict[^"]*"[^>]*>(.*?)</[^>]+>\s*'
                        r'(?:<[^>]*gardener-verdict-detail[^>]*>(.*?)</)?', body, re.S)
    verdict_text = _text(verdict.group(0)) if verdict else ""
    progress_state_json = bool(re.search(r"Progress state.{0,40}?\{", body, re.S | re.I))
    progress_narrative = bool(verdict_text) or progress_state_json or \
        bool(re.search(r"progress[- ]narrative|cross[- ]run|trajectory", body, re.I))
    spark_bars = _count(r"gardener-spark-bar", body)

    # ---------------- correctness smells ----------------
    # Scored against the CHROME (Connected-beads panel excised): the panel embeds
    # other beads' titles verbatim, so an unexpanded '{{...}}' token or a '{{'-in-href
    # there is DATA the page faithfully renders, not a gardener-page template
    # mis-render. `template_leaks_panel` is kept as an informational count of the
    # data-side corruption the panel surfaces (real, but upstream, not a page bug).
    template_leaks = _count(r"\{\{|\}\}|<no value>", chrome)
    template_leaks_panel = _count(r"\{\{|\}\}|<no value>", body) - template_leaks
    error_markers = (_count(r'class="[^"]*(?:render-)?error', chrome)
                     + _count(r"\bpanic\b|failed to|unprovisioned|\b5\d\d\b error", chrome))
    broken_links = (_count(r'href="#"', chrome) + _count(r'href=""', chrome)
                    + _count(r'href="/beads/gardener/"', chrome)
                    + _count(r'href="/beads/u/"', chrome)
                    + _count(r'href="[^"]*\{\{', chrome))
    empty_section = (_count(r"No runs yet|No older runs|No iterations", chrome + runs)
                     + _count(r'<ul class="bead-list"></ul>', chrome + runs))

    htmx_runs_targets = _count(r'hx-get="[^"]*/gardener/[^"]*runs"', body)
    runs_tail_empty = None
    htmx_unresolved = 0
    if runs_html_path and runs_html_path.exists():
        runs_tail_empty = (len(_text(runs)) == 0) or ("No older runs" in runs)
        more_exist = (runs_spawned or iterations_stat or 0) > page_cards
        if runs_tail_empty and more_exist:
            htmx_unresolved = 1

    # counter consistency: the *visible* iteration counters must agree. The
    # engine "lifetime tally" (spawned/completed) is EXPECTED to exceed the
    # iteration count and is honestly labelled distinct, so it is NOT a drift.
    counter_consistency_ok = None
    if iterations_stat is not None:
        counter_consistency_ok = (iterations_stat >= iter_cards) and (
            thread_heading is None or thread_heading == iterations_stat
            or thread_heading == iter_cards
        )

    # ---------------- header health-field coverage ----------------
    HEALTH = {
        "status_badge": r"gardener-goal-badge|gardener-badge|>\s*disabled\s*<|>\s*enabled\s*<",
        "owner_chip": r"owner\s+[A-Z]|gardener-header-meta",
        "terminal_goal": r"Terminal goal",
        "runs_spawned": r"spawned\s*/\s*[0-9]+\s*completed|Runs spawned|Engine count",
        "runs_completed": r"[0-9]+\s*completed|Runs completed",
        "run_budget": r"Run budget",
        "max_runs": r"Max runs",
        "consec_failures": r"consec|Consecutive failures|consecutive_failures",
        "stop_spec": r"Stop spec|loop_end|hard_stop",
        "progress_state": r"Progress state|gardener-verdict",
    }
    present = [k for k, pat in HEALTH.items() if re.search(pat, body, re.I)]
    populated = []
    if "status_badge" in present:
        populated.append("status_badge")
    if re.search(r"owner\s+\w", body, re.I):
        populated.append("owner_chip")
    if re.search(r"Terminal goal", body, re.I) and re.search(
            r"Terminal goal(?:</[^>]+>)?\s*(?:<[^>]+>)*\s*[A-Za-z0-9]", body, re.I):
        populated.append("terminal_goal")
    if runs_spawned:
        populated.append("runs_spawned")
    if runs_completed:
        populated.append("runs_completed")
    if run_budget:
        populated.append("run_budget")
    if max_runs:
        populated.append("max_runs")
    if "consec_failures" in present:
        populated.append("consec_failures")
    if re.search(r"hard_stop|loop_end", body):
        populated.append("stop_spec")
    if progress_narrative:
        populated.append("progress_state")

    # ---------------- operability ----------------
    ctl_buttons = _count(r"gardener-ctl\b", body)
    hx_mutations = _count(r"hx-post|hx-put|hx-patch|hx-delete", body)
    write_affordances = hx_mutations + ctl_buttons
    has_form = _count(r"<form\b", body)
    config_form_inline = _count(r"gardener-config-form", body) + _count(r'hx-get="[^"]*/edit"', body)
    config_editable_inputs = _count(
        r'gardener-config-field|<input[^>]+name="(?:terminal_goal|run_budget|max_runs|max_retries|exit_condition)"'
        r'|<textarea[^>]+name="terminal_goal"', body)
    config_editability = 1 if (config_form_inline or config_editable_inputs) else 0
    pre_json_blocks = _count(r"<pre", body)

    routes_wired = None
    route_detail = {}
    if routes_txt_path and routes_txt_path.exists():
        wired = 0
        total = 0
        for line in routes_txt_path.read_text().splitlines():
            m = re.match(r"(\S+)\s+\S+\s+(\S+)\s+->\s+(\d+)", line)
            if not m:
                continue
            slug, _path, code = m.group(1), m.group(2), int(m.group(3))
            route_detail[slug] = code
            total += 1
            # "wired" = implemented. 401/403 => the route EXISTS and is
            # auth-gated (implemented+protected); 405 on a GET-probe of a
            # POST-only route also => implemented.
            if code < 400 or code in (401, 403, 405):
                wired += 1
        routes_wired = round(wired / total, 3) if total else None

    gated = [c for s, c in route_detail.items()
             if s in {"enable", "disable", "runnow", "cancelrun", "config-post", "pausetoggle", "retire"}]
    if gated:
        operator_gating_present = all(c in (401, 403) for c in gated)
    elif write_affordances == 0:
        operator_gating_present = None
    else:
        operator_gating_present = False

    read_fields = len(present)
    rwc_loop_closed = bool(read_fields > 0 and write_affordances > 0 and config_editability > 0)

    return {
        "correctness": {
            "error_markers": error_markers,
            "empty_section_markers": empty_section,
            "broken_links": broken_links,
            "template_leaks": template_leaks,
            "htmx_unresolved": htmx_unresolved,
            "counter_consistency_ok": counter_consistency_ok,
            "bytime_ribbons": bytime_ribbons,
            "template_leaks_panel": template_leaks_panel,
            "_panel_excised": panel_excised,
            "_advertised": {
                "iterations_stat": iterations_stat,
                "runs_spawned": runs_spawned,
                "runs_completed": runs_completed,
                "thread_heading": thread_heading,
            },
            "_rendered_cards": {"page": page_cards, "runs_partial": runs_cards,
                                "total": iter_cards},
            "_ordering": {"intact_run_chain": intact_run_chain,
                          "page_descending": page_ok,
                          "runs_descending": runs_ok,
                          "bytime_labels": bytime_labels,
                          "page_iter_sample": page_iter_nums[:6] + (["..."] if len(page_iter_nums) > 6 else []),
                          "runs_iter_sample": runs_iter_nums[:6] + (["..."] if len(runs_iter_nums) > 6 else [])},
            "_runs_tail_empty": runs_tail_empty,
            "_htmx_runs_targets": htmx_runs_targets,
        },
        "relevance": {
            "header_fields_present": present,
            "header_fields_populated": populated,
            "iteration_cards": iter_cards,
            "live_count": live,
            "terminated_count": terminated,
            "didnotrun_count": didnotrun,
            "wrapup_coverage": wrapup_coverage,
            "progress_narrative": progress_narrative,
            "_verdict_text": verdict_text[:200],
            "_spark_bars": spark_bars,
            "_exit_chips": exit_chips,
        },
        "operability": {
            "read_fields": read_fields,
            "write_affordances": write_affordances,
            "config_editability": config_editability,
            "routes_wired": routes_wired,
            "operator_gating_present": operator_gating_present,
            "rwc_loop_closed": rwc_loop_closed,
            "_ctl_buttons": ctl_buttons,
            "_hx_mutations": hx_mutations,
            "_has_form": has_form,
            "_config_editable_inputs": config_editable_inputs,
            "_pre_json_blocks": pre_json_blocks,
            "_route_status": route_detail,
        },
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--captures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--target-url", required=True)
    ap.add_argument("--gardener-id", default="")
    ap.add_argument("--iteration", required=True)
    ap.add_argument("--runs-html", default="section-runs.html")
    args = ap.parse_args()

    cap = Path(args.captures)
    html_dir = cap / "html"
    desktop = cap / "screenshots" / "desktop" / "gardener-page.png"
    narrow = cap / "screenshots" / "narrow" / "gardener-page.png"
    page_html = html_dir / "gardener-page.html"
    runs_html = html_dir / args.runs_html
    routes_txt = cap / "operability" / "routes.txt"

    if not page_html.exists():
        print(f"FATAL: {page_html} missing", file=sys.stderr)
        sys.exit(2)

    sig = gardener_signals(page_html, runs_html, routes_txt)

    def _png(p):
        if not p.exists():
            print(f"WARN: screenshot {p} missing — density block null", file=sys.stderr)
            return None
        try:
            return png_metrics(p)
        except Exception as e:  # noqa: BLE001 - degrade to null, never block
            print(f"WARN: unreadable screenshot {p}: {e} — density null", file=sys.stderr)
            return None

    out = {
        "target_url": args.target_url,
        "gardener_id": args.gardener_id,
        "iteration": args.iteration,
        "density": {
            "desktop": _png(desktop),
            "narrow": _png(narrow),
            "html": html_metrics(page_html) if page_html.exists() else None,
        },
        "correctness": sig["correctness"],
        "relevance": sig["relevance"],
        "operability": sig["operability"],
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(out, indent=2, sort_keys=True))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
