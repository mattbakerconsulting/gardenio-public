#!/usr/bin/env python3
"""check-ci-required-checks.py

Guard the required-check aggregators: the `ci` job in .github/workflows/ci.yml
and the `checks` job in .github/workflows/checks.yml (gd-UqzOTRj6cX).

WHY THIS EXISTS
---------------
A ruleset's required status check matches a CHECK-RUN name, which is a job's
`name:`, not a workflow's. No job was named `ci` or `checks`, so requiring
those on main would have left every PR waiting for a check that never reports.
Requiring the real job names instead would tie the ruleset to every job
rename, matrix leg and path gate. So each workflow has one aggregator job that
needs every other job and fails when any of them did not end `success` or
`skipped`, and the ruleset requires only `ci` and `checks`.

Every way to lose an aggregator fails OPEN, in silence, while the PR still
shows green:

  * a job missing from `needs:` can fail and `ci` still passes;
  * `if: !cancelled()` (or any `if:` that can be false) SKIPS the aggregator
    when a run is cancelled, and a skipped check satisfies a required one;
  * a `name:` that is not exactly the check name on every run. Until Task
    7.3 part B (gd-19ECZxvFQw) a draft run reported `ci (draft)`, because a
    ready_for_review run with the heavy jobs was still to come. Every PR run
    is now the light tier, draft or not, and `gh pr ready` starts no run, so
    a draft-split name would leave a ready PR waiting forever for `ci`;
  * a step that reads the wrong context, or a jq filter that matches nothing,
    passes every run;
  * any other job, in any workflow, that reports a check named `ci` or
    `checks` satisfies the required check on its own when it is green.

WHAT IT CHECKS
--------------
For ci.yml -> job `ci` and checks.yml -> job `checks`:
  1. The job exists.
  2. Its `name:` is the check name, literally or as a single `${{ }}`
     expression that evaluates to it on a push, a schedule, a dispatch and a
     draft and a ready pull_request.
  3. Its `if:` is exactly `always()`.
  4. Its `needs:` names every other job in the workflow, once each, and
     nothing else. `if: false` jobs count: a job turned back on is then
     already covered.
  5. It has exactly one step. That step's env has `NEEDS: ${{ toJSON(needs) }}`
     and `ADVISORY`, a space-separated list of exactly the jobs that carry a
     job-level `continue-on-error` (each of them a need). A job meant only to
     report must not fail the required check; one that is not meant to must
     not hide in that list.
  6. That step's script, EXECUTED with bash and jq on fixture NEEDS: every job
     `success` or `skipped` -> exit 0 (the positive control); one `failure`,
     one `cancelled`, or a result it does not know -> non-zero; `{}` -> non-zero
     (nothing judged is not green); an ADVISORY job's `failure` -> exit 0.
And for every workflow file in .github/workflows/: no job other than the two
aggregators reports a check named `ci` or `checks` (its literal `name:`, the
value of a `name:` expression on a push or a PR, or its id when it has no
`name:`).

Stdlib only, for the same reason as check-ci-draft-gate.py, whose readers and
expression evaluator this reuses: the light runner image has no PyYAML. Needs
`bash` and `jq` on PATH for check 6.

Usage:  python3 scripts/check-ci-required-checks.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-ci-required-checks.py
"""
import importlib.util
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(HERE)
WF_DIR = os.path.join(ROOT, ".github", "workflows")

# workflow -> (aggregator job id, the check name the ruleset requires)
AGGREGATORS = {"ci.yml": ("ci", "ci"), "checks.yml": ("checks", "checks")}
IF = "always()"
NEEDS_ENV = "toJSON(needs)"

# (label, event_name, pull_request.draft) for evaluating a `name:`.
EVENTS = (
    ("push", "push", None),
    ("schedule", "schedule", None),
    ("workflow_dispatch", "workflow_dispatch", None),
    ("ready pull_request", "pull_request", False),
    ("draft pull_request", "pull_request", True),
)


