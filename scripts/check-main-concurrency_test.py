#!/usr/bin/env python3
"""check-main-concurrency_test.py -- unit tests for check-main-concurrency.py

Each case copies this tree's ci.yml and checks.yml into a tmpdir, optionally
mutates one of them, and asserts the guard's verdict. GARDENIO_ROOT points the
guard at it.

The POSITIVE CONTROL comes first: the unmutated copy must pass and report the
groups it evaluated -- a per-run group for a release tag push, a cancelling
per-PR group for a pull_request -- and that the path gates still pass the
filter's own answer through when a base was found. Then one mutation per way
the tag-not-main event set (owner 2026-11-03) can break silently:

  * `branches: [main]` back on `on.push`;
  * `on.push.tags` losing the calver pattern;
  * the tag arm dropped from the per-run key, so a tag is per-sha again and
    two tags on one commit share a slot;
  * cancel-in-progress true for a tag push;
  * cancel-in-progress false for pull_request;
  * the dispatch / schedule arm dropped, so their runs share a group;
  * an expression the evaluator cannot read (must fail, not pass);
  * the paths-filter `base:` removed (the evicted-push coverage hole);
  * the paths-filter step skipped on every event, PRs included;
  * a ci.yml path output losing its fail-open arm, or its tag arm while the
    fail-open arm no longer covers a tag;
  * a ci.yml path output forced true even with a base (no positive control);
  * checks.yml's BASE_SHA back on github.event.before;
  * the base step without continue-on-error;
  * the action missing from the gate's sparse checkout;
  * the action's RAN accept-list taking in startup_failure, cancelled or
    failure (gd-NkRBUlh9Ar), RUNNING taking in queued, RAN losing 'success',
    and the loop going back to a deny-list (gd-5VoRbArSS8);
  * the browser suite's tag exception (gd-rmHsVjmKse): the exception gone
    (forced 'true' again), losing its tag-base or filter-outcome term,
    spread to coop, the last-tag-base step without continue-on-error or
    outside the sparse checkout, the filter's base, `if:` or
    continue-on-error no longer covering a tag, and last-tag-base's PASSED
    taking in `skipped`, its JOB or WORKFLOW drifting.

Then both actions' scripts are RUN under node (when there is one) against
mocked run listings: which earlier run becomes the base, and that every
run off the accept-list -- startup_failure and failure above all -- is
stepped over; which earlier tag becomes the browser base, that a skipped
or red suite is stepped over, and that every doubt answers empty.

The last case runs the guard against the REAL tree.

Usage:  python3 scripts/check-main-concurrency_test.py
"""
import glob
import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
GUARD = os.path.join(HERE, "check-main-concurrency.py")
WF_DIR = os.path.join(REPO, ".github", "workflows")
ACTION = os.path.join(".github", "actions", "last-run-base", "action.yml")
TAG_ACTION = os.path.join(".github", "actions", "last-tag-base", "action.yml")

PASS = FAIL = 0

REAL = {}
for _f in ("ci.yml", "checks.yml"):
    with open(os.path.join(WF_DIR, _f)) as _fh:
        REAL[_f] = _fh.read()
for _f in (ACTION, TAG_ACTION):
    with open(os.path.join(REPO, _f)) as _fh:
        REAL[_f] = _fh.read()

DISPATCH_ARM = ("(github.event_name == 'workflow_dispatch' || "
                "github.event_name == 'schedule' || "
                "startsWith(github.ref, 'refs/tags/')) && github.run_id || ")
CANCEL = "  cancel-in-progress: ${{ github.event_name == 'pull_request' }}"


def run_guard(root=None):
    env = dict(os.environ)
    if root is not None:
        env["GARDENIO_ROOT"] = root
    p = subprocess.run([sys.executable, "-S", "-E", GUARD], env=env,
                       capture_output=True, text=True)
    return p.returncode, p.stdout + p.stderr


