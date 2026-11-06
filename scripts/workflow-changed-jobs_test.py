#!/usr/bin/env python3
"""Self-test for workflow-changed-jobs.py.

A GREEN SELF-TEST IS NOT COVERAGE -- it proves the splitter's logic, never
that the gate in checks.yml is wired to it. The wiring is verified separately
by T_wired_into_checks_yml below, which reads the workflow with comments
STRIPPED, because a comment naming a script is not an invocation of it.

The cases are grouped by what they must guarantee, and the fail-open cases
outnumber the narrowing ones on purpose: every narrowing bug skips a job that
should have run, which is invisible, and every fail-open bug costs runner time,
which is not.
"""

import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import importlib.util

_spec = importlib.util.spec_from_file_location(
    "wcj", os.path.join(HERE, "workflow-changed-jobs.py")
)
wcj = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(wcj)

REPO = os.path.dirname(HERE)

PASS = 0
FAIL = 0


def check(name, got, want):
    global PASS, FAIL
    if got == want:
        PASS += 1
        print("  PASS %s" % name)
    else:
        FAIL += 1
        print("  FAIL %s\n    got:  %r\n    want: %r" % (name, got, want))


BASE = """\
name: checks
on:
  push:
    branches: [main]
env:
  GO_VERSION: 1.25.14
jobs:
  changes:
    name: changed-path gate
    runs-on: light
    steps:
      - run: echo gate
  lint-go:
    needs: changes
    runs-on: heavy
    steps:
      - run: golangci-lint run ./...
  # a comment that sits between two jobs
  helm:
    needs: changes
    runs-on: heavy
    steps:
      - run: helm lint
"""


def edit(text, old, new):
    assert old in text, "fixture drift: %r not in fixture" % old
    return text.replace(old, new, 1)


print("== splitter")
pre, jobs, post, order = wcj.split_workflow(BASE)
check("job order", order, ["changes", "lint-go", "helm"])
check("preamble stops at jobs:", pre.endswith("jobs:\n"), True)
check("top-level env is in the preamble", "GO_VERSION" in pre, True)
check("postamble empty", post, "")
check("lint-go block has its own step", "golangci-lint run" in jobs["lint-go"], True)
check(
    "boundary comment attaches FORWARD, to helm",
    "a comment that sits between two jobs" in jobs["helm"],
    True,
)
check(
    "boundary comment is NOT in lint-go",
    "a comment that sits between two jobs" in jobs["lint-go"],
    False,
)

print("== narrowing: only the edited job is reported")
check("identical files report nothing", wcj.compare(BASE, BASE), ("JOBS", []))
check(
    "editing one job's step reports only that job",
    wcj.compare(BASE, edit(BASE, "helm lint", "helm lint --strict")),
    ("JOBS", ["helm"]),
)
check(
    "editing a job's comment reports only that job",
    wcj.compare(BASE, edit(BASE, "a comment that sits", "an edited comment that sits")),
    ("JOBS", ["helm"]),
)
check(
    "two edited jobs report both, in file order",
    wcj.compare(
        BASE,
        edit(edit(BASE, "helm lint", "helm lint --strict"), "runs-on: heavy", "runs-on: medium"),
    ),
    ("JOBS", ["lint-go", "helm"]),
)

print("== fail-open: anything outside the jobs mapping")
for label, old, new in [
    ("top-level env", "GO_VERSION: 1.25.14", "GO_VERSION: 1.26.0"),
    ("workflow name", "name: checks", "name: checks2"),
    ("on: trigger", "branches: [main]", "branches: [main, next]"),
]:
    scope, reason = wcj.compare(BASE, edit(BASE, old, new))
    check("%s fails open" % label, scope, "ALL")

print("== fail-open: the job set is not stable")
added = BASE + "  extra:\n    runs-on: light\n    steps:\n      - run: echo hi\n"
check("an added job fails open", wcj.compare(BASE, added)[0], "ALL")
check("a removed job fails open", wcj.compare(added, BASE)[0], "ALL")
renamed = edit(BASE, "  helm:\n", "  helm-lint:\n")
check("a renamed job fails open", wcj.compare(BASE, renamed)[0], "ALL")

print("== fail-open: --fail-open-on names a job others depend on")
gate_edited = edit(BASE, "run: echo gate", "run: echo gate v2")
check(
    "editing `changes` alone narrows to `changes` without the flag",
    wcj.compare(BASE, gate_edited),
    ("JOBS", ["changes"]),
)
check(
    "editing `changes` fails open WITH the flag",
    wcj.compare(BASE, gate_edited, fail_open_on=["changes"])[0],
    "ALL",
)
check(
    "the flag does not fail open when that job is untouched",
    wcj.compare(
        BASE, edit(BASE, "helm lint", "helm lint --strict"), fail_open_on=["changes"]
    ),
    ("JOBS", ["helm"]),
)

