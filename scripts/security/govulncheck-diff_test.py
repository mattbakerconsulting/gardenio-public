#!/usr/bin/env python3
"""govulncheck-diff_test.py — hermetic unit suite for the CI / Go-Vuln gate.

Drives the REAL govulncheck-diff.py library functions against synthetic
govulncheck -json fixtures. No network, no Go toolchain, no real modules — the
whole point of the reachability-diff being a pure function (bundle ka-0000005053
§2.3). Run: python3 scripts/security/govulncheck-diff_test.py
"""

import importlib.util
import io
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "govulncheck_diff", os.path.join(_HERE, "govulncheck-diff.py")
)
gd = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gd)

_failures = []


def check(name, cond, detail=""):
    if cond:
        print(f"  ok   {name}")
    else:
        print(f"  FAIL {name}  {detail}")
        _failures.append(name)


def eq(name, got, want):
    check(name, got == want, f"got={got!r} want={want!r}")


# ── Fixture builders ─────────────────────────────────────────────────────────


def osv_msg(oid, severity_word=None, cvss=None):
    """A govulncheck {"osv": {...}} message."""
    osv = {"id": oid}
    if severity_word is not None:
        osv["database_specific"] = {"severity": severity_word}
    if cvss is not None:
        osv["severity"] = [{"type": "CVSS_V3", "score": cvss}]
    return {"osv": osv}


def finding_msg(oid, reachable):
    """A govulncheck {"finding": {...}} message.

    reachable=True -> deepest trace frame names a function (symbol-level).
    reachable=False -> trace bottoms out at a package only (import-level).
    """
    if reachable:
        trace = [
            {"module": "example.com/app", "package": "example.com/app/main", "function": "main"},
            {"module": "example.com/dep", "package": "example.com/dep", "function": "Vulnerable"},
        ]
    else:
        trace = [{"module": "example.com/dep", "package": "example.com/dep"}]
    return {"finding": {"osv": oid, "trace": trace}}


def stream(*msgs):
    """Concatenate messages the way govulncheck emits them (whitespace-separated)."""
    return "\n".join(json.dumps(m) for m in msgs)


# ── Severity normalization ───────────────────────────────────────────────────


def test_severity_normalization():
    print("test_severity_normalization")
    eq("word critical", gd._norm_severity("CRITICAL"), "critical")
    eq("word high", gd._norm_severity("High"), "high")
    eq("word moderate->medium", gd._norm_severity("moderate"), "medium")
    eq("word low", gd._norm_severity("low"), "low")
    eq("cvss 9.8 -> critical", gd._norm_severity("9.8"), "critical")
    eq("cvss 7.5 -> high", gd._norm_severity("7.5"), "high")
    eq("cvss 5.0 -> medium", gd._norm_severity("5.0"), "medium")
    eq("cvss 2.0 -> low", gd._norm_severity("2.0"), "low")
    # Fail-loud default: unknown / empty severity is HIGH, never dropped.
    eq("none -> high", gd._norm_severity(None), "high")
    eq("empty -> high", gd._norm_severity("  "), "high")
    eq("garbage -> high", gd._norm_severity("wat"), "high")


# ── Reachability classification ──────────────────────────────────────────────


def test_reachability_classification():
    print("test_reachability_classification")
    # symbol-level finding -> reachable
    check("symbol-level reachable", gd._finding_is_reachable(finding_msg("X", True)["finding"]))
    # package-level finding -> NOT reachable
    check(
        "package-level not reachable",
        not gd._finding_is_reachable(finding_msg("X", False)["finding"]),
    )
    # malformed
    check("empty finding not reachable", not gd._finding_is_reachable({}))
    check("no-trace finding not reachable", not gd._finding_is_reachable({"osv": "X"}))


