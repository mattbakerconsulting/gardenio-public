#!/usr/bin/env python3
"""govulncheck-diff.py — the reachability-diff decision function for CI / Go-Vuln.

Bundle ka-0000005053 §2.3: the `CI / Go-Vuln` named status check must gate on
**NEW reachable** critical/high vulnerabilities vs the upstream-main merge-base
baseline — NOT on the raw vulnerability count. The 2026-04-19 controller-go
audit found only 1 of 118 known vulns actually reachable from the module's call
graph; reachability is the signal that separates a real exposure from advisory
noise in a transitive dependency. Gating on the raw count would red every MR the
moment any dependency picks up a fresh advisory, reachable or not — pure churn.

# What "reachable" means here

`govulncheck -json ./...` emits a stream of JSON objects. For each vulnerability
it emits an OSV record (`{"osv": {...}}`) describing the advisory, and — only
when the vulnerable symbol is actually reachable from the analyzed packages — one
or more `{"finding": {...}}` objects whose deepest `trace` frame names a
`function`. A finding whose trace bottoms out at a *package* (no `function`) is a
non-reachable, import-level match (govulncheck calls this OSV-level vs
symbol-level). We treat a vuln as REACHABLE iff it has >=1 finding with a
function-level trace frame. This mirrors `govulncheck`'s own "Vulnerability #N"
(reachable) vs "informational" (imported, not called) split.

# The diff

Reachability is computed for HEAD and for the merge-base baseline independently.
The gate fails iff a critical/high OSV id is reachable on HEAD that was NOT
reachable at the baseline — i.e. this MR *introduced or newly exposed* a serious
vuln. A vuln that was already reachable on the baseline is pre-existing debt,
tracked separately, and does not block the MR (it is reported, not gating). A
vuln that becomes UN-reachable is a silent win.

# Severity

govulncheck's OSV records do not carry a normalized severity in every case; the
`database_specific.severity` / `severity[].type=="CVSS_V3"` fields are the usual
carriers. We map to a 4-level scale (critical/high/medium/low) and default an
un-scored reachable vuln to HIGH — a reachable vuln with no severity metadata is
treated as serious, never silently dropped below the gate threshold (fail-loud).

# Modes

  --mode reachable-ids <report.json>
        Print the sorted set of reachable critical/high OSV ids in a govulncheck
        -json report, one per line. Used to snapshot the baseline and HEAD.

  --mode diff --baseline <ids-file> --head <report.json>
        Compute NEW reachable crit/high ids on HEAD not in the baseline ids file.
        Exit 0 = clean (no new reachable crit/high), exit 3 = gate FAIL (new
        reachable crit/high present), exit 2 = usage/parse error. --json for
        machine output.

Pure/hermetic: parsing + classification + diff are library functions the test
(govulncheck-diff_test.py) drives directly against synthetic govulncheck JSON —
no network, no Go toolchain, no real modules. The bash orchestrator
(govulncheck-baseline.sh) runs the real `govulncheck` at both revisions and feeds
its output here.
"""

import argparse
import json
import sys

# Severity ordering: only critical/high gate. medium/low are reported but never
# block (matching the §2.3 "critical/high" threshold and the audit's finding
# that the vast majority of advisories are low-relevance noise).
GATING_SEVERITIES = ("critical", "high")


def _norm_severity(raw):
    """Normalize a raw severity token to one of critical/high/medium/low.

    Accepts CVSS numeric strings, CVSS vectors, and word tokens. A reachable
    vuln with NO parseable severity defaults to 'high' — fail-loud, never
    silently below the gate threshold.
    """
    if raw is None:
        return "high"
    s = str(raw).strip().lower()
    if not s:
        return "high"
    # Word tokens (GHSA/database_specific style).
    if s in ("critical", "crit"):
        return "critical"
    if s in ("high",):
        return "high"
    if s in ("medium", "moderate", "med"):
        return "medium"
    if s in ("low",):
        return "low"
    # A CVSS v3 vector string ("CVSS:3.1/AV:N/...") — derive from the base score
    # if present, else be conservative (high).
    # A bare numeric CVSS base score.
    try:
        score = float(s)
    except ValueError:
        score = None
    if score is not None:
        # CVSS v3 qualitative bands.
        if score >= 9.0:
            return "critical"
        if score >= 7.0:
            return "high"
        if score >= 4.0:
            return "medium"
        return "low"
    # Unknown non-numeric token — conservative default.
    return "high"


def osv_severity(osv):
    """Best-effort severity of a govulncheck OSV record -> critical/high/medium/low.

    Checks, in order: database_specific.severity (word), the OSV `severity` array
    (CVSS_V3 score), then defaults to 'high' for a reachable but un-scored vuln.
    """
    if not isinstance(osv, dict):
        return "high"
    # 1. database_specific.severity (often a word: "HIGH", "CRITICAL").
    dbs = osv.get("database_specific")
    if isinstance(dbs, dict):
        word = dbs.get("severity")
        if word:
            return _norm_severity(word)
    # 2. OSV severity array (CVSS vector / score).
    for sev in osv.get("severity", []) or []:
        if not isinstance(sev, dict):
            continue
        val = sev.get("score")
        if val is None:
            continue
        # A CVSS vector: extract the base score if embedded, else fall through.
        text = str(val)
        # Some feeds put a bare number in `score`; _norm_severity handles both.
        norm = _norm_severity(text)
        if norm:
            return norm
    # 3. Un-scored reachable vuln — conservative HIGH.
    return "high"


