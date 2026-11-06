#!/usr/bin/env python3
"""osv-diff_test.py — hermetic unit suite for the CI / OSV-Scan diff gate.

Drives the REAL osv-diff.py library functions against synthetic osv-scanner v2
JSON fixtures. No network, no osv-scanner binary. Run:
    python3 scripts/security/osv-diff_test.py
"""

import importlib.util
import io
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location("osv_diff", os.path.join(_HERE, "osv-diff.py"))
od = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(od)

_failures = []


def check(name, cond, detail=""):
    if cond:
        print(f"  ok   {name}")
    else:
        print(f"  FAIL {name}  {detail}")
        _failures.append(name)


def eq(name, got, want):
    check(name, got == want, f"got={got!r} want={want!r}")


def report(*vuln_ids, path="coop/tests/e2e/bun.lock"):
    """Build an osv-scanner v2 JSON report naming the given vuln ids."""
    return json.dumps(
        {
            "results": [
                {
                    "source": {"path": path, "type": "lockfile"},
                    "packages": [
                        {
                            "package": {"name": "pkg", "version": "1.0.0", "ecosystem": "npm"},
                            "groups": [{"ids": list(vuln_ids), "max_severity": "7.1"}],
                            "vulnerabilities": [{"id": vid, "aliases": []} for vid in vuln_ids],
                        }
                    ],
                }
            ]
        }
    )


def test_parse():
    print("test_parse")
    ids = od.parse_osv_report(report("GHSA-aaaa", "GHSA-bbbb"))
    eq("parsed ids", ids, {"GHSA-aaaa", "GHSA-bbbb"})


def test_parse_empty():
    print("test_parse_empty")
    eq("empty string", od.parse_osv_report(""), set())
    eq("whitespace", od.parse_osv_report("  \n "), set())
    eq("no results key", od.parse_osv_report(json.dumps({"experimental_config": {}})), set())


def test_parse_garbage():
    print("test_parse_garbage")
    eq("garbage -> empty", od.parse_osv_report("not json {["), set())


def test_diff_clean():
    print("test_diff_clean")
    new, head, pre = od.diff_new(["GHSA-aaaa"], od.parse_osv_report(report("GHSA-aaaa")))
    eq("no new", new, [])
    eq("preexisting tracked", pre, ["GHSA-aaaa"])


def test_diff_new_detected():
    print("test_diff_new_detected")
    head = od.parse_osv_report(report("GHSA-aaaa", "GHSA-NEW"))
    new, _, _ = od.diff_new(["GHSA-aaaa"], head)
    eq("new detected", new, ["GHSA-NEW"])


def test_diff_moved_lockfile_not_new():
    print("test_diff_moved_lockfile_not_new")
    # Same vuln id, different lockfile path -> keyed by id, so NOT new.
    head = od.parse_osv_report(report("GHSA-aaaa", path="coop/crates/web/bun.lock"))
    new, _, _ = od.diff_new(["GHSA-aaaa"], head)
    eq("moved vuln not new", new, [])


def test_diff_removed_is_clean():
    print("test_diff_removed_is_clean")
    # Baseline had GHSA-aaaa; MR removed it -> head empty -> no new, clean.
    head = od.parse_osv_report(report())  # no vulns
    new, headlist, _ = od.diff_new(["GHSA-aaaa"], head)
    eq("removal clean", new, [])
    eq("head empty", headlist, [])


def _run_main(argv):
    old = sys.stdout
    sys.stdout = io.StringIO()
    try:
        rc = od.main(argv)
        return rc, sys.stdout.getvalue()
    finally:
        sys.stdout = old


def test_cli_exit_codes(td):
    print("test_cli_exit_codes")
    baseline = os.path.join(td, "baseline.txt")
    head_clean = os.path.join(td, "clean.json")
    head_bad = os.path.join(td, "bad.json")
    with open(baseline, "w", encoding="utf-8") as fh:
        fh.write("GHSA-aaaa\n")
    with open(head_clean, "w", encoding="utf-8") as fh:
        fh.write(report("GHSA-aaaa"))
    with open(head_bad, "w", encoding="utf-8") as fh:
        fh.write(report("GHSA-aaaa", "GHSA-NEW"))
    rc, _ = _run_main(["--mode", "diff", "--baseline", baseline, "--head", head_clean])
    eq("clean -> exit 0", rc, 0)
    rc, out = _run_main(["--mode", "diff", "--baseline", baseline, "--head", head_bad, "--json"])
    eq("new -> exit 3", rc, 3)
    eq("json gate fail", json.loads(out)["gate"], "fail")
    eq("json new list", json.loads(out)["new"], ["GHSA-NEW"])
    # ids mode
    rc, out = _run_main(["--mode", "ids", "--report", head_bad])
    eq("ids exit 0", rc, 0)
    eq("ids output sorted", out.strip().splitlines(), ["GHSA-NEW", "GHSA-aaaa"])


def main():
    import tempfile

    test_parse()
    test_parse_empty()
    test_parse_garbage()
    test_diff_clean()
    test_diff_new_detected()
    test_diff_moved_lockfile_not_new()
    test_diff_removed_is_clean()
    with tempfile.TemporaryDirectory() as td:
        test_cli_exit_codes(td)

    print()
    if _failures:
        print(f"FAILED ({len(_failures)}): {', '.join(_failures)}")
        return 1
    print("all osv-diff tests passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
