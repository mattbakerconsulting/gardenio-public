#!/usr/bin/env python3
"""check-ci-draft-gate_test.py -- unit tests for check-ci-draft-gate.py

Each case renders a synthetic repo in a tmpdir (three workflow files under
.github/workflows/) and asserts the guard's verdict. GARDENIO_ROOT points the
guard at it.

The POSITIVE CONTROL comes first and matters most: a workflow set that carries
every gate the guard wants must pass, and the guard must say how many heavy
jobs it actually read. A guard whose parser silently matched nothing would
print "no problems" in the same words as a clean tree; the per-workflow job
count is what makes that distinguishable, and `no gated jobs at all` below
pins it.

Then one bad case per failure mode the guard exists to catch (pull requests
run the light tier only, draft or not, since gd-19ECZxvFQw):

  * `pull_request.types` lists `ready_for_review` or `converted_to_draft`
    (a run on a draft flip re-runs the light tier for nothing), or lacks one
    of opened / synchronize / reopened (a head with no run: the required
    checks never report). A bare `pull_request:` is GitHub's default set and
    passes;
  * a heavy job that can run on SOME pull_request: no conjunct, no `if:` at
    all, the conjunct ORed in, the pre-part-B draft conjunct (it lets a ready
    PR through), `always()` or `failure()`, or an `if:` the evaluator cannot
    read. A job that cannot run on a PR in another spelling passes;
  * `draft-light` missing, misnamed (`light:` prefix), still keyed on
    `draft == true` (a ready PR gets nothing), or running on a push;
  * a gate job's checkout with `fetch-depth: 0`, or not sparse
    (gd-LjoM6ipXQX), or a gate job named in GATE_JOBS that no longer exists.
  * the nightly (gd-2kZxn16TaS): ci.yml or checks.yml with no `schedule`; a
    path gate that lets `workflow_dispatch` through but not `schedule`; a gate
    that skips on SOME output value (every value is tried); an `if:` the
    evaluator cannot read; a concurrency key or shell line that handles
    dispatch and forgets schedule;
  * the run-level concurrency key: a pull_request arm still split by draft
    state (gd-gw9mSq9Qtm, retired by gd-19ECZxvFQw), the draft term in any
    arm, no one-slot-per-PR arm, and no `concurrency:` block at all;
  * the required-check aggregators (`ci` in ci.yml, `checks` in checks.yml)
    are exempt from the conjunct, but only in their own workflow;
  * the release tag push: a path gate that forgets the tag arm, except
    ci.yml's gardenhub-browser on its own output (gd-rmHsVjmKse), which
    must still run on a tag when that output is 'true' or the gate failed;
  * images.yml with a `pull_request` or `pull_request_target` trigger, block
    or flow form, or with no `on:` (gd-bjj3robkjs). Its jobs are not held to
    the conjunct: a job with no `if:` there passes.

Then checks.yml's supersession shape (gd-BCyDxqofKe), again positive
controls first -- a compliant set passes and reports how many jobs carry the
re-check, and a path-gated job WITHOUT the run-wide verdict is accepted (that
absence is required, not tolerated) -- then one case per way to lose it: no
gate job, gate job on a heavy runner or comparing the wrong workflow, a gate job
whose `superseded -- run-wide skip` step is missing, renamed or mis-keyed, an
ungated job (or one with no `if:`) without `needs`, the verdict, or
`!cancelled()`, a path-gated job
reading the run-wide verdict, a missing/misnamed/misplaced re-check, no
`actions: read`, no sentinel or a sentinel that is not first, and a step
after the re-check that is unguarded or ORs its guard.

Then ci.yml's (gd-025MCE7PhK): the path-gated `rust` passes without the
run-wide verdict and the pull_request-only-gated `go` passes without `job:`,
then rust losing its `job:`, reading the verdict, or losing its re-check
altogether; go losing the verdict or gaining a push path gate; and `gate`
losing its fired step, passing `job:` or comparing the wrong workflow.

The last case runs the guard against the REAL tree. It is the only case that
proves the three workflow files still exist where the guard looks and still
carry the gates; a guard whose inputs were all renamed reports "no problems"
in the same words as a clean one unless it counts what it read.

Usage:  python3 scripts/check-ci-draft-gate_test.py
"""
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-ci-draft-gate.py')

PASS = FAIL = 0

CONJUNCT = "github.event_name != 'pull_request'"
# The pre-part-B conjunct (gd-JGkmjFfv3B): it let a ready PR run the job.
OLD_CONJUNCT = ("(github.event_name != 'pull_request' || "
                "github.event.pull_request.draft != true)")
GOOD_TYPES = "[opened, synchronize, reopened]"
SCHEDULE = "  schedule:\n    - cron: '43 10 * * *'\n"
# The path-gate arm that forces a job on dispatch, the nightly and a release
# tag push.
FORCED = ("(github.event_name == 'workflow_dispatch' || "
          "github.event_name == 'schedule' || "
          "startsWith(github.ref, 'refs/tags/') || "
          "needs.changes.result != 'success' || "
          "needs.changes.outputs.x == 'true')")
FORCED_GATE = FORCED.replace("needs.changes", "needs.gate")

# The run-level key the guard wants: one slot per PR.
GOOD_GROUP = ("${{ github.workflow }}-${{ github.event_name == 'pull_request' "
              "&& github.ref || github.sha }}")
# The key from gd-gw9mSq9Qtm until part B: PR runs split by draft state.
DRAFT_GROUP = ("${{ github.workflow }}-${{ github.event_name == 'pull_request' "
               "&& format('{0}-{1}', github.ref, "
               "github.event.pull_request.draft && 'draft' || 'ready') "
               "|| github.sha }}")
