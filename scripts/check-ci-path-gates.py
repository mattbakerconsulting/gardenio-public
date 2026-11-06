#!/usr/bin/env python3
"""check-ci-path-gates.py

Guard what the path-gated jobs of .github/workflows/checks.yml read, and keep
the whole-tree scans out from behind a path gate (gd-is2Nts83Ai; trimmed to
these two checks by gd-whdmfznixO, when the PR-only `terraform` and `scripts`
gates were retired).

WHY THIS EXISTS
---------------
A positive path list fails CLOSED: add a file a gated job reads outside its
filter and the job silently stops running for changes to it -- a green skip.
A unit test cannot see that: the filter and the job are YAML in one file, and
only reading both together says whether they still agree.

WHAT IT CHECKS
--------------
1. Every file a filter-gated job sources (`. path` / `source path`, in
   SOURCED_GATES) exists and is matched by that job's filter. A pin file the
   job reads that way changes what it runs without changing its block, so the
   filter must watch it (the `helm` job's promtool pin in versions.env).
2. The whole-tree scans (WHOLE_TREE). Their verdict can change with any file
   anywhere, so no path gate may skip them: each is invoked by an ungated job
   (no `needs: changes`).

Stdlib only (the light runner image has no PyYAML). Reuses
check-ci-draft-gate.py's line reader.

Usage:  python3 scripts/check-ci-path-gates.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-ci-path-gates.py
"""
import importlib.util
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(HERE)
WORKFLOW = os.path.join(ROOT, ".github", "workflows", "checks.yml")

# filter -> the job whose `. path` reads that filter must match (check 1).
SOURCED_GATES = {"helm": "helm"}
SOURCE = re.compile(r'^\s*(?:\.|source)\s+([A-Za-z0-9_./-]+)(?:\s|;|$)')
# Scans whose verdict any file in the tree can change. They live in an
# ungated job; see `quench`.
WHOLE_TREE = (
    "scripts/check-merge-conflict-markers.sh",
    "scripts/check-large-tracked-files.sh",
    "scripts/check-go-module-coverage.sh",
    "scripts/security/lint-kubectl-apply-secret.py",
    "scripts/check-sigpipe-producers.py",       # every tracked *.sh
    "scripts/check-no-curl-pipe-shell.sh",      # gardenio/{helm,terraform}/...
    "scripts/ratchets/line-budget.sh",          # every Go package
    "scripts/lint-public-strings.sh",           # every file in the tree
    "scripts/check-cmd-shipped.py",             # every controller cmd/ dir
    "scripts/check-guard-provenance.py",        # every guard test in the tree
)


def load(name, fname):
    spec = importlib.util.spec_from_file_location(
        name, os.path.join(HERE, fname))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


DG = load("draft_gate", "check-ci-draft-gate.py")


def glob_re(pattern):
    """picomatch-style glob (dot: true, as dorny/paths-filter sets it)."""
    out, i = [], 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif pattern.startswith("**", i):
            out.append(".*")
            i += 2
        elif pattern[i] == "*":
            out.append("[^/]*")
            i += 1
        elif pattern[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(pattern[i]))
            i += 1
    return re.compile("^" + "".join(out) + "$")


def job_block(raw_lines, job):
    """Raw lines of `jobs.<job>` (indent 2), comments and blanks kept."""
    start = None
    for i, line in enumerate(raw_lines):
        if start is None:
            if line.rstrip() == "  %s:" % job:
                start = i
            continue
        if line.strip() and not line.lstrip().startswith("#") and \
                DG.indent(line) <= 2:
            return raw_lines[start:i]
    return raw_lines[start:] if start is not None else None


def nested(raw, i):
    """Raw lines nested under raw[i] (strictly deeper; blanks and comment
    lines carried along until the first real line at or above its indent)."""
    base = DG.indent(raw[i])
    j = i + 1
    while j < len(raw):
        line = raw[j]
        if line.strip() and not line.lstrip().startswith("#") and \
                DG.indent(line) <= base:
            break
        j += 1
    return raw[i + 1:j]


def needs_of(blk):
    for line in blk:
        m = DG.KEY.match(DG.strip_comment(line))
        if m and DG.indent(line) == 4 and m.group(2) == "needs":
            v = (m.group(3) or "").strip().strip("[]")
            return [x.strip() for x in v.split(",") if x.strip()]
    return []


def filter_patterns(changes, name):
    for i, line in enumerate(changes):
        if line.strip() == "filters: |":
            filt = nested(changes, i)
            for k, fl in enumerate(filt):
                if fl.strip() == name + ":":
                    pats = []
                    for item in nested(filt, k):
                        m = DG.ITEM.match(DG.strip_comment(item))
                        if m:
                            pats.append(DG.unquote(m.group(1)))
                    return pats
    return None


def sourced_paths(block):
    """Repo paths the job block sources with `. path` or `source path`."""
    out = set()
    for line in block:
        m = SOURCE.match(DG.strip_comment(line))
        if m:
            out.add(os.path.normpath(m.group(1)).replace(os.sep, "/"))
    return out


def check_sourced(changes, raw, problems):
    n = 0
    for gate, job in sorted(SOURCED_GATES.items()):
        pats = filter_patterns(changes, gate)
        if not pats:
            problems.append("changes: no `%s:` filter with any patterns" % gate)
            continue
        regs = [glob_re(p) for p in pats]
        for rel in sorted(sourced_paths(job_block(raw, job) or [])):
            n += 1
            if not os.path.isfile(os.path.join(ROOT, rel)):
                problems.append("%s sources %s, which does not exist"
                                % (job, rel))
            elif not any(x.match(rel) for x in regs):
                problems.append("changes: %s sources %s, and the `%s` filter "
                                "does not match it" % (job, rel, gate))
    return n


def check_whole_tree(text, problems):
    raw = text.split("\n")
    found = DG.jobs(DG.logical_lines(text)) or {}
    ungated = []
    for job in found:
        blk = job_block(raw, job)
        if blk and "changes" not in needs_of(blk) and job != "changes":
            ungated.append((job, blk))
    n = 0
    for scan in WHOLE_TREE:
        # An invocation, not a mention: shellcheck lints the script itself.
        run = re.compile(r'\b(?:bash|python3)\s+%s(?:\s|$)' % re.escape(scan))
        homes = [j for j, blk in ungated
                 if any(run.search(DG.strip_comment(l)) for l in blk)]
        if homes:
            n += 1
        else:
            problems.append("no ungated job (one without `needs: changes`) "
                            "runs %s" % scan)
    return n


def main():
    problems = []
    n_src = n_scans = 0
    if not os.path.exists(WORKFLOW):
        problems.append("%s does not exist" % WORKFLOW)
    else:
        with open(WORKFLOW) as fh:
            text = fh.read()
        raw = text.split("\n")
        changes = job_block(raw, "changes")
        if changes is None:
            problems.append("checks.yml: no `changes` job")
        else:
            n_src = check_sourced(changes, raw, problems)
        n_scans = check_whole_tree(text, problems)

    if problems:
        e = sys.stderr
        print("check-ci-path-gates: FAIL", file=e)
        print("", file=e)
        for p in problems:
            print("  %s" % p, file=e)
        print("", file=e)
        print("See the header of %s for why this matters." % __file__, file=e)
        return 1
    print("check-ci-path-gates: OK (%d files sourced by gated jobs, %d "
          "whole-tree scans in an ungated job)" % (n_src, n_scans))
    return 0


if __name__ == "__main__":
    sys.exit(main())