def _finding_is_reachable(finding):
    """True iff a govulncheck finding's trace reaches a FUNCTION (symbol-level).

    A finding whose deepest trace frame names only a `package`/`module` (no
    `function`) is an import-level, non-reachable match. Symbol-level reachability
    requires >=1 trace frame with a non-empty `function`.
    """
    if not isinstance(finding, dict):
        return False
    trace = finding.get("trace")
    if not isinstance(trace, list):
        return False
    for frame in trace:
        if isinstance(frame, dict) and frame.get("function"):
            return True
    return False


def parse_govulncheck(stream_text):
    """Parse a govulncheck -json stream into {osv_id: {"severity","reachable"}}.

    The stream is a concatenation of JSON objects (govulncheck emits one object
    per message, NOT a JSON array). We tolerate both whitespace-separated objects
    and a top-level array. Every OSV id seen is recorded; `reachable` is True iff
    any finding for that id has a function-level trace.
    """
    vulns = {}  # osv_id -> {"severity": str, "reachable": bool}

    def _record_osv(osv):
        if not isinstance(osv, dict):
            return
        oid = osv.get("id")
        if not oid:
            return
        entry = vulns.setdefault(oid, {"severity": None, "reachable": False})
        entry["severity"] = osv_severity(osv)

    def _record_finding(finding):
        if not isinstance(finding, dict):
            return
        oid = finding.get("osv")
        if not oid:
            return
        entry = vulns.setdefault(oid, {"severity": None, "reachable": False})
        if _finding_is_reachable(finding):
            entry["reachable"] = True

    def _dispatch(obj):
        if not isinstance(obj, dict):
            return
        if "osv" in obj and isinstance(obj["osv"], dict):
            _record_osv(obj["osv"])
        if "finding" in obj and isinstance(obj["finding"], dict):
            _record_finding(obj["finding"])

    text = stream_text.strip()
    if not text:
        return vulns
    # Try a single JSON value first (array or object).
    try:
        loaded = json.loads(text)
        if isinstance(loaded, list):
            for obj in loaded:
                _dispatch(obj)
        else:
            _dispatch(loaded)
        return vulns
    except json.JSONDecodeError:
        pass
    # Fall back to a streaming decoder for whitespace-separated objects.
    decoder = json.JSONDecoder()
    idx = 0
    n = len(text)
    while idx < n:
        while idx < n and text[idx].isspace():
            idx += 1
        if idx >= n:
            break
        obj, end = decoder.raw_decode(text, idx)
        _dispatch(obj)
        idx = end
    return vulns


def reachable_gating_ids(vulns):
    """Sorted set of OSV ids that are reachable AND critical/high."""
    ids = [
        oid
        for oid, v in vulns.items()
        if v.get("reachable") and _norm_severity(v.get("severity")) in GATING_SEVERITIES
    ]
    return sorted(set(ids))


def diff_new_reachable(baseline_ids, head_vulns):
    """NEW reachable crit/high on HEAD not in the baseline id set.

    Returns (new_ids, head_reachable_ids, preexisting_ids), all sorted.
    """
    baseline = set(baseline_ids)
    head_reachable = reachable_gating_ids(head_vulns)
    new_ids = sorted(set(head_reachable) - baseline)
    preexisting = sorted(set(head_reachable) & baseline)
    return new_ids, head_reachable, preexisting


def _read(path):
    if path == "-":
        return sys.stdin.read()
    with open(path, "r", encoding="utf-8") as fh:
        return fh.read()


def _cmd_reachable_ids(args):
    vulns = parse_govulncheck(_read(args.report))
    for oid in reachable_gating_ids(vulns):
        print(oid)
    return 0


def _cmd_diff(args):
    head_vulns = parse_govulncheck(_read(args.head))
    baseline_ids = []
    if args.baseline:
        for line in _read(args.baseline).splitlines():
            line = line.strip()
            if line and not line.startswith("#"):
                baseline_ids.append(line)
    new_ids, head_reachable, preexisting = diff_new_reachable(baseline_ids, head_vulns)
    if args.json:
        json.dump(
            {
                "new_reachable": new_ids,
                "head_reachable": head_reachable,
                "preexisting_reachable": preexisting,
                "gate": "fail" if new_ids else "pass",
            },
            sys.stdout,
            indent=2,
            sort_keys=True,
        )
        sys.stdout.write("\n")
    else:
        if new_ids:
            print("FAIL: NEW reachable critical/high vulnerabilities vs baseline:")
            for oid in new_ids:
                print(f"  + {oid}")
        else:
            print("PASS: no NEW reachable critical/high vulnerabilities vs baseline.")
        if preexisting:
            print(f"pre-existing reachable crit/high (tracked, non-gating): {len(preexisting)}")
            for oid in preexisting:
                print(f"  = {oid}")
    return 3 if new_ids else 0


def main(argv=None):
    p = argparse.ArgumentParser(description="govulncheck reachability-diff gate")
    p.add_argument(
        "--mode",
        required=True,
        choices=("reachable-ids", "diff"),
        help="reachable-ids: emit reachable crit/high ids; diff: gate NEW vs baseline",
    )
    p.add_argument("--report", help="govulncheck -json report path (reachable-ids mode; '-' = stdin)")
    p.add_argument("--head", help="HEAD govulncheck -json report path (diff mode; '-' = stdin)")
    p.add_argument("--baseline", help="baseline reachable-ids file (diff mode)")
    p.add_argument("--json", action="store_true", help="machine-readable output")
    args = p.parse_args(argv)

    if args.mode == "reachable-ids":
        if not args.report:
            p.error("--report is required in reachable-ids mode")
        return _cmd_reachable_ids(args)
    if args.mode == "diff":
        if not args.head:
            p.error("--head is required in diff mode")
        return _cmd_diff(args)
    return 2


if __name__ == "__main__":
    sys.exit(main())