GOOD_CONCURRENCY = ("concurrency:\n  group: %s\n  cancel-in-progress: true\n"
                    % GOOD_GROUP)

HEADER = """name: %(name)s

on:
  push:
    branches: [main]
%(pr)s  workflow_dispatch:
%(sched)s
%(conc)s

jobs:
"""

GATE_JOB = """  %(name)s:
    name: %(name)s gate
    runs-on: gardenio-k3s-light
    outputs:
      x: ${{ steps.s.outputs.x }}
    steps:
%(checkout)s      - id: s
        run: echo x=1 >> "$GITHUB_OUTPUT"
"""

SPARSE = """      - uses: actions/checkout@v4
        with:
          sparse-checkout: |
            .github/actions/superseded
            scripts/x.py
          sparse-checkout-cone-mode: false
"""
FULL = """      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
"""
PLAIN = """      - name: check out
        uses: actions/checkout@v4
"""

DRAFT_LIGHT = """  draft-light:
    name: "%(lname)s build + vet + changed-package tests"
    runs-on: gardenio-k3s-medium
    timeout-minutes: 15
    needs: gate
    if: >-
      %(lif)s
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
"""

DRAFT_LIGHT_IF = ("github.event_name == 'pull_request' && "
                  "needs.gate.outputs.superseded != 'true'")


def pr_block(types):
    """Render the on.pull_request block; None means a bare `pull_request:`."""
    if types is None:
        return "  pull_request:\n"
    return "  pull_request:\n    types: %s\n" % types


def heavy(name, cond, needs="changes"):
    """Render a heavy job. cond=None omits `if:` entirely; a list renders a
    folded `>-` block, one element per line."""
    out = ["  %s:" % name, "    name: %s (heavy)" % name,
           "    runs-on: gardenio-k3s", "    timeout-minutes: 20"]
    if needs:
        out.append("    needs: %s" % needs)
    if isinstance(cond, list):
        out.append("    if: >-")
        for part in cond:
            out.append("      %s" % part)
    elif cond is not None:
        out.append("    if: %s" % cond)
    out += ["    steps:", "      - uses: actions/checkout@v4", ""]
    return "\n".join(out) + "\n"


def good_cond(extra=FORCED):
    return "!cancelled() && %s && %s" % (extra, CONJUNCT)


RUN_WIDE = "needs.supersession.outputs.superseded != 'true'"
STEP_GUARD = "steps.superseded.outputs.superseded != 'true'"
UNGATED_COND = "!cancelled() && %s && %s" % (RUN_WIDE, CONJUNCT)

SUP_GATE = """  supersession:
    name: superseded by a newer push?
    runs-on: gardenio-k3s-light # a trailing comment is fine
    permissions:
      actions: read
    outputs:
      superseded: ${{ steps.check.outputs.superseded }}
    steps:
      - uses: actions/checkout@v4
        with:
          sparse-checkout: .github/actions/superseded
          sparse-checkout-cone-mode: false
      - id: check
        uses: ./.github/actions/superseded
        with:
          workflow: checks.yml
      - name: superseded -- run-wide skip
        if: steps.check.outputs.superseded == 'true'
        run: echo skipped

"""


def sheavy(name, cond, needs, jname=None, wf="checks.yml", job=True):
    """A heavy job carrying the full supersession shape, checks.yml's unless
    wf says otherwise; job=False leaves the re-check's `job:` out (ci.yml's
    whole-run form). Bad cases take this text and break exactly one thing
    with str.replace."""
    out = ["  %s:" % name, "    name: %s (${{ matrix.x }})" % name,
           "    runs-on: gardenio-k3s"]
    if needs:
        out.append("    needs: %s" % needs)
    if isinstance(cond, list):
        out.append("    if: >-")
        out += ["      %s" % part for part in cond]
    else:
        out.append("    if: %s" % cond)
    out += [
        "    permissions:",
        "      actions: read",
        "      contents: read",
        "    steps:",
        "      - uses: actions/checkout@v4",
        "        with: { fetch-depth: 0 }",
        "      # a comment between steps",
        "      - id: superseded",
        "        uses: ./.github/actions/superseded",
        "        with:",
        "          workflow: %s" % wf,
    ] + (["          job: %s (${{ matrix.x }})" % (jname or name)] if job else []) + [
        "      - name: guard decision",
        "        run: echo \"superseded='${{ steps.superseded.outputs.superseded }}'\"",
        "      - name: heavy work starting",
        "        if: %s" % STEP_GUARD,
        "        run: echo building",
        "      - if: %s" % STEP_GUARD,
        "        run: |",
        "          make build",
        "      - uses: ./.github/actions/peak-memory",
        "        if: always() && %s" % STEP_GUARD,
        "        with: { label: x }",
        "",
    ]
    return "\n".join(out) + "\n"


def checks_wf(jobs=None, sup=True, **kw):
    """checks.yml with the supersession gate job, a path-gated lint-go and
    an ungated terraform, unless jobs says otherwise."""
    if jobs is None:
        jobs = [sheavy("lint-go", good_cond(), "changes"),
                sheavy("terraform", UNGATED_COND, "supersession"),
                heavy("deferred", "false", needs=None)]
    body = workflow("checks", jobs=jobs, **kw)
    if sup:
        body = body.replace("jobs:\n", "jobs:\n" + SUP_GATE, 1)
    return body


# ci.yml's supersession verdict lives on `gate`, which also computes the path
# outputs (gd-LjoM6ipXQX).
CI_RUN_WIDE = "needs.gate.outputs.superseded != 'true'"
# go's path gate: pull_request only, so on a push it is not path-gated.
PR_ONLY_GATE = ("(github.event_name != 'pull_request' || "
                "needs.gate.result != 'success' || "
                "needs.gate.outputs.go_count != '0')")