def case(name, fname=None, old=None, new=None, want_rc=0, want_out=()):
    """Mutate `fname` (replace `old` with `new`; `old` must occur), run the
    guard and check its exit code and that every `want_out` string appears."""
    global PASS, FAIL
    with tempfile.TemporaryDirectory() as tmp:
        for f, body in REAL.items():
            if f == fname:
                if old not in body:
                    print("FAIL %s: mutation target not found in %s: %r"
                          % (name, f, old))
                    FAIL += 1
                    return
                body = body.replace(old, new)
            dest = os.path.join(tmp, f if f in (ACTION, TAG_ACTION) else
                                os.path.join(".github", "workflows", f))
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            with open(dest, "w") as fh:
                fh.write(body)
        rc, out = run_guard(tmp)
    missing = [w for w in want_out if w not in out]
    if rc != want_rc or missing:
        print("FAIL %s: rc=%d (want %d), missing %r\n%s"
              % (name, rc, want_rc, missing, out))
        FAIL += 1
    else:
        print("ok   %s" % name)
        PASS += 1


# ── positive control ─────────────────────────────────────────────────────────
case("positive control: real workflows pass and report what they evaluated",
     want_out=("check-main-concurrency: OK",
               "ci.yml: tag push -> 'ci-101' (cancel-in-progress false)",
               "checks.yml: tag push -> 'checks-101' (cancel-in-progress false)",
               "PR -> 'ci-refs/pull/7/merge' (cancel-in-progress true)",
               "ci.yml: `gate` diffs a push against step `base`; 3 fail-open",
               "ci.yml: `gate` diffs a calver tag's gardenhub_browser against "
               "`steps.tagbase.outputs.base`",
               ".github/actions/last-tag-base: a browser base is a tag whose "
               "gardenhub browser (chromium) concluded success",
               "checks.yml: `changes` diffs a push against step `base`; 2 "
               "fail-open"))

# ── the trigger ──────────────────────────────────────────────────────────────
TAGS = "    tags: ['20*']"
for f in ("ci.yml", "checks.yml"):
    case("%s: main pushes run it again" % f, f, TAGS,
         "    branches: [main]\n" + TAGS,
         1, ("`on.push` has `branches:`",))
case("ci.yml: tags lose the calver pattern", "ci.yml", TAGS,
     "    tags: ['v*']", 1, ("it must list '20*'",))

# ── the concurrency key ──────────────────────────────────────────────────────
for f in ("ci.yml", "checks.yml"):
    case("%s: a tag push per-sha again" % f, f, DISPATCH_ARM,
         DISPATCH_ARM.replace(" || startsWith(github.ref, 'refs/tags/')", ""),
         1, ("two release tag runs share a concurrency group",))
case("ci.yml: cancel-in-progress true for a tag push", "ci.yml", CANCEL,
     "  cancel-in-progress: ${{ github.event_name != 'schedule' }}",
     1, ("cancel-in-progress is true for a tag push",))
case("checks.yml: cancel-in-progress a literal false", "checks.yml", CANCEL,
     "  cancel-in-progress: false",
     1, ("cancel-in-progress is false for pull_request",))
case("ci.yml: dispatch and schedule lose their per-run arm", "ci.yml",
     DISPATCH_ARM, "startsWith(github.ref, 'refs/tags/') && github.run_id || ",
     1, ("two workflow_dispatch runs share the group",
         "two schedule runs share the group"))
case("ci.yml: an expression the evaluator cannot read", "ci.yml", DISPATCH_ARM,
     "contains(github.ref, 'tags') && github.run_id || ",
     1, ("cannot be evaluated", "contains() is not supported"))

# ── the diff base ────────────────────────────────────────────────────────────
FILTER_BASE = {"ci.yml": "          base: ${{ steps.base.outputs.base || "
                          "steps.tagbase.outputs.base }}\n",
               "checks.yml": "          base: ${{ steps.base.outputs.base }}\n"}
for f in ("ci.yml", "checks.yml"):
    case("%s: paths-filter base removed" % f, f, FILTER_BASE[f], "",
         1, ("the paths-filter step's `base:` must be",))
    case("%s: base step without continue-on-error" % f, f,
         "      - id: base\n        continue-on-error: true\n",
         "      - id: base\n",
         1, ("must be `continue-on-error: true`",))
    case("%s: action missing from the sparse checkout" % f, f,
         "            .github/actions/last-run-base\n", "",
         1, ("sparse checkout does not include",))