def test_parse_stream():
    print("test_parse_stream")
    text = stream(
        osv_msg("GO-2025-0001", severity_word="HIGH"),
        finding_msg("GO-2025-0001", reachable=True),
        osv_msg("GO-2025-0002", severity_word="CRITICAL"),
        finding_msg("GO-2025-0002", reachable=False),  # imported, not called
        osv_msg("GO-2025-0003", cvss="5.0"),  # medium, reachable
        finding_msg("GO-2025-0003", reachable=True),
    )
    vulns = gd.parse_govulncheck(text)
    eq("3 vulns parsed", len(vulns), 3)
    check("0001 reachable", vulns["GO-2025-0001"]["reachable"])
    check("0002 not reachable", not vulns["GO-2025-0002"]["reachable"])
    check("0003 reachable", vulns["GO-2025-0003"]["reachable"])
    eq("0001 high", gd._norm_severity(vulns["GO-2025-0001"]["severity"]), "high")
    eq("0003 medium", gd._norm_severity(vulns["GO-2025-0003"]["severity"]), "medium")


def test_parse_json_array_form():
    print("test_parse_json_array_form")
    # Some tools wrap the stream in a top-level array; we must accept it too.
    arr = [
        osv_msg("GO-2025-0009", severity_word="critical"),
        finding_msg("GO-2025-0009", reachable=True),
    ]
    vulns = gd.parse_govulncheck(json.dumps(arr))
    eq("array form parsed", len(vulns), 1)
    check("array reachable", vulns["GO-2025-0009"]["reachable"])


def test_parse_empty():
    print("test_parse_empty")
    eq("empty string", gd.parse_govulncheck(""), {})
    eq("whitespace only", gd.parse_govulncheck("   \n  "), {})


def test_reachable_gating_ids():
    print("test_reachable_gating_ids")
    text = stream(
        osv_msg("GO-A", severity_word="HIGH"),
        finding_msg("GO-A", reachable=True),  # reachable high -> gates
        osv_msg("GO-B", severity_word="CRITICAL"),
        finding_msg("GO-B", reachable=False),  # not reachable -> excluded
        osv_msg("GO-C", cvss="5.0"),
        finding_msg("GO-C", reachable=True),  # reachable medium -> excluded (below threshold)
        osv_msg("GO-D", severity_word="CRITICAL"),
        finding_msg("GO-D", reachable=True),  # reachable critical -> gates
        osv_msg("GO-E"),  # reachable, no severity -> defaults HIGH -> gates
        finding_msg("GO-E", reachable=True),
    )
    vulns = gd.parse_govulncheck(text)
    ids = gd.reachable_gating_ids(vulns)
    eq("gating ids", ids, ["GO-A", "GO-D", "GO-E"])


# ── The diff gate (the headline behavior) ────────────────────────────────────


def test_diff_clean_when_no_new():
    print("test_diff_clean_when_no_new")
    # HEAD reachable = {GO-A}; baseline already had GO-A -> no NEW -> pass.
    head = gd.parse_govulncheck(
        stream(osv_msg("GO-A", severity_word="HIGH"), finding_msg("GO-A", True))
    )
    new_ids, head_reach, pre = gd.diff_new_reachable(["GO-A"], head)
    eq("no new", new_ids, [])
    eq("preexisting tracked", pre, ["GO-A"])
    eq("head reachable", head_reach, ["GO-A"])


def test_diff_fails_on_new_reachable():
    print("test_diff_fails_on_new_reachable")
    # This MR introduced GO-NEW as reachable+critical; baseline had only GO-A.
    head = gd.parse_govulncheck(
        stream(
            osv_msg("GO-A", severity_word="HIGH"),
            finding_msg("GO-A", True),
            osv_msg("GO-NEW", severity_word="CRITICAL"),
            finding_msg("GO-NEW", True),
        )
    )
    new_ids, _, _ = gd.diff_new_reachable(["GO-A"], head)
    eq("new reachable detected", new_ids, ["GO-NEW"])


def test_diff_newly_imported_not_called_is_clean():
    print("test_diff_newly_imported_not_called_is_clean")
    # A new dependency advisory that is IMPORTED but NOT reachable must NOT gate —
    # this is the whole reachability thesis (1 of 118 reachable in the audit).
    head = gd.parse_govulncheck(
        stream(
            osv_msg("GO-IMPORTED", severity_word="CRITICAL"),
            finding_msg("GO-IMPORTED", reachable=False),
        )
    )
    new_ids, head_reach, _ = gd.diff_new_reachable([], head)
    eq("imported-not-called does not gate", new_ids, [])
    eq("imported-not-called not in reachable set", head_reach, [])


