#!/usr/bin/env python3
"""check-no-pr-goldens.py

A pull request may not change a golden browser screenshot unless its head
branch starts with `bless/goldens-` (owner ruling gd-oxDhsqdFVP, task
gd-fedEoEGC9i). The goldens are re-blessed on main once per release window
by .github/workflows/bless-goldens.yml, which opens the one PR allowed to
touch them.

WHY THIS EXISTS
---------------
The golden PNGs (gardenhub's testdata/golden, 758 files, 44 MB) were the
conflicted files in 56 of the 301 conflicted main-into-PR merges from 10-27 to
11-03, and a PR's goldens are never verified before merge: the job that diffs
them (`gardenhub browser (chromium)` in ci.yml) runs on no pull_request since
0498ce841. A feature PR's re-blessed PNGs were therefore pure conflict surface.

Why a guard and not a unit test: nothing is wrong with the code of a PR that
carries goldens. Only its changed-path list shows it, so only a check over
that list, on the PR, can refuse it.

What counts: any path under one of GOLDEN_DIRS, except their `fonts/`. The
pinned fonts are render inputs, not screenshots, and a light-tier test reads
them (gardenhub glyphs_test.go); a font change still lands in a feature PR and
the next bless re-shoots every image.

Usage:  check-no-pr-goldens.py [--base REF] [--head-branch NAME]
  --base         what to diff against, `git diff --name-only BASE...HEAD`
                 (default origin/main)
  --head-branch  the PR's head branch (default $GITHUB_HEAD_REF, else the
                 checked-out branch)
Exit 0 with a count of the changed paths it read, 1 with the offenders or when
the diff cannot be read (fail closed).
"""
import argparse
import os
import subprocess
import sys

ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))

GOLDEN_DIRS = (
    "gardenio/controller/internal/gardenhub/testdata/golden/",
    "gardenio/controller/cmd/beads-viewer/testdata/golden/",
)
BLESS_PREFIX = "bless/goldens-"


def is_golden(path):
    for d in GOLDEN_DIRS:
        if path.startswith(d):
            return not path[len(d):].startswith("fonts/")
    return False


def git(*args):
    return subprocess.run(("git",) + args, cwd=ROOT, capture_output=True,
                          text=True)


def head_branch(given):
    if given:
        return given
    if os.environ.get("GITHUB_HEAD_REF"):
        return os.environ["GITHUB_HEAD_REF"]
    p = git("rev-parse", "--abbrev-ref", "HEAD")
    return p.stdout.strip() if p.returncode == 0 else ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="origin/main")
    ap.add_argument("--head-branch", default="")
    a = ap.parse_args()

    # --no-renames: a moved golden shows both its old and its new path.
    p = git("diff", "--name-only", "--no-renames", a.base + "...HEAD")
    if p.returncode != 0:
        print("check-no-pr-goldens: FAIL -- `git diff --name-only %s...HEAD` "
              "exited %d; cannot tell which paths this PR changes:\n%s"
              % (a.base, p.returncode, p.stderr.strip()), file=sys.stderr)
        return 1
    changed = [l for l in p.stdout.splitlines() if l.strip()]
    goldens = [c for c in changed if is_golden(c)]
    branch = head_branch(a.head_branch)

    if goldens and branch.startswith(BLESS_PREFIX):
        print("check-no-pr-goldens: OK (bless branch %s; %d of %d changed "
              "paths are goldens)" % (branch, len(goldens), len(changed)))
        return 0
    if goldens:
        print("check-no-pr-goldens: FAIL -- this PR changes %d golden "
              "screenshot(s); only a %s* branch may (gd-fedEoEGC9i):"
              % (len(goldens), BLESS_PREFIX), file=sys.stderr)
        for g in goldens[:20]:
            print("  " + g, file=sys.stderr)
        if len(goldens) > 20:
            print("  ... and %d more" % (len(goldens) - 20), file=sys.stderr)
        dirs = [d.rstrip("/") for d in GOLDEN_DIRS]
        print("Drop them and commit; the next window's bless PR "
              "(bless-goldens.yml) re-shoots them on main. This keeps any "
              "fonts/ change:\n  git restore --source=%s --staged --worktree "
              "-- %s %s" % (a.base, " ".join(dirs), " ".join(
                  "':(exclude)%s/fonts'" % d for d in dirs)), file=sys.stderr)
        return 1
    print("check-no-pr-goldens: OK (%d changed paths vs %s, none a golden)"
          % (len(changed), a.base))
    return 0


if __name__ == "__main__":
    sys.exit(main())