COOP = ("coop: ${{ (startsWith(github.ref, 'refs/tags/') || "
        "github.event_name == 'push' && steps.base.outputs.base == '') "
        "&& 'true' || steps.filter.outputs.coop }}")
case("ci.yml: coop output loses its fail-open arms", "ci.yml", COOP,
     "coop: ${{ steps.filter.outputs.coop }}",
     1, ("output `coop` is '' on a tag push when",
         "it must fail open"))
case("ci.yml: coop output only fails open on a branch push", "ci.yml", COOP,
     "coop: ${{ github.ref == 'refs/heads/main' && "
     "steps.base.outputs.base == '' && 'true' || steps.filter.outputs.coop }}",
     1, ("output `coop` is '' on a tag push when",))
case("ci.yml: lands output forced true even with a base", "ci.yml",
     "lands_conformance: ${{ (startsWith(github.ref, 'refs/tags/') || "
     "github.event_name == 'push' && steps.base.outputs.base == '') && "
     "'true' || steps.filter.outputs.lands_conformance }}",
     "lands_conformance: ${{ github.event_name == 'push' && 'true' || "
     "steps.filter.outputs.lands_conformance }}",
     1, ("output `lands_conformance` is 'true' on a push with a base",
         "pass the filter's answer"))
FILTER_IF = {"ci.yml": "        if: ${{ !startsWith(github.ref, 'refs/tags/') "
                       "|| steps.tagbase.outputs.base != '' }}\n",
             "checks.yml": "        if: ${{ !startsWith(github.ref, "
                           "'refs/tags/') }}\n"}
for f in ("ci.yml", "checks.yml"):
    case("%s: paths-filter skipped on every event" % f, f, FILTER_IF[f],
         "        if: ${{ startsWith(github.ref, 'refs/tags/') }}\n",
         1, ("the paths-filter step's `if:`", "is false on a pull_request"))
case("checks.yml: BASE_SHA back on github.event.before", "checks.yml",
     "github.event.pull_request.base.sha || steps.base.outputs.base }}",
     "github.event.pull_request.base.sha || github.event.before }}",
     1, ("never github.event.before",))

# ── the browser suite's tag exception (gd-rmHsVjmKse) ────────────────────────
BROWSER = ("gardenhub_browser: ${{ startsWith(github.ref, 'refs/tags/') && "
           "(steps.tagbase.outputs.base != '' && steps.filter.outcome == "
           "'success' && steps.filter.outputs.gardenhub_browser == 'false' && "
           "'false' || 'true') || (github.event_name == 'push' && "
           "steps.base.outputs.base == '') && 'true' || "
           "steps.filter.outputs.gardenhub_browser }}")
case("ci.yml: browser output forced on every tag again", "ci.yml", BROWSER,
     "gardenhub_browser: ${{ (startsWith(github.ref, 'refs/tags/') || "
     "github.event_name == 'push' && steps.base.outputs.base == '') && "
     "'true' || steps.filter.outputs.gardenhub_browser }}",
     1, ("output `gardenhub_browser` is 'true' on a tag push with a tag base",
         "the browser suite's tag exception is gone"))
case("ci.yml: browser output ignores a failed filter", "ci.yml", BROWSER,
     BROWSER.replace("steps.filter.outcome == 'success' && ", ""),
     1, ("output `gardenhub_browser` is 'false' on a tag push whose filter "
         "failed", "it must fail open"))
case("ci.yml: browser output skips with no tag base", "ci.yml", BROWSER,
     BROWSER.replace("steps.tagbase.outputs.base != '' && ", ""),
     1, ("output `gardenhub_browser` is 'false' on a tag push when",
         "it must fail open"))
case("ci.yml: coop takes the browser's tag exception", "ci.yml", COOP,
     BROWSER.replace("gardenhub_browser", "coop"),
     1, ("output `coop` is 'false' on a tag push with a tag base",))
