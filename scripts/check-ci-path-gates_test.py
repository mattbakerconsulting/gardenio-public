#!/usr/bin/env python3
"""check-ci-path-gates_test.py -- unit tests for check-ci-path-gates.py

Each case builds a synthetic repo in a tmpdir -- this tree's checks.yml and
every file a SOURCED_GATES job sources (empty files) -- optionally mutates it,
and asserts the guard's verdict. GARDENIO_ROOT points the guard at it.

The POSITIVE CONTROL comes first: the unmutated copy must pass and report
what it read. Then one mutation per way the checks can fail silently:

  * the `helm` filter stops matching a pin file the helm job sources, or the
    job sources a file that does not exist;
  * a whole-tree scan dropped from `quench`, or moved into a gated job.

The last case runs the guard against the REAL tree.

Usage:  python3 scripts/check-ci-path-gates_test.py
"""
import importlib.util
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
GUARD = os.path.join(HERE, "check-ci-path-gates.py")
WF = os.path.join(REPO, ".github", "workflows", "checks.yml")

PASS = FAIL = 0

with open(WF) as fh:
    REAL = fh.read()


def guard_sourced():
    spec = importlib.util.spec_from_file_location("pg", GUARD)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    raw = REAL.split("\n")
    sourced = set()
    for job in mod.SOURCED_GATES.values():
        sourced |= mod.sourced_paths(mod.job_block(raw, job))
    return sourced


SOURCED = guard_sourced()


def replaced(old, new):
    def f(text):
        assert text.count(old) == 1, "mutation anchor not unique: %r" % old
        return text.replace(old, new)
    return f


def touch(root, rel, body=""):
    p = os.path.join(root, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as fh:
        fh.write(body)


def check(name, want_hit, want_substr=None, mutate=None, real_tree=False):
    global PASS, FAIL
    env = dict(os.environ)
    tmp = None
    if not real_tree:
        tmp = tempfile.mkdtemp(prefix="path-gates-test-")
        touch(tmp, ".github/workflows/checks.yml",
              mutate(REAL) if mutate else REAL)
        for rel in sorted(SOURCED):
            touch(tmp, rel)
        env["GARDENIO_ROOT"] = tmp
    else:
        env.pop("GARDENIO_ROOT", None)
    try:
        r = subprocess.run([sys.executable, GUARD], env=env,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                           text=True, timeout=180)
    finally:
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
    out = r.stdout + r.stderr
    hit = r.returncode != 0
    ok = hit == want_hit and (want_substr is None or want_substr in out)
    if ok:
        PASS += 1
        print("PASS: %s" % name)
    else:
        FAIL += 1
        print("FAIL: %s (rc=%d)\n%s" % (name, r.returncode, out))


# Positive control: the unmutated copy passes and counts what it read.
check("positive control: this checks.yml", False,
      "2 files sourced by gated jobs, 10 whole-tree scans in an ungated job")

check("helm filter misses the versions.env the helm job sources", True,
      "helm sources gardenio/images/agent/install/versions.env, and the "
      "`helm` filter does not match it",
      mutate=replaced(
          "              - 'gardenio/images/agent/install/versions.env'\n", ""))

check("helm filter misses the checksums.env the helm job sources", True,
      "helm sources gardenio/images/agent/install/checksums.env, and the "
      "`helm` filter does not match it",
      mutate=replaced(
          "              - 'gardenio/images/agent/install/checksums.env'\n", ""))

check("helm job sources a file that does not exist", True,
      "helm sources gardenio/images/agent/install/gone.env, which does not "
      "exist",
      mutate=replaced(
          "          . gardenio/images/agent/install/checksums.env\n"
          "          HELM_VER=",
          "          . gardenio/images/agent/install/checksums.env\n"
          "          . gardenio/images/agent/install/gone.env\n"
          "          HELM_VER="))

check("large-file scan dropped from quench", True,
      "no ungated job (one without `needs: changes`) runs "
      "scripts/check-large-tracked-files.sh",
      mutate=replaced("        run: bash scripts/check-large-tracked-files.sh\n",
                      "        run: true\n"))

# Moved, not dropped: the scan still runs, but only behind helm's path gate.
check("conflict-marker scan moved into the path-gated helm job", True,
      "no ungated job (one without `needs: changes`) runs "
      "scripts/check-merge-conflict-markers.sh",
      mutate=lambda t: replaced(
          "          HELM_VER=",
          "          bash scripts/check-merge-conflict-markers.sh\n"
          "          HELM_VER=")(replaced(
              "        run: bash scripts/check-merge-conflict-markers.sh\n",
              "        run: true\n")(t)))

check("REAL tree passes", False, "10 whole-tree scans", real_tree=True)

print("\ncheck-ci-path-gates_test: %d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
