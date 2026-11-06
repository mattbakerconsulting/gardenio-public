#!/usr/bin/env python3
"""Canonical metric-staleness probe for the Grafana GRADE cross-check harness.

Bug ka-0000001289: the STALE detector in the 08-31 / 09-01 / 09-02 GRADE
harnesses measured a metric's last-sample age with

    time() - max(timestamp(last_over_time(<metric>[30d])))

which is **structurally inert** in VictoriaMetrics: `timestamp()` applied over a
rollup returns the EVALUATION-STEP timestamp, not the sample's timestamp, so the
expression evaluates to ~0 for every metric — including one that has been silent
for weeks.  Measured 2026-09-02 against three controls:

    gardenio_agents_active            live, scraping now  -> 0.03h
    gardenio_jira_scrape_errors_total silent 13 days      -> 0.03h
    gardenio_agents_deferred_total    sporadic, ~8-25min  -> 0.03h

Identical readings on working and broken inputs: the STALE branch could never
fire, so the "0 STALE" line both the 08-31 and 09-01 reports carried was
*unmeasured*, not verified.

This module is the durable replacement.  It has two hats:

  library  imported by each iteration's docs/grafana-grade/<iter>/crosscheck-
           harness.py.  `StalenessProbe.audit()` measures ages with VM's
           `lag()` rollup AND corroborates every reading against an independent
           second instrument (`count_over_time` presence brackets), refusing to
           return a clean bill of health when the two disagree.

  lint     `python3 scripts/grade_staleness_probe.py --lint` scans the committed
           harnesses for the inert form so it cannot be reintroduced.  Wired
           into `make lint`; its _test.py runs in checks.yml script-selftests.

Why corroboration rather than a bare bug-fix: the inert probe was invisible
precisely because nothing ever cross-checked it (advice ka-0000002586 — verify
with a field that increments on success, and test any candidate field against a
KNOWN-GOOD control first; advice ka-0000005924 — derive every important quantity
two independent ways so the two must agree).  `lag()` is the fix; the audit is
what makes the NEXT inert detector fail loudly instead of reporting zero.

Independent second derivation
-----------------------------
For a metric whose true last sample is `age` seconds old, `count_over_time(m[W])`
is non-empty iff `age <= W`.  Walking a ladder of windows therefore brackets the
age without using `lag()` at all:

    smallest W with samples      -> age <= W          (upper bound)
    largest  W with no samples   -> age >= W          (lower bound)

A reported age outside its own bracket is a CONTRADICTION: the probe is lying.
Replayed against the historical failure, the inert form reports 0.03h for
`gardenio_jira_scrape_errors_total` while the metric is absent from every window
out to 24h -> lower bound 24h > 0.03h -> the audit raises ProbeInertError instead
of printing "0 STALE".

Usage (harness):

    from grade_staleness_probe import StalenessProbe, ProbeInertError

    probe = StalenessProbe(vm_query)                 # vm_query(expr) -> VM JSON
    result = probe.audit(metrics, stale_secs=6*3600) # raises if inert
    ages = result["ages"]                            # metric -> seconds | None

Usage (lint / live spot-check):

    python3 scripts/grade_staleness_probe.py --lint
    python3 scripts/grade_staleness_probe.py --vm http://gardenio-vmsingle:8428 \
        --probe gardenio_agents_active gardenio_jira_scrape_errors_total
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.parse
import urllib.request

# --------------------------------------------------------------------------
# The probe expression
# --------------------------------------------------------------------------

#: MetricsQL rollup that returns, per series, the real gap in seconds between
#: the evaluation time and that series' last sample.  `min()` across series is
#: the freshest sample of the metric family, which is what a panel renders.
AGE_EXPR = "min(lag({metric}[{lookback}]))"

#: Independent presence instrument used to bracket the age (see module docstring).
PRESENCE_EXPR = "max(count_over_time({metric}[{window}]))"

#: Default bracket ladder, seconds.  Coarse on purpose: four cheap instant
#: queries per corroborated metric, enough to catch an order-of-magnitude lie.
DEFAULT_BRACKETS = (600, 3600, 6 * 3600, 24 * 3600)

#: A reported age may exceed its upper bracket by this much before it counts as
#: a contradiction — `count_over_time(m[W])` counts samples in the HALF-OPEN
#: window and the two queries do not evaluate at the identical instant.
BRACKET_TOLERANCE_SECS = 120.0

#: A corpus of at least this many measured metrics whose ages all collapse into
#: DEGENERATE_SPREAD_SECS is the inert-probe signature (the broken form spread
#: 0.47s across three metrics that truly differed by 316 hours).  It is reported
#: as a WARNING, not an error: a small corpus whose metrics all come from one
#: scrape target legitimately shares a sample timestamp, so collapse alone is
#: suggestive rather than conclusive.  The bracket contradiction above is the
#: conclusive test.
MIN_CORPUS_FOR_SPREAD = 5
DEGENERATE_SPREAD_SECS = 5.0


class ProbeInertError(RuntimeError):
    """The staleness detector cannot be trusted — do NOT report its zero-count.

    Raised when a measured age contradicts the independent presence bracket, or
    when a whole corpus of ages collapses onto one value (the signature of a
    probe reading something other than the sample timestamp).
    """


class StalenessProbe:
    """Measure per-metric last-sample age, and prove the measurement is real."""

    def __init__(self, query, lookback="30d", brackets=DEFAULT_BRACKETS):
        """`query(expr)` must return the parsed VM /api/v1/query JSON body."""
        self.query = query
        self.lookback = lookback
        self.brackets = tuple(sorted(brackets))
        self._age_cache = {}

    # -- primitives --------------------------------------------------------

    def _scalar(self, expr):
        r = self.query(expr)
        if not isinstance(r, dict) or r.get("status") != "success":
            return None
        result = r.get("data", {}).get("result") or []
        if not result:
            return None
        try:
            return float(result[0]["value"][1])
        except (TypeError, ValueError, KeyError, IndexError):
            return None

    def age(self, metric):
        """Seconds since `metric`'s most recent sample, or None if unseen."""
        if metric not in self._age_cache:
            self._age_cache[metric] = self._scalar(
                AGE_EXPR.format(metric=metric, lookback=self.lookback)
            )
        return self._age_cache[metric]

    def bracket(self, metric):
        """(lower, upper) bounds on the age, derived WITHOUT the age probe.

        `upper` is the smallest window that contains samples (None if the metric
        is absent from every window); `lower` is the largest window that does
        not (None if the metric is present in the smallest window).
        """
        lower = None
        upper = None
        for window in self.brackets:
            n = self._scalar(PRESENCE_EXPR.format(metric=metric, window=_dur(window)))
            if n:
                upper = float(window)
                break
            lower = float(window)
        return lower, upper

    # -- the guard ---------------------------------------------------------

    def corroborate(self, metric, age):
        """Check one reported age against its independent presence bracket."""
        lower, upper = self.bracket(metric)
        row = {"metric": metric, "age": age, "bracket_lower": lower, "bracket_upper": upper}
        if age is None:
            # Never seen in `lookback` — corroborated only if it is absent from
            # every bracket window too.
            row["verdict"] = "ok" if upper is None else "contradiction"
            if upper is not None:
                row["detail"] = (
                    f"probe reports {metric} unseen in {self.lookback}, but it has "
                    f"samples within {_dur(int(upper))}"
                )
            return row
        if lower is not None and age < lower:
            row["verdict"] = "contradiction"
            row["detail"] = (
                f"probe reports {metric} last sampled {age / 3600:.2f}h ago, but the "
                f"metric has NO samples in the last {_dur(int(lower))} — the probe is "
                f"not reading the sample timestamp (see ka-0000001289)"
            )
            return row
        if upper is not None and age > upper + BRACKET_TOLERANCE_SECS:
            row["verdict"] = "contradiction"
            row["detail"] = (
                f"probe reports {metric} last sampled {age / 3600:.2f}h ago, but the "
                f"metric HAS samples within {_dur(int(upper))}"
            )
            return row
        row["verdict"] = "ok"
        return row

    def audit(self, metrics, stale_secs, corroborate_max=8):
        """Measure every metric's age and positive-control the detector.

        Returns a dict with `ages`, the reachability `ladder`, the `controls`
        that were corroborated, and `stale` (the metrics past `stale_secs`).
        Raises ProbeInertError if the detector cannot be trusted — callers must
        NOT downgrade that into a zero-count.
        """
        ages = {m: self.age(m) for m in metrics}
        measured = {m: a for m, a in ages.items() if a is not None}

        # Control selection: the extremes are where an inert probe is caught —
        # the freshest reading must really be fresh and the stalest must really
        # be stale.  Corroborating a handful bounds the query cost.
        ordered = sorted(measured, key=lambda m: measured[m])
        picks = ordered[: max(1, corroborate_max // 2)] + ordered[-max(1, corroborate_max // 2):]
        unseen = [m for m, a in ages.items() if a is None]
        controls = []
        seen = set()
        for m in picks + unseen[:2]:
            if m in seen:
                continue
            seen.add(m)
            controls.append(self.corroborate(m, ages[m]))

        contradictions = [c for c in controls if c["verdict"] == "contradiction"]
        if contradictions:
            raise ProbeInertError(
                "staleness probe failed its positive control — "
                + "; ".join(c["detail"] for c in contradictions)
            )

        spread = (max(measured.values()) - min(measured.values())) if measured else 0.0
        warnings = []
        if len(measured) >= MIN_CORPUS_FOR_SPREAD and spread < DEGENERATE_SPREAD_SECS:
            warnings.append(
                f"degenerate spread — {len(measured)} metrics all report an age within "
                f"{spread:.2f}s of each other, the inert-probe signature (ka-0000001289); "
                "legitimate only if every metric shares one scrape target"
            )

        stale = sorted(m for m, a in measured.items() if a > stale_secs)
        result = {
            "ages": ages,
            "stale": stale,
            "stale_secs": stale_secs,
            "ladder": ladder(measured),
            "controls": controls,
            "spread_secs": spread,
            "measured": len(measured),
            "unseen": sorted(unseen),
            "warnings": warnings,
        }
        result["positive_control"] = _control_note(result)
        return result


def ladder(ages, thresholds=(6 * 3600, 2 * 3600, 3600)):
    """Reachability evidence: how many metrics trip the STALE branch as the
    threshold drops.  A report claiming "0 STALE" must publish this — it is what
    shows the branch is reachable at all rather than structurally dead."""
    measured = [a for a in ages.values() if a is not None] if isinstance(ages, dict) else list(ages)
    return {_dur(t): sum(1 for a in measured if a > t) for t in sorted(thresholds, reverse=True)}


def _control_note(result):
    """One line a report can quote instead of an unbacked "0 STALE"."""
    corroborated = len([c for c in result["controls"] if c["verdict"] == "ok"])
    hits = [n for n in result["ladder"].values() if n]
    if result.get("warnings"):
        return "UNPROVEN — " + "; ".join(result["warnings"])
    if not hits:
        return (
            f"{corroborated} controls corroborated against count_over_time brackets; "
            "NO metric in this corpus is stale at any ladder threshold, so the STALE "
            "branch was not exercised by data this iteration — report the zero as "
            "'no stale metric found', never as 'detector verified'"
        )
    return (
        f"{corroborated} controls corroborated against count_over_time brackets; "
        f"STALE branch demonstrably reachable (ladder {result['ladder']})"
    )


def _dur(seconds):
    seconds = int(seconds)
    if seconds % 86400 == 0:
        return f"{seconds // 86400}d"
    if seconds % 3600 == 0:
        return f"{seconds // 3600}h"
    if seconds % 60 == 0:
        return f"{seconds // 60}m"
    return f"{seconds}s"


# --------------------------------------------------------------------------
# Lint: the inert form must never come back
# --------------------------------------------------------------------------

#: `timestamp(...)` wrapped around ANY rollup/subquery — the whole family reads
#: the eval-step timestamp in VictoriaMetrics, not the sample's.
BANNED_RE = re.compile(
    r"timestamp\s*\(\s*(?:last_over_time|max_over_time|min_over_time|avg_over_time|"
    r"present_over_time|last)\s*\(",
    re.I,
)

#: Default lint scope: every committed GRADE harness plus this module itself.
#: `*_test.py` is excluded — the test suite carries the inert form as a fixture
#: on purpose (it is what proves the lint fires).
LINT_GLOBS = ("docs/grafana-grade/*/*.py", "scripts/grade_staleness_probe.py")


def scan_text(text):
    """Return [(lineno, line)] for banned-form uses in executable (non-comment,
    non-docstring-prose) lines.  A comment that names the form to warn about it
    — as the 09-02 harness does — is deliberately allowed."""
    findings = []
    in_doc = None
    for i, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        # Strip triple-quoted regions so module/function prose explaining the
        # bug — like the NB comment the 09-02 harness carries — does not trip.
        code_parts = []
        rest = line
        while rest:
            if in_doc:
                idx = rest.find(in_doc)
                if idx < 0:
                    rest = ""
                else:
                    rest = rest[idx + 3:]
                    in_doc = None
                continue
            m = re.search(r'"""|\'\'\'', rest)
            if not m:
                code_parts.append(rest)
                break
            code_parts.append(rest[: m.start()])
            in_doc = m.group(0)
            rest = rest[m.end():]
        code = "".join(code_parts).split("#", 1)[0]
        if BANNED_RE.search(code):
            findings.append((i, line))
    return findings


def lint_paths(paths):
    findings = []
    for path in paths:
        try:
            with open(path, "r", encoding="utf-8", errors="replace") as fh:
                text = fh.read()
        except OSError as exc:
            findings.append((path, 0, f"unreadable: {exc}"))
            continue
        for lineno, line in scan_text(text):
            findings.append((path, lineno, line))
    return findings


def _default_lint_paths(repo_root):
    import glob

    out = []
    for pattern in LINT_GLOBS:
        out.extend(sorted(glob.glob(os.path.join(repo_root, pattern))))
    return out


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def _http_query(vm_url):
    def query(expr):
        url = vm_url.rstrip("/") + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
        try:
            with urllib.request.urlopen(url, timeout=30) as resp:
                return json.load(resp)
        except (OSError, ValueError) as exc:
            # OSError covers URLError/HTTPError/socket timeouts, ValueError a
            # malformed body: a VM hiccup must surface as a failed query the
            # probe can report, never as a crash mid-audit.
            return {"status": "error", "error": str(exc)}

    return query


def main(argv=None):
    repo_root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--lint", action="store_true", help="scan committed harnesses for the inert form")
    ap.add_argument("--paths", nargs="*", default=None, help="explicit files to lint")
    ap.add_argument("--vm", default=os.environ.get("VM_URL", "http://gardenio-vmsingle:8428"))
    ap.add_argument("--probe", nargs="*", default=None, help="metrics to measure live")
    ap.add_argument("--stale-hours", type=float, default=6.0)
    args = ap.parse_args(argv)

    if args.lint or args.probe is None:
        paths = args.paths if args.paths is not None else _default_lint_paths(repo_root)
        findings = lint_paths(paths)
        if findings:
            print("grade_staleness_probe: BANNED staleness form found — it reads the", file=sys.stderr)
            print("evaluation-step timestamp, not the sample's, so it reports ~0 for every", file=sys.stderr)
            print("metric and the STALE branch can never fire (bug ka-0000001289).", file=sys.stderr)
            print("Use StalenessProbe / min(lag(<metric>[30d])) instead.\n", file=sys.stderr)
            for path, lineno, line in findings:
                print(f"  {os.path.relpath(path, repo_root)}:{lineno}: {line}", file=sys.stderr)
            return 1
        print(f"grade_staleness_probe: {len(paths)} harness file(s) clean — no inert staleness probe")
        return 0

    probe = StalenessProbe(_http_query(args.vm))
    try:
        result = probe.audit(args.probe, stale_secs=args.stale_hours * 3600)
    except ProbeInertError as exc:
        print(f"PROBE INERT: {exc}", file=sys.stderr)
        return 2
    for metric, age in sorted(result["ages"].items(), key=lambda kv: (kv[1] is None, kv[1])):
        print(f"  {metric:50s} {'unseen' if age is None else f'{age / 3600:9.3f}h'}")
    print(f"ladder: {result['ladder']}")
    print(f"stale (> {args.stale_hours}h): {result['stale'] or 'none'}")
    print(f"positive control: {result['positive_control']}")
    for w in result["warnings"]:
        print(f"WARNING: {w}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