case("ci.yml: browser output removed", "ci.yml", "      " + BROWSER + "\n",
     "", 1, ("no `gardenhub_browser` output reading steps.filter",))
case("ci.yml: last-tag-base step without continue-on-error", "ci.yml",
     "      - id: tagbase\n        continue-on-error: true\n",
     "      - id: tagbase\n",
     1, ("last-tag-base step must be `continue-on-error: true`",))
case("ci.yml: last-tag-base step removed", "ci.yml",
     "      - id: tagbase\n        continue-on-error: true\n"
     "        uses: ./.github/actions/last-tag-base\n", "",
     1, ("exactly one step using ./.github/actions/last-tag-base",))
case("ci.yml: last-tag-base missing from the sparse checkout", "ci.yml",
     "            .github/actions/last-tag-base\n", "",
     1, ("sparse checkout does not include .github/actions/last-tag-base",))
case("ci.yml: paths-filter base drops the tag base", "ci.yml",
     FILTER_BASE["ci.yml"], FILTER_BASE["checks.yml"],
     1, ("`base:` gave '' on a tag push with a tag base",))
case("ci.yml: paths-filter still skipped on every tag", "ci.yml",
     FILTER_IF["ci.yml"], FILTER_IF["checks.yml"],
     1, ("is false on a tag push with a tag base",))
FILTER_COE = ("        continue-on-error: ${{ startsWith(github.ref, "
              "'refs/tags/') }}\n")
case("ci.yml: paths-filter not continue-on-error on a tag", "ci.yml",
     FILTER_COE, "", 1, ("must be continue-on-error on a tag push",))
case("ci.yml: paths-filter continue-on-error on every event", "ci.yml",
     FILTER_COE, "        continue-on-error: true\n",
     1, ("continue-on-error on a branch push or a pull_request",))
PASSED = "const PASSED = ['success'];"
case("tag action: a skipped suite accepted as a base", TAG_ACTION, PASSED,
     "const PASSED = ['success', 'skipped'];", 1, ("PASSED holds skipped",))
case("tag action: PASSED loses 'success'", TAG_ACTION, PASSED,
     "const PASSED = [];", 1, ("PASSED lacks 'success'",))
case("tag action: back to a deny-list", TAG_ACTION,
     "PASSED.includes(job.conclusion)",
     "!['failure'].includes(job.conclusion)",
     1, ("must decide on `PASSED.includes(job.conclusion)`",))
case("tag action: job name drifts from ci.yml", TAG_ACTION,
     "const JOB = 'gardenhub browser (chromium)';",
     "const JOB = 'gardenhub browser';",
     1, ("`const JOB` is ['gardenhub browser']",))
case("tag action: reads the wrong workflow", TAG_ACTION,
     "const WORKFLOW = 'ci.yml';", "const WORKFLOW = 'checks.yml';",
     1, ("`const WORKFLOW` is ['checks.yml']",))

# ── which earlier run may be a base ──────────────────────────────────────────
RAN = "const RAN = ['success'];"
case("action: startup_failure accepted as a base", ACTION, RAN,
     "const RAN = ['success', 'startup_failure'];",
     1, ("RAN holds startup_failure",))
case("action: cancelled accepted as a base", ACTION, RAN,
     "const RAN = ['success', 'cancelled'];",
     1, ("RAN holds cancelled",))
case("action: a red run accepted as a base", ACTION, RAN,
     "const RAN = ['success', 'failure'];",
     1, ("RAN holds failure",))
case("action: a queued run counts as running", ACTION,
     "const RUNNING = ['in_progress'];",
     "const RUNNING = ['in_progress', 'queued'];",
     1, ("RUNNING holds queued",))
case("action: RAN loses 'success'", ACTION, RAN, "const RAN = [];",
     1, ("RAN lacks 'success'",))
case("action: back to a deny-list", ACTION,
     "? RAN.includes(cand.conclusion)",
     "? !['cancelled', 'skipped'].includes(cand.conclusion)",
     1, ("does not test `RAN.includes(cand.conclusion)`",))