CI_GATE = """  gate:
    name: "gate: superseded? + changed paths"
    runs-on: gardenio-k3s-light
    permissions:
      actions: read
    outputs:
      superseded: ${{ steps.superseded.outputs.superseded }}
      x: ${{ steps.s.outputs.x }}
    steps:
%(checkout)s      - id: superseded
        continue-on-error: true
        uses: ./.github/actions/superseded
      - name: superseded -- run-wide skip
        if: steps.superseded.outputs.superseded == 'true'
        run: echo skipped
      - id: s
        run: echo x=1 >> "$GITHUB_OUTPUT"

"""


def ci_jobs():
    """ci.yml's two shapes: `go` reads the run-wide verdict (its path gate is
    pull_request only) and re-checks without `job:`; `rust` is path-gated
    per push, so it does not, and passes `job:`."""
    return [sheavy("go", good_cond(CI_RUN_WIDE + " && " + PR_ONLY_GATE),
                   "gate", wf="ci.yml", job=False),
            sheavy("rust", good_cond(FORCED_GATE), "gate", wf="ci.yml")]


def ci_wf(jobs=None, checkout=SPARSE, **kw):
    """ci.yml with the supersession-carrying `gate` job and ci_jobs(),
    unless jobs says otherwise."""
    body = workflow("ci", gates=(), jobs=ci_jobs() if jobs is None else jobs,
                    **kw)
    return body.replace("jobs:\n", "jobs:\n" + CI_GATE % {"checkout": checkout},
                        1)


def workflow(name, types=GOOD_TYPES, jobs=None, gates=None,
             light=True, lname="light:", lif=DRAFT_LIGHT_IF, checkout=SPARSE,
             sched=SCHEDULE, conc=GOOD_CONCURRENCY):
    if gates is None:
        gates = ("gate",) if name == "ci" else ("changes",)
    body = HEADER % {"name": name, "pr": pr_block(types), "sched": sched,
                     "conc": conc}
    for g in gates:
        body += GATE_JOB % {"name": g, "checkout": checkout} + "\n"
    if name == "ci" and light:
        body += DRAFT_LIGHT % {"lname": lname, "lif": lif} + "\n"
    for j in (jobs if jobs is not None else [heavy("go", good_cond())]):
        body += j
    return body


IMAGES_CONCURRENCY = ("concurrency:\n  group: images-${{ github.sha }}\n"
                      "  cancel-in-progress: false\n")


def images_wf(pr=""):
    """images.yml: no pull_request trigger (pr="" renders none), no schedule,
    and jobs that carry no conjunct, since nothing here runs on a PR."""
    return (HEADER % {"name": "images", "pr": pr, "sched": "",
                      "conc": IMAGES_CONCURRENCY}
            + GATE_JOB % {"name": "changes", "checkout": SPARSE} + "\n"
            + heavy("build", None))


def default_set():
    return {
        "ci.yml": ci_wf(),
        "checks.yml": checks_wf(),
        "images.yml": images_wf(),
    }


def check(name, want_hit, want_substr=None, files=None, real_tree=False):
    """Write the file set into a synthetic root and assert the verdict."""
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix='cdg-test-')
    try:
        env = dict(os.environ)
        if real_tree:
            env.pop("GARDENIO_ROOT", None)
        else:
            wf_dir = os.path.join(root, '.github', 'workflows')
            os.makedirs(wf_dir)
            for fname, text in (files or {}).items():
                with open(os.path.join(wf_dir, fname), 'w') as f:
                    f.write(text)
            env["GARDENIO_ROOT"] = root
        p = subprocess.run([sys.executable, GUARD], capture_output=True,
                           text=True, env=env)
        out = p.stdout + p.stderr
        got_hit = p.returncode != 0
        ok = got_hit == want_hit
        if ok and want_substr and want_substr not in out:
            ok = False
            out += '\n(expected output to contain %r)' % want_substr
        if ok:
            print('PASS: %s' % name)
            PASS += 1
        else:
            print('FAIL: %s: expected %s, got rc=%d\n%s'
                  % (name, 'a hit' if want_hit else 'no hit', p.returncode, out))
            FAIL += 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


def with_file(fname, text):
    s = default_set()
    s[fname] = text
    return s


# ── positive control: a clean synthetic set passes and is COUNTED ──────────
check('clean set passes and reports the heavy jobs it read', False,
      want_substr='4 heavy jobs', files=default_set())

check('clean set counts the heavy jobs proven to run on schedule', False,
      want_substr='4 heavy jobs run on schedule', files=default_set())

check('conjunct split across lines of a folded block still counts', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("helm", ["!cancelled() &&",
                          "(github.event_name == 'workflow_dispatch' ||",
                          " github.event_name == 'schedule' ||",
                          " startsWith(github.ref, 'refs/tags/') ||",
                          " needs.changes.result != 'success') &&",
                          "github.event_name !=",
                          "  'pull_request'"],
                 "changes")])))

check('`if: false` job is exempt', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("lint-go", good_cond(), "changes"),
          heavy("deferred", "false", needs=None)])))

check('`if: ${{ ... }}` wrapper is read through', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("lint-go", "${{ %s }}" % good_cond(), "changes")])))

# ── trigger types ───────────────────────────────────────────────────────────
check('ci.yml types list ready_for_review (the pre-part-B trigger)', True,
      'lists `ready_for_review`',
      files=with_file("ci.yml", workflow(
          "ci", types="[opened, synchronize, reopened, ready_for_review]")))

check('bare pull_request: with no types (the default set) passes', False,
      files=with_file("checks.yml", checks_wf(types=None)))

check('block-list types without reopened', True, 'lacks `reopened`',
      files=with_file("checks.yml", checks_wf(
          types="\n      - opened\n      - synchronize")))

