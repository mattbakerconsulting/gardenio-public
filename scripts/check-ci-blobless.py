#!/usr/bin/env python3
"""check-ci-blobless.py

Every `actions/checkout` that fetches full history (`fetch-depth: 0`) in
.github/workflows/*.yml must also set `filter:` to a value containing
`blob:none` (owner rule 2026-11-03: blobless is mandatory; gd-uHnjMtsHUl).

WHY THIS EXISTS
---------------
Measured 2026-11-03 (16:00-22:05Z), the gitleaks job's full clone took
227-548 s per run while the range scan it fed took ~1 s, and draft-light's
took 120-672 s against ~140 s of build and test. The repo is 1.37 GB on
GitHub and gained 2,305 commits in the six days before; a full clone moves
all of it through the NAT for every PR. A `--filter=blob:none` clone of the
same repo is 15 MB and 4 s, and it has every commit and tree, so merge-base,
`git diff --name-only`, `git log` ranges, tags and `git worktree add` all
work; git fetches the blobs a step actually reads, on demand.

A job that truly needs every blob (the nightly full-history gitleaks scan)
writes the filter as an expression that is `blob:none` on pull_request and
'' otherwise. The literal must still appear, so dropping the filter -- the
silent regression, since the full clone still works, just slowly -- fails.

Why a guard and not a unit test: nothing breaks when the filter is dropped.
The clone still succeeds and every job still passes, minutes slower, so only
a check over the workflow text can see it. Lands get the same rule for their
rendered workflows (Task 3.6 gd-ux2XqmtXXc, Tasks 1.3/1.5 of gd-QM4BoQ9gzO).

Exit 0 with a count of the checkouts it read, 1 with the offenders.
"""
import glob
import os
import re
import sys

ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))
WF_GLOB = os.path.join(ROOT, ".github", "workflows", "*.yml")

CHECKOUT = re.compile(r"^(\s*)(-\s+)?uses:\s*['\"]?actions/checkout@")
FULL = re.compile(r"\bfetch-depth:\s*['\"]?0['\"]?\s*(?:[,}#]|$)", re.M)
FILTER = re.compile(r"\bfilter:\s*(.*?)\s*(?:#.*)?$")


def code(line):
    """The line with a trailing `# comment` removed (not inside quotes)."""
    out, q = [], None
    for ch in line:
        if q:
            if ch == q:
                q = None
        elif ch in "'\"":
            q = ch
        elif ch == "#":
            break
        out.append(ch)
    return "".join(out).rstrip()


def checkout_steps(lines):
    """Yield (line number, [code lines]) for each actions/checkout step."""
    for i, line in enumerate(lines):
        m = CHECKOUT.match(line)
        if not m:
            continue
        # The step's own indent: the `- ` item column. `uses:` on the item
        # line itself, or as a key under an item one line up.
        if m.group(2):
            item = len(m.group(1))
            start = i
        else:
            start = i
            while start > 0 and not re.match(r"^\s*-\s", lines[start]):
                start -= 1
            item = len(lines[start]) - len(lines[start].lstrip())
        body = [code(lines[start])]
        for nxt in lines[start + 1:]:
            if not nxt.strip() or nxt.lstrip().startswith("#"):
                continue
            ind = len(nxt) - len(nxt.lstrip())
            if ind <= item:
                break
            body.append(code(nxt))
        yield start + 1, body


def check(path):
    with open(path) as fh:
        lines = fh.read().splitlines()
    read, problems = 0, []
    for lineno, body in checkout_steps(lines):
        read += 1
        text = "\n".join(body)
        if not FULL.search(text):
            continue
        filt = None
        for b in body:
            m = FILTER.search(b)
            if m:
                filt = m.group(1)
        where = "%s:%d" % (os.path.relpath(path, ROOT), lineno)
        if filt is None:
            problems.append(
                "%s: checkout sets `fetch-depth: 0` with no `filter:`. Add "
                "`filter: blob:none` -- a full clone is 1.37 GB and 4-9 min; "
                "blobless is 15 MB and has every commit and tree." % where)
        elif "blob:none" not in filt:
            problems.append(
                "%s: checkout sets `fetch-depth: 0` with `filter: %s`, which "
                "never asks for `blob:none`." % (where, filt))
    return read, problems


def main():
    paths = sorted(glob.glob(WF_GLOB))
    if not paths:
        print("check-ci-blobless: FAIL -- no workflows under %s" % WF_GLOB,
              file=sys.stderr)
        return 1
    read, problems = 0, []
    for p in paths:
        r, pr = check(p)
        read += r
        problems += pr
    if problems:
        print("check-ci-blobless: FAIL", file=sys.stderr)
        for p in problems:
            print("  " + p, file=sys.stderr)
        print("See the header of %s." % __file__, file=sys.stderr)
        return 1
    if read == 0:
        print("check-ci-blobless: FAIL -- read 0 checkout steps in %d "
              "workflows; the parser matched nothing" % len(paths),
              file=sys.stderr)
        return 1
    print("check-ci-blobless: OK (%d checkout steps in %d workflows)"
          % (read, len(paths)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