# ── the action's script, run ─────────────────────────────────────────────────
# Mocks for actions/github-script: our run is #10 at sha "us"; `earlier` are
# the older push runs, newest first; every older sha is an ancestor.
HARNESS = r"""
const [script, cases] = [process.argv[1], JSON.parse(process.argv[2])];
const AsyncFunction = Object.getPrototypeOf(async () => {}).constructor;
(async () => {
  const out = [];
  for (const c of cases) {
    let base = null;
    const core = { setOutput: (k, v) => { base = v; }, info: () => {} };
    const runs = [{ id: 1, run_number: 10, status: 'in_progress',
                    conclusion: null, head_sha: 'us' }]
      .concat(c.earlier.map((r, i) => ({ id: 100 + i, run_number: 9 - i,
                                         head_sha: 's' + (9 - i), ...r })));
    const github = { rest: {
      actions: { listWorkflowRuns: async () => ({ data: { workflow_runs: runs } }) },
      repos: { compareCommitsWithBasehead: async () =>
        ({ data: { status: 'ahead', total_commits: 1 } }) },
    } };
    const context = { eventName: 'push', ref: c.ref || 'refs/heads/main', sha: 'us',
                      runId: 1, repo: { owner: 'o', repo: 'r' } };
    process.env.WORKFLOW = 'ci.yml';
    await new AsyncFunction('github', 'context', 'core', script)(github, context, core);
    out.push(base);
  }
  console.log(JSON.stringify(out));
})().catch((e) => { console.error(e); process.exit(1); });
"""
DONE = lambda c: {"status": "completed", "conclusion": c}
SELECTION = [
    ("the previous run succeeded", [DONE("success")], "s9"),
    # gd-NkRBUlh9Ar: push 9 broke the chart, so its run failed `helm`; ours
    # is a docs-only push. It must diff from 8, the last green run, so its
    # `helm` gate sees 9's change and fails too. Based on 9 it would skip
    # `helm`, go green and be the commit release-cut.sh tags.
    ("a red run, then a docs-only push: it diffs from the last green run",
     [DONE("failure"), DONE("success")], "s8"),
    # ...and that docs-only push went red as well (it re-ran `helm`), so
    # the next push still reaches back past both.
    ("a red run and the red docs-only push after it: both stepped over",
     [DONE("failure"), DONE("failure"), DONE("success")], "s7"),
    ("main red for several pushes: every red run stepped over",
     [DONE("failure"), DONE("failure"), DONE("cancelled"), DONE("success")],
     "s6"),
    ("startup_failure is stepped over to the run before it",
     [DONE("startup_failure"), DONE("success")], "s8"),
    ("a cancelled (evicted) run is stepped over",
     [DONE("cancelled"), DONE("success")], "s8"),
    ("timed_out, skipped, stale, action_required, neutral stepped over",
     [DONE(c) for c in ("timed_out", "skipped", "stale", "action_required",
                        "neutral")] + [DONE("success")], "s4"),
    ("a conclusion GitHub adds later is stepped over (fails toward wider)",
     [DONE("some_new_conclusion"), DONE("success")], "s8"),
    ("an in_progress run counts",
     [{"status": "in_progress", "conclusion": None}], "s9"),
    ("a queued / pending / waiting run is stepped over",
     [{"status": s, "conclusion": None} for s in ("queued", "pending",
                                                  "waiting")]
     + [DONE("success")], "s6"),
    ("only startup_failure runs earlier: fail open (empty base)",
     [DONE("startup_failure"), DONE("startup_failure")], ""),
    ("only red runs earlier: fail open (empty base)",
     [DONE("failure"), DONE("failure")], ""),
]
# A release tag push (owner 2026-11-03): not a branch, so no base at all, even
# with a green run just before it; the callers then open every path gate.
TAG_SELECTION = [
    ("a tag push gets no base: fail open", [DONE("success")], "",
     "refs/tags/2026.307.1"),
]


def find_node():
    # The ARC runner image bundles node for JS actions but not on PATH, next
    # to musl builds (node24_alpine) that cannot exec on its glibc, so each
    # candidate must actually run.
    cands = [shutil.which("node")] + sorted(
        (n for n in glob.glob("/home/runner/externals/node*/bin/node")
         if "alpine" not in n), reverse=True)
    for n in filter(None, cands):
        try:
            if subprocess.run([n, "--version"], capture_output=True,
                              timeout=30).returncode == 0:
                return n
        except (OSError, subprocess.TimeoutExpired):
            pass
    return None