check('block-list types with the three, in another order, passes', False,
      files=with_file("checks.yml", checks_wf(
          types="\n      - synchronize\n      - reopened\n      - opened")))

check('types without synchronize', True, 'lacks `synchronize`',
      files=with_file("checks.yml", checks_wf(types="[opened, reopened]")))

check('converted_to_draft present', True, 'converted_to_draft',
      files=with_file("checks.yml", checks_wf(
          types="[opened, synchronize, reopened, converted_to_draft]")))

check('no pull_request trigger at all', True, 'pull_request',
      files=with_file("checks.yml", HEADER % {"name": "checks", "pr": "",
                                                "sched": SCHEDULE,
                                                "conc": GOOD_CONCURRENCY}
                      + GATE_JOB % {"name": "changes", "checkout": SPARSE} + "\n"
                      + heavy("lint-go", good_cond())))

# ── heavy job conditions ────────────────────────────────────────────────────
check('heavy job without the conjunct', True, 'checks.yml: lint-go',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("lint-go", "!cancelled() && needs.changes.result != 'success'")])))

check('heavy job with no `if:` at all', True, 'checks.yml: build',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("build", None)])))

check('conjunct ORed in instead of ANDed', True,
      'ci.yml: go can run on a pull_request',
      files=with_file("ci.yml", workflow(
          "ci", jobs=[heavy("go", "!cancelled() || " + CONJUNCT)])))

check('conjunct with a mutated comparison does not count', True, 'ci.yml: go',
      files=with_file("ci.yml", workflow(
          "ci", jobs=[heavy("go", "!cancelled() && (github.event_name != 'pull_request' "
                      "|| github.event.pull_request.draft == false)")])))

check('the pre-part-B draft conjunct lets a ready PR through', True,
      'github.event.pull_request.draft=False',
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("lint-go", "!cancelled() && %s && %s" % (FORCED, OLD_CONJUNCT),
                 "changes")])))

check('`always()` runs on a PR whatever else it says', True,
      'checks.yml: build can run on a pull_request',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("build", "always()")])))

check('a failure()-only job is tried with failure() true', True,
      "failure()=True", files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("build", "failure() && needs.changes.result != 'success'")])))

check('a job that cannot run on a PR in another spelling passes', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("build", "!cancelled() && %s && "
                          "(github.event_name == 'workflow_dispatch' || "
                          "github.event_name == 'schedule' || "
                          "github.event_name == 'push')" % RUN_WIDE,
                 "supersession")])))

check("the needs['id'] index form is read", False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("build", "!cancelled() && %s && %s && "
                          "(needs['lint-go'].result == 'success' || "
                          "needs['lint-go'].result == 'skipped')"
                          % (RUN_WIDE, CONJUNCT),
                 "[supersession, lint-go]")])))

check('an `if:` with no pull_request model is reported, not passed', True,
      'cannot be evaluated for a pull_request',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("build", "%s && inputs.images != ''" % CONJUNCT)])))

check('workflow with no heavy jobs at all', True, 'no gated jobs',
      files=with_file("checks.yml", checks_wf(jobs=[])))

# ── draft-light ─────────────────────────────────────────────────────────────
check('draft-light missing from ci.yml', True, 'draft-light',
      files=with_file("ci.yml", workflow(
          "ci", light=False)))

check('draft-light name keeps the old draft: prefix', True, 'light:',
      files=with_file("ci.yml", workflow(
          "ci", lname="draft:")))

check('draft-light still keyed on draft == true', True,
      'does not run on a ready pull_request',
      files=with_file("ci.yml", workflow(
          "ci", lif="github.event_name == 'pull_request' && "
                    "github.event.pull_request.draft == true")))

check('draft-light that runs on a push too', True, 'runs on push',
      files=with_file("ci.yml", workflow(
          "ci", lif="needs.gate.outputs.superseded != 'true'")))

# ── gate jobs: which exist, and how they check out (gd-LjoM6ipXQX) ─────────
check('gate with no checkout at all (API only) passes', False,
      files=with_file("checks.yml", checks_wf(checkout="")))

check('gate checkout with fetch-depth: 0', True, 'fetch-depth: 0',
      files=with_file("checks.yml", workflow("checks", checkout=FULL)))

check('gate checkout that is not sparse', True, 'not a `sparse-checkout`',
      files=with_file("checks.yml", checks_wf(checkout=PLAIN)))

check('ci.yml gate checkout with fetch-depth: 0', True, "ci.yml: gate's checkout",
      files=with_file("ci.yml", workflow("ci", checkout=FULL)))

check('ci.yml still split into supersession + changes', True,
      "no `gate` job, but GATE_JOBS exempts it",
      files=with_file("ci.yml", workflow("ci", gates=("supersession", "changes"))))

check('a `changes` job left in ci.yml is not exempt', True,
      "ci.yml: changes has no job-level `if:`",
      files=with_file("ci.yml", workflow("ci", gates=("gate", "changes"))))

# ── the nightly full run ────────────────────────────────────────────────────
check('ci.yml with no schedule trigger', True, 'ci.yml: no `on.schedule`',
      files=with_file("ci.yml", workflow("ci", sched="")))

check('checks.yml schedule with no cron', True, 'checks.yml: no `on.schedule`',
      files=with_file("checks.yml", workflow(
          "checks", sched="  schedule:\n    - branches: [main]\n")))

check('block-list schedule with a double-quoted cron passes', False,
      files=with_file("checks.yml", checks_wf(
          sched='  schedule:\n    - cron: "43 10 * * *"\n'
                '    - cron: "7 22 * * 0"\n')))

