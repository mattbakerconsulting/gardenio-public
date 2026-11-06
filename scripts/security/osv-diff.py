#!/usr/bin/env python3
"""osv-diff.py — the NEW-vs-baseline decision function for CI / OSV-Scan.

Bundle ka-0000005053 §2.3: OSV manifest/lockfile scanning of the repo's
`bun.lock` surfaces (the JS/TS dependency surface govulncheck does not cover).
Like the Go-Vuln reachability gate, this must gate on what an MR *introduces*,
not on the raw advisory count — the existing bun.lock files already carry
pre-existing advisories (hono, ip-address, qs, ws, ...), so a raw-count gate
would red every MR the moment it touched any lockfile, or worse, red MRs that
touch nothing dependency-related. The signal is the DIFF: an OSV id present on
HEAD that was NOT present at the upstream-main merge-base baseline.

osv-scanner does not carry a Go-style call-graph reachability analysis for npm,
so we cannot filter by reachability the way govulncheck-diff does. The
NEW-vs-baseline diff is the analogous "did THIS MR make it worse" signal.

# osv-scanner v2 JSON shape

  { "results": [ { "source": {"path", "type"},
                   "packages": [ { "package": {"name","version","ecosystem"},
                                   "groups": [{"ids":[...], "max_severity": "7.1"}],
                                   "vulnerabilities": [{"id","aliases":[...]}] } ] } ] }

We key a finding by its canonical OSV id (the `vulnerabilities[].id`, e.g.
GHSA-xxxx / GO-xxxx / CVE-xxxx), independent of which lockfile surfaced it, so a
vuln that merely moves between lockfiles is not counted as "new".

# Modes

  --mode ids <report.json>
        Print the sorted set of OSV vuln ids in an osv-scanner v2 JSON report,
        one per line. Used to snapshot the baseline and HEAD.

  --mode diff --baseline <ids-file> --head <report.json>
        Compute NEW OSV ids on HEAD not in the baseline ids file. Exit 0 = clean,
        exit 3 = gate FAIL (new vulns introduced), exit 2 = usage/parse error.
        --json for machine output.

Pure/hermetic: parsing + diff are library functions the test (osv-diff_test.py)
drives directly against synthetic osv-scanner JSON — no network, no osv-scanner
binary. The bash orchestrator (osv-baseline.sh) runs the real scanner at both
revisions and feeds its output here.
"""

import argparse
import json
import sys


def parse_osv_report(text):
    """Parse an osv-scanner v2 JSON report into a set of OSV vuln ids.

    Robust to an empty/whitespace body (osv-scanner prints nothing to the JSON
    file when it finds no packages) — returns an empty set.
    """
    ids = set()
    text = (text or "").strip()
    if not text:
        return ids
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        return ids
    if not isinstance(data, dict):
        return ids
    for result in data.get("results", []) or []:
        if not isinstance(result, dict):
            continue
        for pkg in result.get("packages", []) or []:
            if not isinstance(pkg, dict):
                continue
            # Prefer the explicit vulnerabilities[].id; fall back to group ids.
            for v in pkg.get("vulnerabilities", []) or []:
                if isinstance(v, dict) and v.get("id"):
                    ids.add(v["id"])
            for g in pkg.get("groups", []) or []:
                if isinstance(g, dict):
                    for gid in g.get("ids", []) or []:
                        if gid:
                            ids.add(gid)
    return ids


def diff_new(baseline_ids, head_ids):
    """NEW OSV ids on HEAD not in the baseline. Returns (new, head, preexisting)."""
    baseline = set(baseline_ids)
    head = set(head_ids)
    new = sorted(head - baseline)
    preexisting = sorted(head & baseline)
    return new, sorted(head), preexisting


def _read(path):
    if path == "-":
        return sys.stdin.read()
    with open(path, "r", encoding="utf-8") as fh:
        return fh.read()


def _cmd_ids(args):
    ids = parse_osv_report(_read(args.report))
    for oid in sorted(ids):
        print(oid)
    return 0


def _cmd_diff(args):
    head_ids = parse_osv_report(_read(args.head))
    baseline_ids = []
    if args.baseline:
        for line in _read(args.baseline).splitlines():
            line = line.strip()
            if line and not line.startswith("#"):
                baseline_ids.append(line)
    new, head, preexisting = diff_new(baseline_ids, head_ids)
    if args.json:
        json.dump(
            {
                "new": new,
                "head": head,
                "preexisting": preexisting,
                "gate": "fail" if new else "pass",
            },
            sys.stdout,
            indent=2,
            sort_keys=True,
        )
        sys.stdout.write("\n")
    else:
        if new:
            print("FAIL: NEW OSV vulnerabilities introduced vs baseline:")
            for oid in new:
                print(f"  + {oid}")
        else:
            print("PASS: no NEW OSV vulnerabilities vs baseline.")
        if preexisting:
            print(f"pre-existing OSV vulns (tracked, non-gating): {len(preexisting)}")
    return 3 if new else 0


def main(argv=None):
    p = argparse.ArgumentParser(description="osv-scanner NEW-vs-baseline diff gate")
    p.add_argument("--mode", required=True, choices=("ids", "diff"))
    p.add_argument("--report", help="osv-scanner v2 JSON report path (ids mode; '-' = stdin)")
    p.add_argument("--head", help="HEAD osv-scanner v2 JSON report path (diff mode; '-' = stdin)")
    p.add_argument("--baseline", help="baseline ids file (diff mode)")
    p.add_argument("--json", action="store_true", help="machine-readable output")
    args = p.parse_args(argv)

    if args.mode == "ids":
        if not args.report:
            p.error("--report is required in ids mode")
        return _cmd_ids(args)
    if args.mode == "diff":
        if not args.head:
            p.error("--head is required in diff mode")
        return _cmd_diff(args)
    return 2


if __name__ == "__main__":
    sys.exit(main())