node = find_node()
if node is None:
    print("SKIP action script run: no node on PATH or in the runner's "
          "externals; the accept-list checks above still ran")
else:
    import importlib.util
    spec = importlib.util.spec_from_file_location("guard", GUARD)
    guard = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(guard)
    script = guard.action_script(REAL[ACTION])
    cases = [{"earlier": e} for _, e, _ in SELECTION] + \
        [{"earlier": e, "ref": r} for _, e, _, r in TAG_SELECTION]
    try:
        p = subprocess.run([node, "-e", HARNESS, script, json.dumps(cases)],
                           capture_output=True, text=True)
    except OSError as e:
        p = None
        print("FAIL action script run: cannot exec %s (%s)" % (node, e))
        FAIL += 1
    if p is None:
        pass
    elif p.returncode != 0:
        print("FAIL action script run: rc=%d\n%s" % (p.returncode,
                                                     p.stdout + p.stderr))
        FAIL += 1
    else:
        got = json.loads(p.stdout.strip().split("\n")[-1])
        named = [(n, w) for n, _, w in SELECTION] + \
            [(n, w) for n, _, w, _ in TAG_SELECTION]
        for (name, want), base in zip(named, got):
            if base == want:
                print("ok   action run: %s" % name)
                PASS += 1
            else:
                print("FAIL action run: %s: base=%r, want %r"
                      % (name, base, want))
                FAIL += 1