check('path gate lets workflow_dispatch through but not schedule', True,
      'checks.yml: lint-go does not run on the nightly',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("lint-go", good_cond(
              "(github.event_name == 'workflow_dispatch' || "
              "needs.changes.result != 'success' || "
              "needs.changes.outputs.lint_count != '0')"))])))

check('gate that skips on the "nothing changed" output alone', True,
      "needs.gate.outputs.coop='false'",
      files=with_file("ci.yml", workflow(
          "ci", jobs=[heavy("rust", good_cond("needs.gate.outputs.coop == 'true'"),
                            needs="gate")])))

check('inverted gate output is caught (every value is tried)', True,
      "needs.changes.outputs.skip='true'",
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("helm", good_cond("needs.changes.outputs.skip != 'true'"))])))

check('a push-only arm does not count as running nightly', True,
      'checks.yml: helm does not run on the nightly',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("helm", good_cond(
              "(github.event_name == 'push' || needs.changes.outputs.helm == 'true')"))])))

# ── the release tag push (owner 2026-11-03) ────────────────────────────────
check('path gate lets dispatch and schedule through but not a tag', True,
      'checks.yml: lint-go does not run on a release tag push',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("lint-go", good_cond(
              "(github.event_name == 'workflow_dispatch' || "
              "github.event_name == 'schedule' || "
              "needs.changes.result != 'success' || "
              "needs.changes.outputs.lint_count != '0')"))])))

check('a branch-only push arm does not count as running on a tag', True,
      'ci.yml: rust does not run on a release tag push',
      files=with_file("ci.yml", workflow("ci", jobs=[
          heavy("rust", good_cond(
              "(github.event_name == 'workflow_dispatch' || "
              "github.event_name == 'schedule' || "
              "github.ref == 'refs/heads/main' || "
              "needs.gate.outputs.coop == 'true')"), needs="gate")])))

# The one tag exception (gd-rmHsVjmKse): ci.yml's browser suite on its own
# gate output. NO_TAG_ARM is the job's real shape.
NO_TAG_ARM = ("(github.event_name == 'workflow_dispatch' || "
              "github.event_name == 'schedule' || "
              "needs.gate.result != 'success' || "
              "needs.gate.outputs.gardenhub_browser == 'true')")
check('the browser suite may skip a tag on its own gate output', False,
      files=with_file("ci.yml", ci_wf(jobs=ci_jobs() + [
          sheavy("gardenhub-browser", good_cond(NO_TAG_ARM), "gate",
                 wf="ci.yml")])))
check('the browser suite must run on a tag when its output says true', True,
      'ci.yml: gardenhub-browser does not run on a release tag push',
      files=with_file("ci.yml", ci_wf(jobs=ci_jobs() + [
          sheavy("gardenhub-browser", good_cond(
              NO_TAG_ARM.replace("needs.gate.outputs.gardenhub_browser",
                                 "needs.gate.outputs.coop")), "gate",
                 wf="ci.yml")])))
check('the browser suite must run on a tag when the gate failed', True,
      "may skip a tag only on needs.gate.outputs.gardenhub_browser == 'false'",
      files=with_file("ci.yml", ci_wf(jobs=ci_jobs() + [
          sheavy("gardenhub-browser", good_cond(
              NO_TAG_ARM.replace("needs.gate.result != 'success' || ", "")),
                 "gate", wf="ci.yml")])))
check('the exception is the browser suite only: rust without a tag arm', True,
      'ci.yml: rust does not run on a release tag push',
      files=with_file("ci.yml", ci_wf(jobs=[
          sheavy("rust", good_cond(NO_TAG_ARM.replace(
              "gardenhub_browser", "coop")), "gate", wf="ci.yml")])))

check('contains(fromJSON(...), event_name) form passes', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("helm", good_cond(
              "(contains(fromJSON('[\"workflow_dispatch\",\"schedule\"]'), "
              "github.event_name) || startsWith(github.ref, 'refs/tags/') || "
              "needs.changes.outputs.helm == 'true')"),
                 "changes")])))

check('superseded is modelled false (supersession is push-only)', False,
      files=with_file("ci.yml", ci_wf(jobs=[
          sheavy("go", good_cond(CI_RUN_WIDE), "gate", wf="ci.yml",
                 job=False)])))

check('a condition the evaluator cannot read is reported, not passed', True,
      'cannot be evaluated',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("helm", good_cond("vars.RUN_HELM == 'yes'"))])))

check('a relational operator is reported, not guessed at', True,
      'relational operator',
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("helm", good_cond("needs.changes.outputs.n > 0"))])))

check('dispatch-only concurrency key is caught', True,
      "handles `workflow_dispatch` but not `schedule`",
      files=with_file("checks.yml", workflow("checks", jobs=[
          heavy("lint-go", good_cond()),
          "  gitleaks:\n    if: %s\n    runs-on: x\n    concurrency:\n"
          "      group: g-${{ github.event_name == 'workflow_dispatch' && "
          "github.run_id || github.ref }}\n    steps:\n      - run: 'true'\n"
          % CONJUNCT])))

check('dispatch-only shell test in a gate step is caught', True,
      '"$EVENT" = "workflow_dispatch"',
      files=with_file("ci.yml", workflow(
          "ci", jobs=[heavy("go", good_cond(FORCED_GATE), needs="gate") + '      - run: |\n'
                '          if [ "$EVENT" = "workflow_dispatch" ]; then echo all; fi\n'])))

# ── run-level concurrency key (gd-gw9mSq9Qtm) ──────────────────────────────
check('PR concurrency arm split by draft state (the pre-part-B key)', True,
      'checks.yml: the run-level `concurrency.group` reads',
      files=with_file("checks.yml", workflow("checks", conc=(
          "concurrency:\n  group: %s\n  cancel-in-progress: true\n"
          % DRAFT_GROUP))))