def test_diff_became_unreachable_is_clean():
    print("test_diff_became_unreachable_is_clean")
    # Baseline had GO-A reachable; this MR removed the call path -> GO-A no longer
    # reachable on HEAD. Not new, and a silent win — gate stays clean.
    head = gd.parse_govulncheck(
        stream(osv_msg("GO-A", severity_word="HIGH"), finding_msg("GO-A", reachable=False))
    )
    new_ids, head_reach, _ = gd.diff_new_reachable(["GO-A"], head)
    eq("no new after fix", new_ids, [])
    eq("no longer reachable", head_reach, [])


# ── CLI exit codes (what CI keys on) ─────────────────────────────────────────


def _run_main(argv, stdin_text=None):
    """Run gd.main capturing stdout + exit-ish return value."""
    old_out, old_in = sys.stdout, sys.stdin
    sys.stdout = io.StringIO()
    if stdin_text is not None:
        sys.stdin = io.StringIO(stdin_text)
    try:
        rc = gd.main(argv)
        return rc, sys.stdout.getvalue()
    finally:
        sys.stdout, sys.stdin = old_out, old_in


def test_cli_diff_exit_codes(tmp_report_dir):
    print("test_cli_diff_exit_codes")
    head_clean = os.path.join(tmp_report_dir, "head_clean.json")
    head_bad = os.path.join(tmp_report_dir, "head_bad.json")
    baseline = os.path.join(tmp_report_dir, "baseline.txt")
    with open(baseline, "w", encoding="utf-8") as fh:
        fh.write("GO-A\n")
    with open(head_clean, "w", encoding="utf-8") as fh:
        fh.write(stream(osv_msg("GO-A", severity_word="HIGH"), finding_msg("GO-A", True)))
    with open(head_bad, "w", encoding="utf-8") as fh:
        fh.write(
            stream(
                osv_msg("GO-A", severity_word="HIGH"),
                finding_msg("GO-A", True),
                osv_msg("GO-NEW", severity_word="CRITICAL"),
                finding_msg("GO-NEW", True),
            )
        )
    rc, _ = _run_main(["--mode", "diff", "--baseline", baseline, "--head", head_clean])
    eq("clean -> exit 0", rc, 0)
    rc, out = _run_main(["--mode", "diff", "--baseline", baseline, "--head", head_bad, "--json"])
    eq("new reachable -> exit 3", rc, 3)
    payload = json.loads(out)
    eq("json gate fail", payload["gate"], "fail")
    eq("json new list", payload["new_reachable"], ["GO-NEW"])


def test_cli_reachable_ids(tmp_report_dir):
    print("test_cli_reachable_ids")
    report = os.path.join(tmp_report_dir, "r.json")
    with open(report, "w", encoding="utf-8") as fh:
        fh.write(
            stream(
                osv_msg("GO-A", severity_word="HIGH"),
                finding_msg("GO-A", True),
                osv_msg("GO-B", severity_word="CRITICAL"),
                finding_msg("GO-B", False),
            )
        )
    rc, out = _run_main(["--mode", "reachable-ids", "--report", report])
    eq("reachable-ids exit 0", rc, 0)
    eq("reachable-ids output", out.strip().splitlines(), ["GO-A"])


def main():
    import tempfile

    test_severity_normalization()
    test_reachability_classification()
    test_parse_stream()
    test_parse_json_array_form()
    test_parse_empty()
    test_reachable_gating_ids()
    test_diff_clean_when_no_new()
    test_diff_fails_on_new_reachable()
    test_diff_newly_imported_not_called_is_clean()
    test_diff_became_unreachable_is_clean()
    with tempfile.TemporaryDirectory() as td:
        test_cli_diff_exit_codes(td)
        test_cli_reachable_ids(td)

    print()
    if _failures:
        print(f"FAILED ({len(_failures)}): {', '.join(_failures)}")
        return 1
    print("all govulncheck-diff tests passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