# ── the tag action's script, run ─────────────────────────────────────────────
# Mocks: our tag is c.ref (default 2026.308.12) at sha "us". `runs` are ci.yml
# push runs {tag, browser: <job conclusion or None for no such job>}, in any
# order; run i has id 100+i, run_number 100+i and head_sha "s-<tag>".
TAG_HARNESS = r"""
const [script, cases] = [process.argv[1], JSON.parse(process.argv[2])];
const AsyncFunction = Object.getPrototypeOf(async () => {}).constructor;
(async () => {
  const out = [];
  for (const c of cases) {
    const got = {};
    const core = { setOutput: (k, v) => { got[k] = v; }, info: () => {} };
    const runs = c.runs.map((r, i) => ({ id: 100 + i, run_number: 100 + i,
      head_branch: r.tag, head_sha: 's-' + r.tag, status: 'completed' }));
    const jobs = (id) => {
      const b = c.runs[id - 100].browser;
      return [{ name: 'build + test (go)', conclusion: 'success' }]
        .concat(b === null ? [] : [{ name: 'gardenhub browser (chromium)', conclusion: b }]);
    };
    const fail = (what) => async () => { throw new Error(what + ' failed'); };
    const github = {
      paginate: c.jobsError ? fail('jobs') : async (fn, args) => jobs(args.run_id),
      rest: {
        actions: {
          listWorkflowRuns: c.listError ? fail('list')
            : async () => ({ data: { workflow_runs: runs } }),
          listJobsForWorkflowRun: () => {},
        },
        repos: { compareCommitsWithBasehead: async () =>
          ({ data: { status: c.compare || 'ahead', total_commits: 3 } }) },
      } };
    const context = { eventName: c.event || 'push',
                      ref: c.ref || 'refs/tags/2026.308.12', sha: 'us',
                      runId: 1, repo: { owner: 'o', repo: 'r' } };
    await new AsyncFunction('github', 'context', 'core', script)(github, context, core);
    out.push([got.base, got.tag]);
  }
  console.log(JSON.stringify(out));
})().catch((e) => { console.error(e); process.exit(1); });
"""
T = lambda tag, browser: {"tag": tag, "browser": browser}
TAG_BASE = [
    ("the previous tag's suite passed", {"runs": [T("2026.308.11", "success")]},
     ["s-2026.308.11", "2026.308.11"]),
    # Tag 11 skipped the suite because 10 passed; we must diff from 10, so
    # 11's own changes are inside our diff.
    ("a skipped suite is stepped over to the tag whose suite passed",
     {"runs": [T("2026.308.11", "skipped"), T("2026.308.10", "success")]},
     ["s-2026.308.10", "2026.308.10"]),
    ("a red suite is stepped over",
     {"runs": [T("2026.308.11", "failure"), T("2026.308.10", "success")]},
     ["s-2026.308.10", "2026.308.10"]),
    ("cancelled, timed_out and a run with no browser job are stepped over",
     {"runs": [T("2026.308.11", "cancelled"), T("2026.308.10", "timed_out"),
               T("2026.308.9", None), T("2026.308.8", "success")]},
     ["s-2026.308.8", "2026.308.8"]),
    ("calver order is numeric: 308.10 is newer than 308.9",
     {"runs": [T("2026.308.9", "success"), T("2026.308.10", "success")]},
     ["s-2026.308.10", "2026.308.10"]),
    ("our own and later tags are never a base",
     {"runs": [T("2026.308.12", "success"), T("2026.309.1", "success"),
               T("2026.308.7", "success")]},
     ["s-2026.308.7", "2026.308.7"]),
    ("a non-calver run head is ignored",
     {"runs": [T("main", "success"), T("v1.2.3", "success"),
               T("2026.307.4", "success")]},
     ["s-2026.307.4", "2026.307.4"]),
    ("the same commit tagged again: identical counts",
     {"runs": [T("2026.308.11", "success")], "compare": "identical"},
     ["s-2026.308.11", "2026.308.11"]),
    ("no earlier tag whose suite passed: fail open",
     {"runs": [T("2026.308.11", "skipped"), T("2026.308.10", "failure")]},
     ["", ""]),
    ("no tag runs at all: fail open", {"runs": []}, ["", ""]),
    ("the base is not an ancestor: fail open",
     {"runs": [T("2026.308.11", "success")], "compare": "diverged"},
     ["", ""]),
    ("listing runs fails: fail open",
     {"runs": [T("2026.308.11", "success")], "listError": True}, ["", ""]),
    ("listing jobs fails: fail open",
     {"runs": [T("2026.308.11", "success")], "jobsError": True}, ["", ""]),
    ("a non-calver tag: fail open",
     {"runs": [T("2026.308.11", "success")], "ref": "refs/tags/v2"},
     ["", ""]),
    ("a branch push: no tag base",
     {"runs": [T("2026.308.11", "success")], "ref": "refs/heads/main"},
     ["", ""]),
    ("a dispatch on a tag: no tag base",
     {"runs": [T("2026.308.11", "success")], "event": "workflow_dispatch"},
     ["", ""]),
]
if node is None:
    print("SKIP tag action script run: no node; the PASSED checks above "
          "still ran")
else:
    script = guard.action_script(REAL[TAG_ACTION])
    try:
        p = subprocess.run([node, "-e", TAG_HARNESS, script,
                            json.dumps([c for _, c, _ in TAG_BASE])],
                           capture_output=True, text=True)
    except OSError as e:
        p = None
        print("FAIL tag action script run: cannot exec %s (%s)" % (node, e))
        FAIL += 1
    if p is None:
        pass
    elif p.returncode != 0:
        print("FAIL tag action script run: rc=%d\n%s"
              % (p.returncode, p.stdout + p.stderr))
        FAIL += 1
    else:
        got = json.loads(p.stdout.strip().split("\n")[-1])
        for (name, _, want), base in zip(TAG_BASE, got):
            if base == want:
                print("ok   tag action run: %s" % name)
                PASS += 1
            else:
                print("FAIL tag action run: %s: [base, tag]=%r, want %r"
                      % (name, base, want))
                FAIL += 1

# ── the real tree ────────────────────────────────────────────────────────────
rc, out = run_guard()
if rc == 0 and "check-main-concurrency: OK" in out:
    print("ok   real tree")
    PASS += 1
else:
    print("FAIL real tree: rc=%d\n%s" % (rc, out))
    FAIL += 1

print("\n%d passed, %d failed" % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
