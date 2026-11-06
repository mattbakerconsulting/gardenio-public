#!/usr/bin/env python3
"""Hermetic tests for scripts/grade_staleness_probe.py (bug ka-0000001289).

No network, no VictoriaMetrics, no `kd` — a FakeVM implements the two MetricsQL
behaviours the probe depends on:

  lag(m[W])                real seconds since the metric's last sample
  count_over_time(m[W])    non-empty iff that last sample is within W

and a second mode replays the HISTORICAL FAILURE: the inert
`time() - max(timestamp(last_over_time(m[30d])))` form, which in VictoriaMetrics
returns the evaluation-step timestamp and therefore reads ~0.06h for every
metric — live, sporadic, or silent for 13 days alike.  The corpus below is the
real one measured on 2026-09-02 (see the bug), so case 2 proves the guard would
have turned the 08-31 / 09-01 "0 STALE" claim red instead of publishing it.

Usage:  python3 scripts/grade_staleness_probe_test.py
"""
import os
import sys
import importlib.util

HERE = os.path.dirname(os.path.abspath(__file__))
REPO_ROOT = os.path.dirname(HERE)

_spec = importlib.util.spec_from_file_location(
    "grade_staleness_probe", os.path.join(HERE, "grade_staleness_probe.py")
)
probe_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(probe_mod)

StalenessProbe = probe_mod.StalenessProbe
ProbeInertError = probe_mod.ProbeInertError

PASS = 0
FAIL = 0


def ok(name):
    global PASS
    PASS += 1
    print(f"PASS: {name}")


def bad(name, detail):
    global FAIL
    FAIL += 1
    print(f"FAIL: {name}: {detail}")


def check(name, cond, detail=""):
    ok(name) if cond else bad(name, detail)


# --------------------------------------------------------------------------
# Fake VictoriaMetrics
# --------------------------------------------------------------------------

HOUR = 3600.0

# The three controls from the bug report, plus the stalest live metric behind a
# populated panel (gardenio_node_lifecycle_events_total, 5.90h on 09-02).
CORPUS = {
    "gardenio_agents_active": 0.03 * HOUR,            # live, scraping now
    "gardenio_jira_scrape_errors_total": 316.45 * HOUR,  # silent 13 days
    "gardenio_agents_deferred_total": 0.19 * HOUR,    # sporadic
    "gardenio_node_lifecycle_events_total": 5.90 * HOUR,
    "gardenio_beads_total": 0.05 * HOUR,
    "kbeads_requests_total": 0.02 * HOUR,
    "gardenio_dark_metric": None,                     # never seen at all
}

#: What the inert form actually returned on 2026-09-02 — the same ~0.06h for
#: every metric in the corpus, which is why the STALE branch could never fire.
INERT_AGE_SECS = 220.0


class FakeVM:
    def __init__(self, corpus, mode="lag"):
        self.corpus = corpus
        self.mode = mode
        self.calls = []

    def __call__(self, expr):
        self.calls.append(expr)
        if "lag(" in expr:
            metric = self._metric(expr, "lag(")
            if self.mode == "inert":
                # Model the broken probe: eval-step timestamp, identical for all.
                if self.corpus.get(metric, "missing") == "missing":
                    return _empty()
                jitter = (len(metric) % 7) * 0.01
                return _scalar(INERT_AGE_SECS + jitter)
            age = self.corpus.get(metric)
            return _empty() if age is None else _scalar(age)
        if "count_over_time(" in expr:
            metric = self._metric(expr, "count_over_time(")
            window = _parse_window(expr)
            age = self.corpus.get(metric)
            if age is None or age > window:
                return _empty()
            return _scalar(42.0)
        raise AssertionError(f"FakeVM got an unexpected expression: {expr}")

    @staticmethod
    def _metric(expr, func):
        tail = expr.split(func, 1)[1]
        return tail.split("[", 1)[0].strip()


def _scalar(v):
    return {"status": "success", "data": {"resultType": "vector", "result": [{"metric": {}, "value": [0, str(v)]}]}}


def _empty():
    return {"status": "success", "data": {"resultType": "vector", "result": []}}


def _parse_window(expr):
    raw = expr.split("[", 1)[1].split("]", 1)[0]
    unit = raw[-1]
    n = float(raw[:-1])
    return n * {"s": 1, "m": 60, "h": 3600, "d": 86400}[unit]


# --------------------------------------------------------------------------
# 1. The fixed probe separates live from long-silent
# --------------------------------------------------------------------------

vm = FakeVM(CORPUS)
p = StalenessProbe(vm)
res = p.audit(list(CORPUS), stale_secs=6 * HOUR)

check(
    "lag probe reads the true age of a live metric",
    abs(res["ages"]["gardenio_agents_active"] - 0.03 * HOUR) < 1,
    res["ages"]["gardenio_agents_active"],
)
check(
    "lag probe reads the true age of a 13-day-silent metric",
    abs(res["ages"]["gardenio_jira_scrape_errors_total"] - 316.45 * HOUR) < 1,
    res["ages"]["gardenio_jira_scrape_errors_total"],
)
check(
    "STALE branch fires on the silent metric and only on it",
    res["stale"] == ["gardenio_jira_scrape_errors_total"],
    res["stale"],
)
check(
    "a metric absent from the whole lookback is reported unseen, not fresh",
    res["ages"]["gardenio_dark_metric"] is None and res["unseen"] == ["gardenio_dark_metric"],
    (res["ages"]["gardenio_dark_metric"], res["unseen"]),
)
check(
    "ladder publishes reachability evidence at descending thresholds",
    res["ladder"]["6h"] == 1 and res["ladder"]["1h"] == 2,
    res["ladder"],
)
check(
    "positive control reports the branch as demonstrably reachable",
    "reachable" in res["positive_control"] and not res["warnings"],
    res["positive_control"],
)