check('the draft term in another arm is caught too', True,
      'ci.yml: the run-level `concurrency.group` reads',
      files=with_file("ci.yml", ci_wf(conc=(
          "concurrency:\n  group: ci-${{ github.event_name == "
          "'pull_request' && github.ref || format('{0}-{1}', github.sha, "
          "github.event.pull_request.draft && 'draft' || 'ready') }}\n"))))

check('no one-slot-per-PR arm', True, 'no one-slot-per-PR arm',
      files=with_file("ci.yml", workflow("ci", conc=(
          "concurrency:\n  group: ${{ github.workflow }}-${{ github.sha }}\n"
          "  cancel-in-progress: true\n"))))

check('no top-level concurrency block', True,
      'ci.yml: no top-level `concurrency:` block',
      files=with_file("ci.yml", workflow("ci", conc="")))

check('concurrency block with no group', True, 'has no `group:`',
      files=with_file("ci.yml", workflow(
          "ci", conc="concurrency:\n  cancel-in-progress: true\n")))

check('job-level concurrency does not stand in for the run-level key', True,
      'checks.yml: no top-level `concurrency:` block',
      files=with_file("checks.yml", workflow("checks", conc="", jobs=[
          heavy("lint-go", good_cond()),
          "  gitleaks:\n    if: %s\n    runs-on: x\n    concurrency:\n"
          "      group: %s\n    steps:\n      - run: 'true'\n"
          % (CONJUNCT, GOOD_GROUP)])))

# ── checks.yml supersession: positive controls ────────────────────────────
check('supersession: clean set reports the jobs carrying the re-check', False,
      want_substr='4 heavy jobs in ci.yml, checks.yml carry the supersession '
                  're-check', files=default_set())

check('supersession: ungated job with a folded `if:` and flow-list needs',
      False, files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("security", ["!cancelled() &&", RUN_WIDE + " &&", CONJUNCT],
                 "[supersession]")])))

check('supersession: block-list needs counts', False,
      files=with_file("checks.yml", checks_wf(jobs=[
          sheavy("quench", UNGATED_COND, "\n      - supersession")])))


def mutate(old, new, job=None, fname="checks.yml"):
    """The default checks.yml (or fname) with one substring replaced (once),
    inside job's text when job is given. Asserts the substring exists, so a
    test cannot pass by mutating nothing."""
    text = default_set()[fname]
    if job is not None:
        start = text.index("  %s:\n" % job)
        nxt = text.find("\n  ", start + 3)
        while nxt != -1 and text[nxt + 3] == " ":
            nxt = text.find("\n  ", nxt + 1)
        end = len(text) if nxt == -1 else nxt
        chunk = text[start:end]
        assert old in chunk, (job, old)
        text = text[:start] + chunk.replace(old, new, 1) + text[end:]
    else:
        assert old in text, old
        text = text.replace(old, new, 1)
    return with_file(fname, text)


# ── checks.yml supersession: the gate job ───────────────────────────────────
check('supersession: no gate job', True, 'no `supersession` job',
      files=with_file("checks.yml", checks_wf(sup=False)))

check('supersession: gate job renamed (the jobs API history keys on it)',
      True, 'jobs API history tells', files=mutate(
          "    name: superseded by a newer push?",
          "    name: supersession gate", job="supersession"))

check('supersession: gate job on a heavy runner', True, 'gardenio-k3s-light',
      files=mutate("runs-on: gardenio-k3s-light", "runs-on: gardenio-k3s",
                   job="supersession"))

check('supersession: gate job compares against the default (ci.yml)', True,
      'CI.YML', files=mutate("        with:\n          workflow: checks.yml\n",
                             "", job="supersession"))

check('supersession: gate job passes job:', True, 'run-wide',
      files=mutate("          workflow: checks.yml\n",
                   "          workflow: checks.yml\n          job: x\n",
                   job="supersession"))

check('supersession: gate job has no fired step', True, 'run-wide skip',
      files=mutate("      - name: superseded -- run-wide skip\n"
                   "        if: steps.check.outputs.superseded == 'true'\n"
                   "        run: echo skipped\n", "", job="supersession"))

check('supersession: fired step renamed (release-deploy.sh keys on it)', True,
      'run-wide skip', files=mutate(
          "      - name: superseded -- run-wide skip\n",
          "      - name: gate fired\n", job="supersession"))

check('supersession: fired step runs unconditionally', True,
      "if: steps.check.outputs.superseded == 'true'", files=mutate(
          "        if: steps.check.outputs.superseded == 'true'\n", "",
          job="supersession"))

check('supersession: fired step keyed on the wrong value', True,
      'run-wide skip', files=mutate(
          "if: steps.check.outputs.superseded == 'true'",
          "if: steps.check.outputs.superseded != 'true'", job="supersession"))

check('supersession: fired step keyed on the composite id', False,
      files=with_file("checks.yml", checks_wf().replace(
          "      - id: check\n", "      - id: gate\n", 1).replace(
          "steps.check.outputs.superseded == 'true'",
          "steps.gate.outputs.superseded == 'true'", 1)))

check('supersession: fired step BEFORE the composite call', True,
      'run-wide skip', files=with_file("checks.yml", checks_wf().replace(
          "      - id: check\n        uses: ./.github/actions/superseded\n"
          "        with:\n          workflow: checks.yml\n"
          "      - name: superseded -- run-wide skip\n"
          "        if: steps.check.outputs.superseded == 'true'\n"
          "        run: echo skipped\n",
          "      - name: superseded -- run-wide skip\n"
          "        if: steps.check.outputs.superseded == 'true'\n"
          "        run: echo skipped\n"
          "      - id: check\n        uses: ./.github/actions/superseded\n"
          "        with:\n          workflow: checks.yml\n", 1)))