def load_draft_gate():
    path = os.path.join(HERE, "check-ci-draft-gate.py")
    spec = importlib.util.spec_from_file_location("draft_gate", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


DG = load_draft_gate()


def raw_name(body):
    """The `name:` value as written (unquoted), or None."""
    at = DG.find_key(body, "name", 4)
    if at is None:
        return None
    m = DG.KEY.match(body[at][1])
    return DG.unquote((m.group(3) or "").strip())


def whole_expression(raw):
    """The expression inside `${{ ... }}` when it is the whole value."""
    if raw and raw.startswith("${{") and raw.endswith("}}") and \
            raw.count("${{") == 1:
        return " ".join(raw[3:-2].split())
    return None


def evaluate_name(expr, event, draft):
    ctx = {"github.event_name": event,
           "github.event.pull_request.draft": draft}
    return DG.evaluate(DG.parse(expr), ctx)


def run_script(raw_lines, job):
    """The `run: |` script of the job's step, dedented, from the raw file
    (comments and blank lines kept, as bash sees them). None if absent."""
    start = None
    for i, line in enumerate(raw_lines):
        if line.rstrip() == "  %s:" % job:
            start = i
            break
    if start is None:
        return None
    for i in range(start + 1, len(raw_lines)):
        line = raw_lines[i]
        if line.strip() and not line.lstrip().startswith("#") and \
                DG.indent(line) <= 2:
            return None
        if line.strip() in ("run: |", "run: |-"):
            base = DG.indent(line)
            out = []
            for nxt in raw_lines[i + 1:]:
                if nxt.strip() and DG.indent(nxt) <= base:
                    break
                out.append(nxt)
            while out and not out[-1].strip():
                out.pop()
            cut = min(DG.indent(l) for l in out if l.strip())
            return "\n".join(l[cut:] for l in out) + "\n"
    return None


def step_env(body):
    """[(env map, step lines)] for each step of the job."""
    out = []
    for item in DG.steps(body):
        level = item[0][0]
        first = item[0][1].lstrip()[2:].lstrip()
        lines = [(level + 2, " " * (level + 2) + first)] + item[1:]
        at = DG.find_key(lines, "env", level + 2)
        out.append(DG.submap(lines, at) if at is not None else {})
    return out


def run_fixture(script, needs, advisory):
    env = dict(os.environ, NEEDS=json.dumps(needs), ADVISORY=advisory)
    p = subprocess.run(["bash", "-c", script], env=env, capture_output=True,
                       text=True, timeout=60)
    return p.returncode, (p.stdout + p.stderr).strip()


def fixtures(need_ids):
    """(label, NEEDS, ADVISORY, want_zero) for check 6."""
    first = need_ids[0]
    ok = {j: {"result": "success", "outputs": {}} for j in need_ids}
    ok[need_ids[-1]]["result"] = "skipped"
    out = [("every need success or skipped", ok, "", True)]
    for result in ("failure", "cancelled", "neutral"):
        bad = json.loads(json.dumps(ok))
        bad[first]["result"] = result
        out.append(("%s %s" % (first, result), bad, "", False))
    out.append(("no needs at all ({})", {}, "", False))
    adv = json.loads(json.dumps(ok))
    adv[first]["result"] = "failure"
    out.append(("ADVISORY %s failure" % first, adv, first, True))
    out.append(("ADVISORY names another job, %s failure" % first, adv,
                "not-%s" % first, False))
    return out


def check_aggregator(fname, problems):
    """Append problems for one workflow's aggregator; return (needs count,
    fixtures run) for the summary."""
    job, check = AGGREGATORS[fname]
    path = os.path.join(WF_DIR, fname)
    if not os.path.exists(path):
        problems.append("%s: does not exist (looked in %s)" % (fname, WF_DIR))
        return 0, 0
    with open(path) as fh:
        text = fh.read()
    found = DG.jobs(DG.logical_lines(text)) or {}
    where = "%s: %s" % (fname, job)
    spec = found.get(job)
    if spec is None:
        problems.append(
            "%s: no `%s` job. It is the check the main ruleset requires for "
            "this workflow; without it every PR waits for a check that never "
            "reports." % (fname, job))
        return 0, 0
    body = spec["body"]

    # 2. the name, on every run
    raw = raw_name(body)
    expr = whole_expression(raw)
    if expr is None:
        if raw != check:
            problems.append(
                "%s's `name:` is %r. The ruleset requires a check named "
                "exactly %r on every run, a draft PR's included "
                "(gd-19ECZxvFQw); write it so:\n      name: %s"
                % (where, raw, check, check))
    else:
        try:
            for label, event, draft in EVENTS:
                got = evaluate_name(expr, event, draft)
                if got != check:
                    problems.append(
                        "%s's `name:` is %r on a %s run, not %r. The ruleset "
                        "requires exactly %r, and since gd-19ECZxvFQw a "
                        "PR's draft run is the run it reads; write it so:\n"
                        "      name: %s"
                        % (where, got, label, check, check, check))
        except DG.Unreadable as e:
            problems.append("%s's `name:` cannot be evaluated (%s). Write it "
                            "as the literal check name." % (where, e))

    # 3. the if
    if spec["if"] != IF:
        problems.append(
            "%s's `if:` is %r, not `%s`. Anything that can be false SKIPS the "
            "aggregator, and a skipped check satisfies a required one; "
            "`!cancelled()` does exactly that to a cancelled run."
            % (where, spec["if"], IF))

    # 4. needs == every other job
    needs = DG.needs_of(body)
    others = [j for j in found if j != job]
    missing = [j for j in others if j not in needs]
    extra = [j for j in needs if j not in others]
    dup = sorted(set(j for j in needs if needs.count(j) > 1))
    if missing:
        problems.append(
            "%s's `needs:` lacks %s. A job it does not need can fail and `%s` "
            "still passes. Add every job in %s." % (where, ", ".join(missing),
                                                    check, fname))
    if extra:
        problems.append("%s's `needs:` names %s, which %s does not define."
                        % (where, ", ".join(extra), fname))
    if dup:
        problems.append("%s's `needs:` lists %s more than once."
                        % (where, ", ".join(dup)))

    # 5. one step, its env
    envs = step_env(body)
    if len(envs) != 1:
        problems.append("%s has %d steps; it must have exactly one, the "
                        "judgement over `needs`." % (where, len(envs)))
        return len(needs), 0
    env = envs[0]
    if env.get("NEEDS") != NEEDS_ENV:
        problems.append("%s's step must set `NEEDS: ${{ %s }}`; it sets %r."
                        % (where, NEEDS_ENV, env.get("NEEDS")))
    if "ADVISORY" not in env:
        problems.append("%s's step has no `ADVISORY` env (write `ADVISORY: "
                        "\"\"` when no job is advisory)." % where)
    advisory = (env.get("ADVISORY") or "").split()
    coe = []
    for j, s in found.items():
        at = DG.find_key(s["body"], "continue-on-error", 4)
        if at is not None and DG.scalar(s["body"], at) != "false":
            coe.append(j)
    for j in coe:
        if j not in advisory:
            problems.append(
                "%s: %s carries job-level `continue-on-error`, so it is meant "
                "only to report, but the aggregator's ADVISORY does not list "
                "it. List it there, or drop the continue-on-error."
                % (fname, j))
    for j in advisory:
        if j not in coe:
            problems.append(
                "%s's ADVISORY lists %s, which carries no job-level "
                "`continue-on-error`. Its failures are real; drop it from "
                "ADVISORY." % (where, j))
        if j not in needs:
            problems.append("%s's ADVISORY lists %s, which is not a need."
                            % (where, j))

    # 6. the script, executed
    script = run_script(text.split("\n"), job)
    if script is None:
        problems.append("%s's step has no `run: |` block this guard can "
                        "execute." % where)
        return len(needs), 0
    missing_tools = [t for t in ("bash", "jq") if shutil.which(t) is None]
    if missing_tools:
        problems.append("%s: cannot execute the step: %s not on PATH."
                        % (where, ", ".join(missing_tools)))
        return len(needs), 0
    if not needs:
        return 0, 0
    ran = 0
    for label, fixture, adv, want_zero in fixtures(needs):
        rc, out = run_fixture(script, fixture, adv)
        ran += 1
        if (rc == 0) != want_zero:
            problems.append(
                "%s's step exits %d on fixture %r; want %s. Output:\n      %s"
                % (where, rc, label, "0" if want_zero else "non-zero",
                   out.replace("\n", "\n      ")))
    return len(needs), ran


def collisions(problems):
    """Append a problem for every job outside the aggregators that reports a
    check named like a required one; return how many workflows were read."""
    required = {c.lower(): (f, j) for f, (j, c) in AGGREGATORS.items()}
    names = sorted(n for n in os.listdir(WF_DIR)
                   if n.endswith((".yml", ".yaml"))) \
        if os.path.isdir(WF_DIR) else []
    for fname in names:
        with open(os.path.join(WF_DIR, fname)) as fh:
            found = DG.jobs(DG.logical_lines(fh.read())) or {}
        for job_id, spec in found.items():
            if AGGREGATORS.get(fname, (None,))[0] == job_id:
                continue
            raw = raw_name(spec["body"])
            reported = set()
            if raw is None:
                reported.add(job_id)
            elif whole_expression(raw) is None:
                reported.add(raw)
            else:
                for label, event, draft in EVENTS:
                    try:
                        got = evaluate_name(whole_expression(raw), event, draft)
                    except DG.Unreadable:
                        continue  # matrix or inputs: not a fixed name
                    if isinstance(got, str):
                        reported.add(got)
            for name in reported:
                if name.lower() in required:
                    f, j = required[name.lower()]
                    problems.append(
                        "%s: job %s reports a check named %r, the name the "
                        "main ruleset requires from %s's `%s` job. A green %s "
                        "would satisfy that rule on its own; rename it."
                        % (fname, job_id, name, f, j, job_id))
    return len(names)


def main():
    problems = []
    summary = []
    for fname in AGGREGATORS:
        n, ran = check_aggregator(fname, problems)
        summary.append("`%s` needs %d jobs, %d fixtures run"
                       % (AGGREGATORS[fname][1], n, ran))
    read = collisions(problems)

    if problems:
        e = sys.stderr
        print("check-ci-required-checks: FAIL", file=e)
        print("", file=e)
        for p in problems:
            print("  %s" % p, file=e)
        print("", file=e)
        print("See the header of %s for why this matters." % __file__, file=e)
        return 1
    print("check-ci-required-checks: OK (%s; %d workflows hold no other job "
          "named ci or checks)" % ("; ".join(summary), read))
    return 0


if __name__ == "__main__":
    sys.exit(main())