# --------------------------------------------------------------------------
# 2. HISTORICAL REPLAY: the inert form is caught, not published as "0 STALE"
# --------------------------------------------------------------------------

vm_broken = FakeVM(CORPUS, mode="inert")
p_broken = StalenessProbe(vm_broken)

# Sanity: the inert probe really does read ~0 on the 13-day-silent control —
# this is the reading that made "0 STALE" look clean on 08-31 and 09-01.
inert_age = p_broken.age("gardenio_jira_scrape_errors_total")
check(
    "inert form reads ~0.06h on a metric silent for 13 days (the bug)",
    inert_age is not None and inert_age < 300,
    inert_age,
)

try:
    p_broken.audit(list(CORPUS), stale_secs=6 * HOUR)
    bad("inert probe raises ProbeInertError instead of reporting 0 STALE", "audit() returned normally")
except ProbeInertError as exc:
    msg = str(exc)
    check(
        "inert probe raises ProbeInertError instead of reporting 0 STALE",
        "gardenio_jira_scrape_errors_total" in msg and "ka-0000001289" in msg,
        msg,
    )

# --------------------------------------------------------------------------
# 3. The bracket instrument is genuinely independent of lag()
# --------------------------------------------------------------------------

p2 = StalenessProbe(FakeVM(CORPUS))
lower, upper = p2.bracket("gardenio_node_lifecycle_events_total")   # true age 5.90h
check(
    "presence bracket brackets a 5.9h-old metric between 1h and 6h",
    lower == 3600.0 and upper == 6 * HOUR,
    (lower, upper),
)
lower, upper = p2.bracket("gardenio_jira_scrape_errors_total")      # true age 316h
check(
    "presence bracket finds no upper bound for a 13-day-silent metric",
    lower == 24 * HOUR and upper is None,
    (lower, upper),
)
check(
    "bracket queries never call lag() — the second derivation is independent",
    all("lag(" not in c for c in p2.query.calls),
    [c for c in p2.query.calls if "lag(" in c],
)

# --------------------------------------------------------------------------
# 4. A collapsed all-fresh corpus is flagged UNPROVEN, not failed
# --------------------------------------------------------------------------

flat = {f"metric_{i}": 1.0 + i * 0.2 for i in range(6)}   # one scrape target
res_flat = StalenessProbe(FakeVM(flat)).audit(list(flat), stale_secs=6 * HOUR)
check(
    "homogeneous fresh corpus is warned about but not failed",
    res_flat["warnings"] and res_flat["positive_control"].startswith("UNPROVEN"),
    (res_flat["warnings"], res_flat["positive_control"]),
)

# A corpus with no stale metric at all must never claim the detector was proven.
fresh = {"a": 60.0, "b": 900.0}
res_fresh = StalenessProbe(FakeVM(fresh)).audit(list(fresh), stale_secs=6 * HOUR)
check(
    "an all-fresh corpus reports 'no stale metric found', never 'detector verified'",
    res_fresh["stale"] == [] and "not exercised by data" in res_fresh["positive_control"],
    res_fresh["positive_control"],
)

# --------------------------------------------------------------------------
# 5. The lint catches the inert form and tolerates prose about it
# --------------------------------------------------------------------------

banned_code = 'r = vm_query(f"time() - max(timestamp(last_over_time({m}[30d])))")\n'
check("lint flags the inert form in code", len(probe_mod.scan_text(banned_code)) == 1, probe_mod.scan_text(banned_code))

comment_only = (
    "# NB: `time() - timestamp(last_over_time(m[30d]))` is NOT usable here.\n"
    'r = vm_query(f"min(lag({m}[30d]))")\n'
)
check("lint tolerates a comment warning about the inert form", probe_mod.scan_text(comment_only) == [], probe_mod.scan_text(comment_only))

docstring_prose = '"""Do not use timestamp(last_over_time(m[30d])) — it reads the eval step."""\nx = 1\n'
check("lint tolerates docstring prose about the inert form", probe_mod.scan_text(docstring_prose) == [], probe_mod.scan_text(docstring_prose))

variants = [
    'q(f"timestamp(max_over_time({m}[1h]))")\n',
    'q("timestamp( last_over_time(up[5m]) )")\n',
]
check(
    "lint flags the whole timestamp(<rollup>) family, not just last_over_time",
    all(probe_mod.scan_text(v) for v in variants),
    [probe_mod.scan_text(v) for v in variants],
)

# 6. The committed harness corpus is clean today.
paths = probe_mod._default_lint_paths(REPO_ROOT)
check("repo has GRADE harnesses to lint", len(paths) > 0, paths)
findings = probe_mod.lint_paths(paths)
check("every committed GRADE harness is free of the inert form", findings == [], findings)

print(f"\n{PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