# ── checks.yml supersession: job-level ──────────────────────────────────────
check('supersession: ungated job without needs: supersession', True,
      "terraform: `needs:` lacks", files=mutate(
          "    needs: supersession\n", "", job="terraform"))

check('supersession: ungated job without the run-wide verdict', True,
      "terraform's `if:` lacks the run-wide", files=mutate(
          RUN_WIDE + " && ", "", job="terraform"))

check('supersession: ungated job ORs the run-wide verdict', True,
      "does not AND the run-wide", files=mutate(
          RUN_WIDE + " &&", RUN_WIDE + " ||", job="terraform"))

check('supersession: ungated job without !cancelled()', True,
      "failing open", files=mutate("!cancelled() && ", "", job="terraform"))

check('supersession: path-gated job reads the run-wide verdict', True,
      'path-gated', files=mutate(
          "!cancelled() &&", "!cancelled() && %s &&" % RUN_WIDE, job="lint-go"))

check('supersession: no actions: read', True, 'actions: read', files=mutate(
          "      actions: read\n", "", job="terraform"))

check('supersession: heavy job with no `if:` is checked, not skipped', True,
      "security: `needs:` lacks", files=with_file("checks.yml", checks_wf(
          jobs=[sheavy("security", CONJUNCT, None).replace(
              "    if: %s\n" % CONJUNCT, "")])))

# ── checks.yml supersession: steps ──────────────────────────────────────────
check('supersession: no in-job re-check', True, 'no in-job', files=mutate(
          "        uses: ./.github/actions/superseded\n",
          "        uses: ./.github/actions/not-it\n", job="lint-go"))

check('supersession: re-check job: does not match name:', True,
      'must match exactly', files=mutate(
          "          job: lint-go (", "          job: lint-go-x (", job="lint-go"))

def unnamed_terraform(job_line):
    """The default set with terraform's `name:` dropped and its re-check's
    `job:` line replaced by job_line ("" drops it)."""
    files = mutate("    name: terraform (${{ matrix.x }})\n", "", job="terraform")
    files["checks.yml"] = files["checks.yml"].replace(
        "          job: terraform (${{ matrix.x }})\n", job_line, 1)
    return files


check('supersession: no name: and no job: (whole-run semantics)', True,
      'must match exactly', files=unnamed_terraform(""))

check('supersession: no name: and job: is the job id (as GitHub names it)',
      False, files=unnamed_terraform("          job: terraform\n"))

check('supersession: re-check without workflow:', True,
      'lacks `workflow: checks.yml`', files=mutate(
          "          workflow: checks.yml\n", "", job="terraform"))

check('supersession: re-check id is not superseded', True, "`id:`",
      files=mutate("      - id: superseded\n", "      - id: sup\n",
                   job="terraform"))

check('supersession: a non-checkout step before the re-check', True,
      'only `actions/checkout`', files=mutate(
          "      # a comment between steps\n",
          "      - run: make deps\n", job="terraform"))

check('supersession: no sentinel', True, 'first guarded step must be',
      files=mutate("      - name: heavy work starting\n",
                   "      - name: building\n", job="terraform"))

check('supersession: sentinel with an extra condition', True,
      'first guarded step must be', files=mutate(
          "        if: %s\n        run: echo building" % STEP_GUARD,
          "        if: success() && %s\n        run: echo building" % STEP_GUARD,
          job="lint-go"))

check('supersession: unguarded step after the re-check', True,
      "'run: make build' is unguarded", files=mutate(
          "      - if: %s\n        run: |" % STEP_GUARD,
          "      - run: |", job="lint-go"))

check('supersession: step guard ORed in', True, 'not ANDed', files=mutate(
          "always() && " + STEP_GUARD, "always() || " + STEP_GUARD,
          job="terraform"))

check('supersession: guard decision AFTER the sentinel is a guarded step', True,
      "'guard decision' is unguarded", files=mutate(
          "      - if: %s\n        run: |" % STEP_GUARD,
          "      - name: guard decision\n        run: echo\n"
          "      - if: %s\n        run: |" % STEP_GUARD, job="lint-go"))

# ── the required-check aggregators (gd-UqzOTRj6cX) ─────────────────────────
# `if: always()` and no heavy work: exempt from the conjunct and from the
# supersession shape, by job id and only in their own workflow.
AGG = """  %s:
    name: ${{ github.event.pull_request.draft && '%s (draft)' || '%s' }}
    needs: [gate]
    if: always()
    runs-on: gardenio-k3s-light
    steps:
      - run: "true"
"""
s = default_set()
s["ci.yml"] += AGG % ("ci", "ci", "ci")
s["checks.yml"] += AGG % ("checks", "checks", "checks")
check('aggregators `ci` and `checks` are exempt in their own workflows', False,
      want_substr='4 heavy jobs', files=s)

check('an aggregator id is not exempt in another workflow', True,
      'checks.yml: ci can run on a pull_request',
      files=with_file("checks.yml", default_set()["checks.yml"]
                      + AGG % ("ci", "ci", "ci")))

check('`checks` is not exempt in ci.yml', True,
      'ci.yml: checks can run on a pull_request',
      files=with_file("ci.yml", default_set()["ci.yml"]
                      + AGG % ("checks", "checks", "checks")))

# ── ci.yml supersession (gd-025MCE7PhK) ─────────────────────────────────────
# Same shape as checks.yml, with ci.yml's differences: the verdict lives on
# `gate`, a job is path-gated if its `if:` can be false on a main push (go's
# pull_request-only gate is not), and an ungated job may re-check without
# `job:` because every newer run runs it.
def cmutate(old, new, job=None):
    return mutate(old, new, job=job, fname="ci.yml")