print("== fail-open: unparsable input")
check("empty input fails open", wcj.compare("", "")[0], "ALL")
check("no jobs: key fails open", wcj.compare("name: x\n", "name: x\n")[0], "ALL")
check(
    "two jobs: keys fail open",
    wcj.compare(BASE + "jobs:\n  b:\n    runs-on: x\n", BASE)[0],
    "ALL",
)
check(
    "a duplicate job key fails open",
    wcj.compare(BASE, BASE + "  helm:\n    runs-on: heavy\n")[0],
    "ALL",
)

print("== block scalars cannot impersonate a job header")
# A `run: |` body is indented under a key at >=6 spaces, so nothing inside it
# can sit at the two-space indent a job key needs. This asserts the property
# rather than trusting it.
scalar = edit(
    BASE,
    "      - run: helm lint\n",
    "      - run: |\n          echo 'not-a-job:'\n          helm lint\n",
)
_, sjobs, _, sorder = wcj.split_workflow(scalar)
check("block scalar adds no phantom job", sorder, ["changes", "lint-go", "helm"])
check("block scalar body stays in helm", "not-a-job:" in sjobs["helm"], True)

print("== CLI contract")


def cli(base_text, head_text, *flags):
    import tempfile

    with tempfile.TemporaryDirectory() as tmp:
        b = os.path.join(tmp, "base.yml")
        h = os.path.join(tmp, "head.yml")
        for path, text in ((b, base_text), (h, head_text)):
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(text)
        proc = subprocess.run(
            [sys.executable, os.path.join(HERE, "workflow-changed-jobs.py")]
            + list(flags)
            + [b, h],
            capture_output=True,
            text=True,
        )
        return proc.returncode, proc.stdout


rc, out = cli(BASE, edit(BASE, "helm lint", "helm lint --strict"))
check("cli rc", rc, 0)
check("cli narrows", out, "scope=JOBS\nchanged=helm\n")
rc, out = cli(BASE, edit(BASE, "GO_VERSION: 1.25.14", "GO_VERSION: 1.26.0"))
check("cli fails open on env", out.splitlines()[0], "scope=ALL")
check("cli prints a reason", out.splitlines()[1].startswith("reason="), True)
rc, out = cli(BASE, gate_edited, "--fail-open-on", "changes")
check("cli honours --fail-open-on", out.splitlines()[0], "scope=ALL")

print("== a missing file fails open rather than crashing")
proc = subprocess.run(
    [
        sys.executable,
        os.path.join(HERE, "workflow-changed-jobs.py"),
        os.path.join(HERE, "no-such-file-here.yml"),
        os.path.join(REPO, ".github/workflows/checks.yml"),
    ],
    capture_output=True,
    text=True,
)
check("missing base rc", proc.returncode, 0)
check("missing base scope", proc.stdout.splitlines()[0], "scope=ALL")

print("== against the REAL checks.yml, not a fixture")
real_path = os.path.join(REPO, ".github/workflows/checks.yml")
with open(real_path, "r", encoding="utf-8") as handle:
    real = handle.read()
rpre, rjobs, rpost, rorder = wcj.split_workflow(real)
check("real workflow splits", len(rorder) >= 10, True)
for needed in ("changes", "lint-go", "helm", "cargo-security", "security", "gitleaks"):
    check("real workflow has %s" % needed, needed in rjobs, True)
check(
    "the `on:` block's push/pull_request keys are NOT mistaken for jobs",
    [j for j in rorder if j in ("push", "pull_request", "workflow_dispatch")],
    [],
)
check("real workflow round-trips byte-for-byte", rpre + "".join(rjobs[j] for j in rorder) + rpost, real)
check("real workflow compares clean against itself", wcj.compare(real, real), ("JOBS", []))

print("== T_wired_into_checks_yml (invocation, not logic)")
# Comments stripped: a comment naming this script is not an invocation.
stripped = re.sub(r"(?m)#.*$", "", real)
check(
    "checks.yml actually runs workflow-changed-jobs.py",
    "scripts/workflow-changed-jobs.py" in stripped,
    True,
)
check(
    "and passes --fail-open-on changes",
    re.search(r"workflow-changed-jobs\.py[^\n]*--fail-open-on[^\n]*changes", stripped)
    is not None
    or re.search(r"--fail-open-on[ \t]+changes", stripped) is not None,
    True,
)
check(
    "this self-test is itself in the script self-tests list",
    "scripts/workflow-changed-jobs_test.py" in stripped,
    True,
)

print("workflow-changed-jobs_test: %d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