check('ci supersession: path-gated rust WITHOUT the run-wide verdict passes',
      False, files=default_set())

check('ci supersession: ungated go re-checking with a matching job: passes',
      False, files=with_file("ci.yml", ci_wf(jobs=[
          sheavy("go", good_cond(CI_RUN_WIDE + " && " + PR_ONLY_GATE), "gate",
                 wf="ci.yml")])))

check('ci supersession: re-check may leave workflow: to the default', False,
      files=cmutate("          workflow: ci.yml\n", "", job="rust"))

check('ci supersession: rust re-check without job: (whole-run semantics)',
      True, "ci.yml: rust: the re-check's `job:` is None", files=cmutate(
          "          job: rust (${{ matrix.x }})\n", "", job="rust"))

check('ci supersession: rust reads the run-wide verdict', True,
      "ci.yml: rust is path-gated (its `if:` is false on a main push when "
      "needs.gate.outputs.x=", files=cmutate(
          "!cancelled() &&", "!cancelled() && %s &&" % CI_RUN_WIDE, job="rust"))

check('ci supersession: path-gated job with neither verdict nor re-check',
      True, "ci.yml: rust: no in-job", files=cmutate(
          "        uses: ./.github/actions/superseded\n",
          "        uses: ./.github/actions/not-it\n", job="rust"))

check('ci supersession: rust re-check reads checks.yml runs', True,
      "ci.yml: rust: the re-check lacks `workflow: ci.yml`", files=cmutate(
          "          workflow: ci.yml\n", "          workflow: checks.yml\n",
          job="rust"))

check('ci supersession: go without the run-wide verdict', True,
      "ci.yml: go's `if:` lacks the run-wide", files=cmutate(
          CI_RUN_WIDE + " && ", "", job="go"))

check('ci supersession: go with a push path gate becomes path-gated', True,
      "ci.yml: go is path-gated", files=cmutate(
          PR_ONLY_GATE, FORCED_GATE, job="go"))

check('ci supersession: go with a mismatched job:', True,
      'must match exactly', files=cmutate(
          "          workflow: ci.yml\n",
          "          workflow: ci.yml\n          job: go-x\n", job="go"))

check('ci supersession: gate has no fired step', True,
      'ci.yml: gate needs a step named', files=cmutate(
          "      - name: superseded -- run-wide skip\n"
          "        if: steps.superseded.outputs.superseded == 'true'\n"
          "        run: echo skipped\n", "", job="gate"))

check('ci supersession: gate passes job:', True, 'run-wide and must not',
      files=cmutate("        continue-on-error: true\n",
                    "        continue-on-error: true\n"
                    "        with:\n          job: x\n", job="gate"))

check('ci supersession: gate compares against checks.yml', True,
      'ci.yml: gate calls', files=cmutate(
          "        continue-on-error: true\n",
          "        continue-on-error: true\n"
          "        with:\n          workflow: checks.yml\n", job="gate"))

check('ci supersession: gate does not call the composite', True,
      'ci.yml: gate does not call', files=cmutate(
          "        uses: ./.github/actions/superseded\n",
          "        uses: ./.github/actions/not-it\n", job="gate"))

check('ci supersession: unguarded step in a path-gated job', True,
      "ci.yml: rust: step 'run: make build' is unguarded", files=cmutate(
          "      - if: %s\n        run: |" % STEP_GUARD, "      - run: |",
          job="rust"))

# ── images.yml: no pull_request trigger at all (gd-bjj3robkjs) ─────────────
check('images.yml with no PR trigger, and a job with no `if:`, passes', False,
      want_substr='no pull_request trigger in images.yml', files=default_set())

check('images.yml with a pull_request trigger', True,
      'images.yml: `on:` lists `pull_request`',
      files=with_file("images.yml", images_wf(pr=pr_block(GOOD_TYPES))))

check('images.yml with a bare pull_request: trigger', True,
      'images.yml: `on:` lists `pull_request`',
      files=with_file("images.yml", images_wf(pr=pr_block(None))))

check('images.yml with a pull_request_target trigger', True,
      'images.yml: `on:` lists `pull_request_target`',
      files=with_file("images.yml", images_wf(pr="  pull_request_target:\n")))

check('images.yml with a flow-list `on:` naming pull_request', True,
      'images.yml: `on:` lists `pull_request`',
      files=with_file("images.yml", images_wf().replace(
          "on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n",
          "on: [push, pull_request, workflow_dispatch]\n", 1)))

check('images.yml with no `on:` at all', True,
      'images.yml: no top-level `on:` block',
      files=with_file("images.yml", images_wf().replace(
          "on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n", "", 1)))

# ── files ───────────────────────────────────────────────────────────────────
s = default_set()
del s["images.yml"]
check('a workflow file is missing', True, 'images.yml', files=s)

# ── the real tree ───────────────────────────────────────────────────────────
check('REAL tree: ci.yml and checks.yml run no heavy job on a PR',
      False, want_substr='check-ci-draft-gate: OK', real_tree=True)

check('REAL tree: images.yml has no pull_request trigger', False,
      want_substr='no pull_request trigger in images.yml', real_tree=True)

check('REAL tree: ci.yml and checks.yml heavy jobs all run on schedule',
      False, want_substr='heavy jobs run on schedule', real_tree=True)

check('REAL tree: ci.yml and checks.yml heavy jobs all run on a release tag',
      False, want_substr='heavy jobs run on a release tag push', real_tree=True)

check('REAL tree: ci.yml and checks.yml heavy jobs carry the supersession '
      're-check', False,
      want_substr='heavy jobs in ci.yml, checks.yml carry the supersession',
      real_tree=True)

print('\n%d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
