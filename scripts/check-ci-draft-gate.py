#!/usr/bin/env python3
"""check-ci-draft-gate.py

Guard the pull-request light tier in .github/workflows/{ci,checks}.yml, the
nightly full run of ci.yml and checks.yml that backs it, and the absence of
any pull_request trigger in .github/workflows/images.yml.

WHY THIS EXISTS
---------------
On 2026-10-27, 99 PRs took 218 pushes, and every push ran about 81
runner-minutes of ci, checks and images (gd-WFMvg9sDKE, Phase 1). The first
fix (gd-JGkmjFfv3B) gave DRAFT PRs a light tier -- ci.yml's `draft-light` job
builds, vets and tests the packages the PR touched -- and ran the full suite
once, on `ready_for_review`. The owner's rule of 2026-10-31 (root CLAUDE.md
"CI on pull requests") went further: no big CI run on a pull request at all.
Since Task 7.3 part B (gd-19ECZxvFQw) EVERY PR, draft or ready, runs the
light tier only, and the heavy jobs run on main pushes, the nightly and
dispatch. Main is the proof.

That tier is held up by a few things that are easy to lose in a routine edit,
and losing any one of them fails SILENTLY (the workflow still parses, the jobs
still run, and the only symptom is the CI bill coming back):

1. `on.pull_request.types` is exactly opened, synchronize, reopened (written
   out, or left as GitHub's default, which is that set). `synchronize` gives
   every head a run, so the required checks report on it. `ready_for_review`
   must NOT come back: it is the run that used to lift a PR into the full
   suite, and with the heavy jobs off PRs it would only re-run the light tier
   on a head that already has one. `converted_to_draft` needs no run either.

2. No heavy job can run on a pull_request. This is checked by EVALUATING the
   job's `if:` for every pull_request the guard can model, not by matching
   text: draft or not, every `needs.*.result`, every gate output value, and
   `cancelled()` / `failure()` both ways. The plain form is the conjunct

       github.event_name != 'pull_request'

   ANDed into the `if:`. Push, tag, schedule and dispatch events pass it by
   construction, so tag builds and main pushes are untouched. A job added
   without it runs on every PR push, which is the cost this tier removes.

3. `draft-light` exists in ci.yml, runs on every pull_request whatever its
   draft state, runs on no other event (those run the full suite), and its
   name starts with `light:` so a reader of the checks list can tell it is
   not the full suite. Its id keeps the old name: the peak-memory labels and
   these guards key on it.

4. The nightly full run (gd-2kZxn16TaS). Every gate in this plan decides NOT
   to run something; `on.schedule` in ci.yml and checks.yml is where all of it
   still runs, once a day on main. A scheduled run has nothing to diff, so
   paths-filter reports every filter false on it, exactly as on a dispatch:
   a path gate that lets `workflow_dispatch` through but not `schedule`
   skips its job every night and the nightly reports green over it.
5. The gate jobs' checkout (gd-LjoM6ipXQX). Measured 2026-10-29 (Task 4.1,
   gd-WFMvg9sDKE), a `fetch-depth: 0` checkout was ~78% of what the gates
   cost, ~1,590 of ~2,040 runner-min/day, while the gate work itself took
   1-2 s. A gate needs no history: dorny/paths-filter reads a PR's files
   through the API and fetches a push's `before` commit itself. So every
   `actions/checkout` in a gate job must be a `sparse-checkout` and must not
   set `fetch-depth: 0`. Restoring the full clone breaks nothing, so nothing
   else would catch it.

6. images.yml has no pull_request trigger at all (decision gd-bjj3robkjs,
   gd-NWKY8wZJRt). Once its builds were off PRs, its PR run was only the
   `changes` gate, which fed nothing: `images` is not a required check. That
   was one light pod per PR push, ~130 runner-min/day. A trigger added back
   would still pass every other check here, so its absence is checked.

7. The run-level concurrency key gives each PR one slot, keyed on
   github.ref alone. From gd-gw9mSq9Qtm to part B it was split by draft
   state, because a draft run and a ready_for_review run did different work
   and, with one slot, the draft run cancelled the ready one on PR #1503.
   Every PR run is now the same light tier, so a draft term would only let a
   superseded run of the same head keep running beside its successor.

WHAT IT CHECKS
--------------
For images.yml (NO_PR_WORKFLOWS): `on:` exists, lists at least one trigger,
and lists neither `pull_request` nor `pull_request_target`. Nothing below
applies to it.

For ci.yml and checks.yml:
  * `pull_request.types` is exactly PR_TYPES, in any order, or absent (the
    default is that set).
  * Every job except that workflow's gate jobs (GATE_JOBS: ci.yml `gate` and
    `draft-light`; checks.yml `supersession` and `changes`), its required-check
    aggregator (AGGREGATE_JOBS: ci.yml `ci`, checks.yml `checks`, guarded by
    check-ci-required-checks.py) and jobs with a literal `if: false` has a
    job-level `if:` that EVALUATES false on every modelled pull_request (see
    pr_runs below). An `if:` the evaluator cannot read is reported, not
    passed.
  * At least one gated job was found -- a parser that reads nothing must not
    report success.
  * Every job named in GATE_JOBS exists, so a renamed gate cannot leave a
    stale exemption behind.
  * Every `actions/checkout` step in a gate job other than `draft-light`
    (which diffs against the merge base and needs history) sets
    `sparse-checkout` and does not set `fetch-depth: 0`. A gate with no
    checkout at all passes.
  * The top-level `concurrency.group` contains the pull_request arm written
    exactly so, and does not read the draft state anywhere:
        github.event_name == 'pull_request' && github.ref ||
And for ci.yml: `draft-light` exists, its `name:` starts with `light:`, its
`if:` evaluates true on a draft and on a ready pull_request (gate green, not
superseded) and false on a push, a schedule and a dispatch, and
`timeout-minutes` is at most 15.
And for ci.yml and checks.yml (the nightly):
  * `on.schedule` exists and lists at least one `cron:`.
  * Every heavy job's `if:` EVALUATES true for a scheduled run, for every
    value each gate output it reads could take ('true', 'false', '0', '').
    A small evaluator for the expression subset these files use does this;
    an expression it cannot read is reported, not passed. The model of a
    scheduled run: event_name 'schedule' on refs/heads/main, no event
    payload, every `needs.*.result` 'success' (the gate worked and said
    "nothing changed", the case that skips), and any output named
    `superseded` 'false' -- .github/actions/superseded answers false for
    every event but push.
  * Every heavy job's `if:` EVALUATES true for a release tag push
    (owner 2026-11-03: ci.yml and checks.yml run on the calver tag, not on
    main), under the same model with event_name 'push' on
    refs/tags/2026.307.1: the tag's run is the one release-deploy.sh rolls on.
    The one exception is TAG_GATED (gd-rmHsVjmKse): ci.yml's
    gardenhub-browser may skip a tag on its own gate output, which is 'false'
    there only when no UI path changed since the last tag whose suite passed
    (scripts/check-main-concurrency.py proves that output fails open). It
    must still run on a tag whenever that output is 'true' and whenever the
    gate job did not succeed, under every value of the other outputs.
  * Outside `on:` and the job-level `if:`s (which the evaluator proves),
    every line that mentions `workflow_dispatch` also mentions `schedule`.
    That reaches what the `if:` evaluator cannot: the
    shell in the gate steps that force every arm, and concurrency keys
    (a nightly keyed on github.ref would share main pushes' slot and could
    be evicted from it while pending).

SUPERSESSION (checks.yml gd-BCyDxqofKe, ci.yml gd-025MCE7PhK)
---------------------------------------------------------------
ci.yml and checks.yml skip a main push that a newer push already covers.
The shape is easy to lose step by step, and every loss fails open in silence
-- the run stays green and simply stops saving anything, or worse, a
path-gated job defers to a run that never runs it and a change goes
unchecked. So for both this also checks:
  * a gate job (checks.yml `supersession`, named `superseded by a newer
    push?`, the name release-cut.sh keyed on until 2026-11-03; ci.yml `gate`) on
    `gardenio-k3s-light` that calls ./.github/actions/superseded with
    `workflow:` naming its own file (ci.yml may leave it out: the composite
    defaults to ci.yml, so in checks.yml a missing `workflow:` would compare
    against the wrong workflow's runs) and no `job:`, followed by a step named
    `superseded -- run-wide skip` whose `if:` is exactly
    `steps.<composite id>.outputs.superseded == 'true'`. release-deploy.sh
    refuses a ci.yml run in which that step succeeded: the gate fired, so the
    ungated jobs never ran, even if a path-gated job built and left a
    sentinel `success`;
  * every heavy job (one with no `if:` at all counts as ungated) that is
    NOT path-gated needs the gate job and ANDs both `!cancelled()` and the
    run-wide verdict (`needs.supersession.outputs.superseded != 'true'`, in
    ci.yml `needs.gate.outputs.superseded != 'true'`) into its `if:`.
    PATH-GATED means: in checks.yml, the `if:` reads `needs.changes.` at all;
    in ci.yml, whose `gate` also carries the verdict, the `if:` evaluates
    false on a main push for some value of the gate outputs it reads (so
    `go`, gated on pull_request only, is not, and rust is);
  * every PATH-GATED heavy job does NOT read that run-wide verdict at all: a
    newer run need not run a path-gated job, so deferring to it could leave a
    change unchecked (the header of checks.yml's `supersession` job, and the
    note on ci.yml's `rust` job);
  * every heavy job has `actions: read`, and its steps are: one or more
    `actions/checkout`, then the composite with `id: superseded`, its own
    `workflow:` and `job:` equal to the job's own `name:` (or its id when it
    has none, as GitHub names it) -- except that an ungated ci.yml job may
    leave `job:` out, since every newer run runs it -- then
    every later step except `guard decision` ANDs
    `steps.superseded.outputs.superseded != 'true'` into its `if:`, and the
    first such step is the `heavy work starting` sentinel with exactly that
    `if:`. An unguarded step would quietly rebuild on superseded runs and
    leave the sentinel claiming nothing ran.

A stdlib line scanner, not PyYAML: the `gardenio-k3s-light` runner image
ships python3 without PyYAML and checks.yml has no pip step (`import yaml`
has killed a guard here before). The reader is strict: anything it cannot
find is reported, never assumed.

Usage:  python3 scripts/check-ci-draft-gate.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-ci-draft-gate.py
"""
import itertools
import json
import os
import re
import sys

WORKFLOWS = ("ci.yml", "checks.yml", "images.yml")
# Workflows that must not run on a pull request at all (header item 6).
NO_PR_WORKFLOWS = ("images.yml",)
NO_PR_TRIGGERS = ("pull_request", "pull_request_target")
CONJUNCT = "github.event_name != 'pull_request'"
LIGHT_JOB = "draft-light"
# Jobs exempt from the no-heavy-job-on-a-PR rule, per workflow. ci.yml's `gate` merged
# the old `supersession` and `changes` jobs (gd-LjoM6ipXQX).
GATE_JOBS = {
    "ci.yml": ("gate", LIGHT_JOB),
    "checks.yml": ("supersession", "changes"),
}
# The required-check aggregators (gd-UqzOTRj6cX), per workflow: `if: always()`
# and no heavy work, so neither the PR rule nor the supersession shape
# applies. scripts/check-ci-required-checks.py guards their own shape.
AGGREGATE_JOBS = {
    "ci.yml": ("ci",),
    "checks.yml": ("checks",),
}
# Gate jobs that may clone history: draft-light runs `git diff base...HEAD`.
FULL_CLONE_OK = frozenset((LIGHT_JOB,))
CHECKOUT = re.compile(r"^\s*(?:-\s+)?uses:\s*['\"]?actions/checkout@")
LIGHT_WORKFLOW = "ci.yml"
LIGHT_NAME_PREFIX = "light:"
LIGHT_TIMEOUT_MAX = 15
# on.pull_request.types, exactly (GitHub's default when `types:` is absent).
PR_TYPES = ("opened", "synchronize", "reopened")
PR_GROUP_ARM = "github.event_name == 'pull_request' && github.ref ||"
DRAFT_REF = "github.event.pull_request.draft"
SUPERSESSION_WORKFLOWS = ("ci.yml", "checks.yml")
# The job whose composite call is the run-wide verdict, per workflow. ci.yml's
# `gate` also computes the path outputs (gd-LjoM6ipXQX).
SUPERSESSION_JOB = {"ci.yml": "gate", "checks.yml": "supersession"}
SUPERSESSION_ACTION = "./.github/actions/superseded"
SUPERSESSION_RUNS_ON = "gardenio-k3s-light"
# checks.yml only. gardenio/scripts/release-cut.sh keyed on this name
# (SUPERSESSION_GATE_JOB) until 2026-11-03, when it stopped reading CI; still
# pinned so a run's jobs read the same across that change. Nothing in the
# repo reads it today.
SUPERSESSION_NAME = {"checks.yml": "superseded by a newer push?"}
# gardenio/scripts/release-deploy.sh keys on this step of that job
# (SUPERSESSION_FIRED_STEP there) for ci.yml; pinned in both.
SUPERSESSION_FIRED = "superseded -- run-wide skip"
RUN_WIDE = {"ci.yml": "needs.gate.outputs.superseded != 'true'",
            "checks.yml": "needs.supersession.outputs.superseded != 'true'"}
STEP_GUARD = "steps.superseded.outputs.superseded != 'true'"
NOT_CANCELLED = "!cancelled()"
SENTINEL = "heavy work starting"
DECISION = "guard decision"
# checks.yml: a job is path-gated if its `if:` reads its path gate at all,
# which would count a PR-only gate too. ci.yml's `gate` also carries the
# run-wide verdict, so there a job is path-gated if its `if:` can be false on
# a main push (push_skips below); a PR-only conjunct leaves it on the
# run-wide verdict.
PATH_GATED_MARK = "needs.changes."
SCHEDULE_WORKFLOWS = ("ci.yml", "checks.yml")
# Every value a gate output can take. 'false' and '0' are the gate saying
# "nothing changed"; '' is an errored gate; 'true' catches an inverted test.
OUTPUT_VALUES = ("true", "false", "0", "")
MAX_OUTPUTS = 6  # 4**6 evaluations; more than any job here reads

ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))
WF_DIR = os.path.join(ROOT, ".github", "workflows")

KEY = re.compile(r"^(\s*)([A-Za-z0-9_.-]+):(?:\s+(.*?))?\s*$")
ITEM = re.compile(r"^\s*-\s+(.*?)\s*$")


def indent(line):
    return len(line) - len(line.lstrip(" "))


def strip_comment(line):
    """Drop a trailing `# comment` that is not inside quotes."""
    q = None
    for i, ch in enumerate(line):
        if q:
            if ch == q:
                q = None
        elif ch in "'\"":
            q = ch
        elif ch == "#" and (i == 0 or line[i - 1] in " \t"):
            return line[:i].rstrip()
    return line.rstrip()


def logical_lines(text):
    """(indent, stripped-of-comments) for every non-blank, non-comment line.
    Blank and comment-only lines are dropped so block scalars and nested maps
    can be walked by indent alone."""
    out = []
    for raw in text.split("\n"):
        line = strip_comment(raw.rstrip("\r"))
        if not line.strip():
            continue
        out.append((indent(line), line))
    return out


def unquote(s):
    s = s.strip()
    if len(s) >= 2 and s[0] == s[-1] and s[0] in "'\"":
        body = s[1:-1]
        return body.replace("''", "'") if s[0] == "'" else body
    return s


def block(lines, i):
    """Lines nested under lines[i]: everything that follows with a strictly
    greater indent, up to the first line at or below it."""
    base = lines[i][0]
    j = i + 1
    while j < len(lines) and lines[j][0] > base:
        j += 1
    return lines[i + 1:j]


def scalar(lines, i):
    """The value of the key at lines[i], joining a block scalar (`>-`, `|`,
    `>`, `|-`, `|+`, `>+`) into one whitespace-normalised string. A `${{ ... }}`
    wrapper around the whole value is removed so `if: ${{ a && b }}` and
    `if: a && b` compare equal."""
    m = KEY.match(lines[i][1])
    value = (m.group(3) or "").strip() if m else ""
    if value in ("|", ">", "|-", ">-", "|+", ">+"):
        value = " ".join(l.strip() for _, l in block(lines, i))
    value = unquote(value)
    text = " ".join(value.split())
    if text.startswith("${{") and text.endswith("}}"):
        text = " ".join(text[3:-2].split())
    return text


def find_key(lines, name, at_indent):
    """Index of the first line that is `name:` at exactly at_indent."""
    for i, (ind, line) in enumerate(lines):
        if ind != at_indent:
            continue
        m = KEY.match(line)
        if m and m.group(2) == name:
            return i
    return None


def pull_request_types(lines):
    """Return (types or None, problem or None) for on.pull_request.types."""
    on = find_key(lines, "on", 0)
    if on is None:
        return None, "no top-level `on:` block"
    on_lines = block(lines, on)
    pr = find_key(on_lines, "pull_request", 2)
    if pr is None:
        return None, "no `pull_request:` trigger under `on:`"
    m = KEY.match(on_lines[pr][1])
    if (m.group(3) or "").strip():
        return None, ("`pull_request:` has an inline value %r this guard cannot "
                      "read; write it as a nested block" % m.group(3))
    pr_lines = block(on_lines, pr)
    ty = find_key(pr_lines, "types", 4)
    if ty is None:
        return list(PR_TYPES), None  # GitHub's default set
    m = KEY.match(pr_lines[ty][1])
    inline = (m.group(3) or "").strip()
    if inline:
        if not (inline.startswith("[") and inline.endswith("]")):
            return None, "`pull_request.types` is %r, not a list" % inline
        return [unquote(t) for t in inline[1:-1].split(",") if t.strip()], None
    items = []
    for _, line in block(pr_lines, ty):
        mi = ITEM.match(line)
        if mi:
            items.append(unquote(mi.group(1)))
    if not items:
        return None, "`pull_request.types:` is an empty list"
    return items, None


def concurrency_group(lines):
    """Return (group or None, problem or None) for the top-level
    concurrency.group."""
    top = find_key(lines, "concurrency", 0)
    if top is None:
        return None, "no top-level `concurrency:` block"
    body = block(lines, top)
    at = find_key(body, "group", 2)
    if at is None:
        return None, "top-level `concurrency:` has no `group:`"
    return scalar(body, at), None


def jobs(lines):
    """{job_id: {'name': str|None, 'if': str|None, 'timeout': str|None,
    'body': [(indent, line)]}} for every job under the top-level `jobs:` key,
    in file order."""
    top = find_key(lines, "jobs", 0)
    if top is None:
        return None
    out = {}
    job_lines = block(lines, top)
    i = 0
    while i < len(job_lines):
        ind, line = job_lines[i]
        m = KEY.match(line)
        if ind == 2 and m:
            body = block(job_lines, i)
            spec = {"name": None, "if": None, "timeout": None, "body": body}
            for k, field in (("name", "name"), ("if", "if"),
                             ("timeout-minutes", "timeout")):
                at = find_key(body, k, 4)
                if at is not None:
                    spec[field] = scalar(body, at)
            out[m.group(2)] = spec
            i += 1 + len(body)
        else:
            i += 1
    return out


def steps(body):
    """The job's steps, each as its list of (indent, line), split on the
    `- ` items directly under `steps:`."""
    at = find_key(body, "steps", 4)
    if at is None:
        return []
    out = []
    for ind, line in block(body, at):
        if ITEM.match(line) and (not out or ind <= out[-1][0][0]):
            out.append([(ind, line)])
        elif out:
            out[-1].append((ind, line))
    return out


def exempt(fname):
    """Job ids in fname that carry neither the draft conjunct nor the
    supersession shape: its gate jobs and its required-check aggregator."""
    return GATE_JOBS.get(fname, ()) + AGGREGATE_JOBS.get(fname, ())


def checkout_problems(fname, job_id, body):
    """Problems with each actions/checkout step of a gate job."""
    problems = []
    for n, step in enumerate(steps(body), 1):
        if not any(CHECKOUT.match(line) for _, line in step):
            continue
        keys = {}
        for _, line in step:
            m = KEY.match(line.replace("- ", "", 1) if ITEM.match(line) else line)
            if m:
                keys[m.group(2)] = unquote(m.group(3) or "")
        where = "%s: %s's checkout (step %d)" % (fname, job_id, n)
        if keys.get("fetch-depth") == "0":
            problems.append(
                "%s sets `fetch-depth: 0`. A gate needs no history: "
                "paths-filter reads a PR's files through the API and fetches a "
                "push's `before` itself. The full clone was ~78%% of gate cost "
                "(gd-LjoM6ipXQX); drop it." % where)
        if "sparse-checkout" not in keys:
            problems.append(
                "%s is not a `sparse-checkout`. Check out only the action or "
                "script the gate runs, with `sparse-checkout-cone-mode: false` "
                "(see ci.yml's `gate` job)." % where)
    return problems


def anded(cond, term=CONJUNCT):
    """Is term present in cond as a top-level AND term? The token before
    it must be `&&` or nothing, and the token after it `&&` or nothing."""
    at = cond.find(term)
    if at == -1:
        return "missing"
    before = cond[:at].rstrip()
    after = cond[at + len(term):].lstrip()
    if (before and not before.endswith("&&")) or \
            (after and not after.startswith("&&")):
        return "ored"
    return "ok"


class Unreadable(Exception):
    """An `if:` uses something the evaluator below does not model."""


TOKEN = re.compile(r"""\s*(?:
    (?P<op>&&|\|\||==|!=|<=|>=|<|>|!|\(|\)|,)
  | '(?P<str>(?:[^']|'')*)'
  | (?P<num>-?\d+(?:\.\d+)?)\b
  | (?P<name>[A-Za-z_][A-Za-z0-9_-]*(?:\.[A-Za-z_*][A-Za-z0-9_-]*)*)
)""", re.X)


# `needs['controller-base'].result` is `needs.controller-base.result`: GitHub
# allows the index form for ids a property dereference would misread.
INDEX = re.compile(r"(?<=[A-Za-z0-9_\]])\[\s*'([A-Za-z0-9_-]+)'\s*\]")


def tokenize(expr):
    expr = INDEX.sub(r".\1", expr)
    out, i = [], 0
    while i < len(expr):
        if expr[i:].strip() == "":
            break
        m = TOKEN.match(expr, i)
        if not m:
            raise Unreadable("cannot tokenize at %r" % expr[i:i + 20])
        if m.group("op"):
            out.append(("op", m.group("op")))
        elif m.group("str") is not None:
            out.append(("lit", m.group("str").replace("''", "'")))
        elif m.group("num"):
            out.append(("lit", float(m.group("num"))))
        else:
            out.append(("name", m.group("name")))
        i = m.end()
    return out


def parse(expr):
    """Parse a GitHub Actions expression into a tuple tree. Precedence, low to
    high: ||, &&, == !=, unary !. Relational operators are refused, not
    guessed at: nothing in these workflows uses them."""
    toks = tokenize(expr)
    pos = [0]

    def peek():
        return toks[pos[0]] if pos[0] < len(toks) else (None, None)

    def take(want=None):
        t = peek()
        if t[0] is None or (want is not None and t != ("op", want)):
            raise Unreadable("expected %r at token %d" % (want, pos[0]))
        pos[0] += 1
        return t

    def or_():
        node = and_()
        while peek() == ("op", "||"):
            take()
            node = ("or", node, and_())
        return node

    def and_():
        node = cmp_()
        while peek() == ("op", "&&"):
            take()
            node = ("and", node, cmp_())
        return node

    def cmp_():
        node = unary()
        t = peek()
        if t[0] == "op" and t[1] in ("==", "!="):
            take()
            node = (t[1], node, unary())
        elif t[0] == "op" and t[1] in ("<", ">", "<=", ">="):
            raise Unreadable("relational operator %s" % t[1])
        return node

    def unary():
        if peek() == ("op", "!"):
            take()
            return ("not", unary())
        return primary()

    def primary():
        kind, val = take()
        if (kind, val) == ("op", "("):
            node = or_()
            take(")")
            return node
        if kind == "lit":
            return ("lit", val)
        if kind == "name":
            if val in ("true", "false"):
                return ("lit", val == "true")
            if val == "null":
                return ("lit", None)
            if peek() == ("op", "("):
                take()
                args = []
                if peek() != ("op", ")"):
                    args.append(or_())
                    while peek() == ("op", ","):
                        take()
                        args.append(or_())
                take(")")
                return ("call", val.lower(), args)
            return ("ref", val)
        raise Unreadable("unexpected %r" % val)

    node = or_()
    if pos[0] != len(toks):
        raise Unreadable("trailing tokens after position %d" % pos[0])
    return node


def refs(node):
    if node[0] == "ref":
        yield node[1]
    elif node[0] == "call":
        for a in node[2]:
            for r in refs(a):
                yield r
    elif node[0] in ("and", "or", "==", "!="):
        for r in refs(node[1]):
            yield r
        for r in refs(node[2]):
            yield r
    elif node[0] == "not":
        for r in refs(node[1]):
            yield r


def truthy(v):
    if v is None or v is False:
        return False
    if isinstance(v, float):
        return v == v and v != 0
    if isinstance(v, str):
        return v != ""
    return True


def as_number(v):
    if v is None:
        return 0.0
    if isinstance(v, bool):
        return 1.0 if v else 0.0
    if isinstance(v, float):
        return v
    try:
        return float(v.strip() or "0")
    except ValueError:
        return float("nan")


def loose_eq(a, b):
    """GitHub's `==`: strings compare case-insensitively, mixed types are
    coerced to numbers (null 0, false 0, true 1, '' 0, 'abc' NaN)."""
    if isinstance(a, str) and isinstance(b, str):
        return a.lower() == b.lower()
    if a is None and b is None:
        return True
    return as_number(a) == as_number(b)


def evaluate(node, ctx):
    op = node[0]
    if op == "lit":
        return node[1]
    if op == "ref":
        if node[1] not in ctx:
            raise Unreadable("no model for `%s`" % node[1])
        return ctx[node[1]]
    if op == "not":
        return not truthy(evaluate(node[1], ctx))
    if op == "and":
        a = evaluate(node[1], ctx)
        return evaluate(node[2], ctx) if truthy(a) else a
    if op == "or":
        a = evaluate(node[1], ctx)
        return a if truthy(a) else evaluate(node[2], ctx)
    if op in ("==", "!="):
        eq = loose_eq(evaluate(node[1], ctx), evaluate(node[2], ctx))
        return eq if op == "==" else not eq
    if op == "call":
        name, args = node[1], [evaluate(a, ctx) for a in node[2]]
        if name in ("always", "success", "cancelled", "failure") and not args:
            # pr_runs tries cancelled() and failure() both ways; every other
            # model is a run that has not been cancelled and has not failed.
            key = name + "()"
            if key in ctx:
                return ctx[key]
            return name in ("always", "success")
        if name == "fromjson" and len(args) == 1 and isinstance(args[0], str):
            return json.loads(args[0])
        if name == "contains" and len(args) == 2:
            hay, needle = args
            if isinstance(hay, list):
                return any(loose_eq(x, needle) for x in hay)
            if isinstance(hay, str) and isinstance(needle, str):
                return needle.lower() in hay.lower()
        if name in ("startswith", "endswith") and len(args) == 2 and \
                all(isinstance(a, str) for a in args):
            f = str.startswith if name == "startswith" else str.endswith
            return f(args[0].lower(), args[1].lower())
        raise Unreadable("function %s() with these arguments" % name)
    raise Unreadable("node %r" % (op,))


SCHEDULE_CONTEXT = {
    "github.event_name": "schedule",
    "github.ref": "refs/heads/main",
    "github.ref_name": "main",
    "github.ref_type": "branch",
    "github.base_ref": "",
    "github.head_ref": "",
}


# A push to main, as push_skips models it. The rest is as for the nightly.
PUSH_CONTEXT = dict(SCHEDULE_CONTEXT, **{"github.event_name": "push"})


def push_skips(cond):
    """None if `cond` is true for a main push under every combination of gate
    outputs it reads; otherwise a string saying when it is false. A job that
    can be false is path-gated per push: a newer run need not run it."""
    return skips(cond, PUSH_CONTEXT, "a main push")


# A release tag push (owner 2026-11-03): ci.yml and checks.yml run on the
# calver tag instead of on main, and release-deploy.sh rolls the tag only on
# those runs. A tag push has no base to diff, so it must run every heavy job
# whatever the gate outputs say, exactly as the nightly does -- but for
# TAG_GATED below.
TAG_CONTEXT = dict(SCHEDULE_CONTEXT, **{
    "github.event_name": "push",
    "github.ref": "refs/tags/2026.307.1",
    "github.ref_name": "2026.307.1",
    "github.ref_type": "tag",
})


# Heavy jobs a release tag push may skip on one gate output, and that output
# (gd-rmHsVjmKse; see the header).
TAG_GATED = {("ci.yml", "gardenhub-browser"): "needs.gate.outputs.gardenhub_browser"}


def runs_on_tag(cond):
    """None if `cond` is true for a release tag push under every combination
    of gate outputs it reads; otherwise a string saying why not."""
    return skips(cond, TAG_CONTEXT, "a release tag push")


def runs_on_schedule(cond):
    """None if `cond` is true for a scheduled run under every combination of
    gate outputs it reads; otherwise a string saying why not."""
    return skips(cond, SCHEDULE_CONTEXT, "a scheduled run")


# A pull_request, as pr_runs models it. Everything the event payload or a
# gate can say is tried in pr_runs; these are what a PR run always has.
PR_CONTEXT = {
    "github.event_name": "pull_request",
    "github.ref": "refs/pull/1/merge",
    "github.ref_name": "1/merge",
    "github.ref_type": "branch",
    "github.base_ref": "main",
    "github.head_ref": "agent/x",
}
# Values tried for each thing pr_runs does not pin.
PR_RESULTS = ("success", "failure", "skipped", "cancelled")
PR_EVENT_VALUES = (None, "x")
MAX_PR_COMBOS = 4 ** 8


def pr_runs(cond):
    """None if `cond` is false on EVERY pull_request the guard models;
    otherwise a string naming one case in which it is true. Tried: draft true
    and false, every `needs.<job>.result` in PR_RESULTS, every gate output
    (`superseded` too) in OUTPUT_VALUES, any other `github.event.*` field as
    absent or set, and `cancelled()` / `failure()` both ways. A reference
    with no model (vars, inputs, matrix, ...) raises Unreadable: the guard
    cannot vouch for what it cannot try."""
    tree = parse(cond)
    axes = []
    for r in sorted(set(refs(tree))):
        parts = r.split(".")
        if r in PR_CONTEXT:
            continue
        if r == DRAFT_REF:
            axes.append((r, (True, False)))
        elif parts[0] == "github" and len(parts) > 2 and parts[1] == "event":
            axes.append((r, PR_EVENT_VALUES))
        elif parts[0] == "needs" and len(parts) == 3 and parts[2] == "result":
            axes.append((r, PR_RESULTS))
        elif parts[0] == "needs" and len(parts) == 4 and parts[2] == "outputs":
            axes.append((r, OUTPUT_VALUES))
        else:
            raise Unreadable("no model for `%s` on a pull_request" % r)
    status = (("cancelled()", (False, True)), ("failure()", (False, True)))
    names = [a[0] for a in axes] + [s[0] for s in status]
    domains = [a[1] for a in axes] + [s[1] for s in status]
    combos = 1
    for d in domains:
        combos *= len(d)
    if combos > MAX_PR_COMBOS:
        raise Unreadable("reads %d things that vary on a pull_request; the "
                         "guard tries at most %d combinations"
                         % (len(axes), MAX_PR_COMBOS))
    for values in itertools.product(*domains):
        ctx = dict(PR_CONTEXT)
        ctx.update(zip(names, values))
        ctx["success()"] = not (ctx["cancelled()"] or ctx["failure()"])
        if truthy(evaluate(tree, ctx)):
            return ", ".join("%s=%r" % kv for kv in zip(names, values))
    return None


def light_runs(cond):
    """Problems with draft-light's `if:`: it must be true on a draft and on a
    ready pull_request whose gate is green and not superseded, and false on
    every other event ci.yml takes."""
    tree = parse(cond)
    out = []

    def ctx_for(base, draft):
        ctx = dict(base)
        for r in set(refs(tree)):
            parts = r.split(".")
            if r in ctx:
                continue
            if r == DRAFT_REF:
                ctx[r] = draft
            elif parts[0] == "github" and len(parts) > 2 and parts[1] == "event":
                ctx[r] = None
            elif parts[0] == "needs" and len(parts) == 3 and parts[2] == "result":
                ctx[r] = "success"
            elif parts[0] == "needs" and len(parts) == 4 and \
                    parts[2] == "outputs":
                ctx[r] = "false" if parts[3] == "superseded" else "true"
            else:
                raise Unreadable("no model for `%s`" % r)
        return ctx

    for draft in (True, False):
        if not truthy(evaluate(tree, ctx_for(PR_CONTEXT, draft))):
            out.append("it does not run on a %s pull_request"
                       % ("draft" if draft else "ready"))
    for event in ("push", "schedule", "workflow_dispatch"):
        base = dict(SCHEDULE_CONTEXT, **{"github.event_name": event})
        if truthy(evaluate(tree, ctx_for(base, None))):
            out.append("it runs on %s, which runs the full suite instead"
                       % event)
    return out


def skips(cond, base, label):
    tree = parse(cond)
    outputs = []
    ctx = dict(base)
    for r in sorted(set(refs(tree))):
        parts = r.split(".")
        if r in ctx:
            continue
        if parts[0] == "github" and len(parts) > 2 and parts[1] == "event":
            ctx[r] = None  # no PR payload; the guard models no push payload
        elif parts[0] == "needs" and len(parts) == 3 and parts[2] == "result":
            ctx[r] = "success"
        elif parts[0] == "needs" and len(parts) == 4 and parts[2] == "outputs":
            if parts[3] == "superseded":
                ctx[r] = "false"
            else:
                outputs.append(r)
        else:
            raise Unreadable("no model for `%s` on %s" % (r, label))
    if len(outputs) > MAX_OUTPUTS:
        raise Unreadable("reads %d gate outputs; the guard tries at most %d"
                         % (len(outputs), MAX_OUTPUTS))
    for values in itertools.product(OUTPUT_VALUES, repeat=len(outputs)):
        ctx.update(zip(outputs, values))
        if not truthy(evaluate(tree, ctx)):
            return ", ".join("%s=%r" % kv for kv in zip(outputs, values)) \
                or "with no gate outputs read at all"
    return None


def schedule_crons(lines):
    on = find_key(lines, "on", 0)
    if on is None:
        return []
    on_lines = block(lines, on)
    at = find_key(on_lines, "schedule", 2)
    if at is None:
        return []
    crons = []
    for _, line in block(on_lines, at):
        mi = ITEM.match(line)
        if mi:
            m = KEY.match("  " + mi.group(1))
            if m and m.group(2) == "cron" and m.group(3):
                crons.append(unquote(m.group(3)))
    return crons


def dispatch_without_schedule(lines):
    """Lines that name workflow_dispatch but not schedule, outside `on:` and
    outside job-level `if:`s (runs_on_schedule proves those, across lines)."""
    skip = set()
    for i, (ind, line) in enumerate(lines):
        m = KEY.match(line)
        if m and ((ind == 0 and m.group(2) == "on") or
                  (ind == 4 and m.group(2) == "if")):
            skip.update(range(i, i + 1 + len(block(lines, i))))
    return [line.strip() for i, (_, line) in enumerate(lines)
            if i not in skip and "workflow_dispatch" in line
            and "schedule" not in line]


def check_schedule(fname, lines, found, problems):
    """Append nightly-run problems for one workflow; return how many heavy
    jobs were proven to run on schedule."""
    if not schedule_crons(lines):
        problems.append(
            "%s: no `on.schedule` with a `cron:`. The nightly full run is "
            "where everything the PR gates skip still runs (gd-2kZxn16TaS)."
            % fname)
    proven = 0
    for job_id, spec in found.items():
        cond = spec["if"]
        if job_id in exempt(fname) or cond is None or cond == "false":
            continue  # gate jobs; an unconditional job is reported above
        try:
            why = runs_on_schedule(cond)
        except Unreadable as e:
            problems.append(
                "%s: %s's `if:` cannot be evaluated for a scheduled run (%s). "
                "This guard must prove the job runs nightly; keep the "
                "condition to ==, !=, &&, ||, !, (), github.event_name/ref, "
                "github.event.*, needs.*.result and needs.*.outputs.*."
                % (fname, job_id, e))
            continue
        if why is not None:
            problems.append(
                "%s: %s does not run on the nightly `schedule` run when %s. "
                "Every path gate that lets `workflow_dispatch` through must "
                "let `schedule` through too:\n"
                "      (github.event_name == 'workflow_dispatch' || "
                "github.event_name == 'schedule' || ..."
                % (fname, job_id, why))
            continue
        proven += 1
    for line in dispatch_without_schedule(lines):
        problems.append(
            "%s: this line handles `workflow_dispatch` but not `schedule`; "
            "the nightly run needs the same treatment (every arm forced, its "
            "own concurrency key):\n      %s" % (fname, line))
    return proven


def check_tag(fname, found, problems):
    """Append release-tag problems for one workflow; return how many heavy
    jobs were proven to run on a tag push."""
    proven = 0
    for job_id, spec in found.items():
        cond = spec["if"]
        if job_id in exempt(fname) or cond is None or cond == "false":
            continue
        gate_out = TAG_GATED.get((fname, job_id))
        try:
            if gate_out is None:
                why = runs_on_tag(cond)
            else:
                why = skips(cond, dict(TAG_CONTEXT, **{gate_out: "true"}),
                            "a release tag push") or \
                    skips(cond, dict(TAG_CONTEXT, **{
                        gate_out.split(".outputs.")[0] + ".result": "failure"}),
                        "a release tag push")
                if why is not None:
                    why += (" (it may skip a tag only on %s == 'false' with "
                            "the gate green)" % gate_out)
        except Unreadable as e:
            problems.append(
                "%s: %s's `if:` cannot be evaluated for a release tag push "
                "(%s)." % (fname, job_id, e))
            continue
        if why is not None:
            problems.append(
                "%s: %s does not run on a release tag push when %s. The tag's "
                "run is what release-deploy.sh rolls on (owner 2026-11-03), so "
                "every path gate must let a tag through beside dispatch and "
                "schedule:\n"
                "      (github.event_name == 'workflow_dispatch' || "
                "github.event_name == 'schedule' ||\n"
                "       startsWith(github.ref, 'refs/tags/') || ..."
                % (fname, job_id, why))
            continue
        proven += 1
    return proven


def needs_of(body):
    """The job's `needs:` as a list: a bare scalar, a `[a, b]` flow list, or
    a block list. Empty when absent."""
    at = find_key(body, "needs", 4)
    if at is None:
        return []
    m = KEY.match(body[at][1])
    inline = (m.group(3) or "").strip()
    if inline.startswith("[") and inline.endswith("]"):
        return [unquote(t) for t in inline[1:-1].split(",") if t.strip()]
    if inline:
        return [unquote(inline)]
    return [unquote(ITEM.match(l).group(1)) for _, l in block(body, at)
            if ITEM.match(l)]


def submap(lines, i):
    """{key: scalar} for the map nested under lines[i], one level down."""
    nested = block(lines, i)
    if not nested:
        return {}
    level = nested[0][0]
    out = {}
    for j, (ind, line) in enumerate(nested):
        m = KEY.match(line)
        if ind == level and m:
            out[m.group(2)] = scalar(nested, j)
    return out


def steps_of(body):
    """[{'id','name','uses','if','run','with'}] for the job's `steps:`, in order,
    or None when there is no `steps:`. An item's first key shares the `- `
    line; it is re-indented to its siblings' level so the ordinary key
    readers above work on it."""
    at = find_key(body, "steps", 4)
    if at is None:
        return None
    lines = block(body, at)
    if not lines:
        return []
    level = lines[0][0]
    starts = [i for i, (ind, l) in enumerate(lines)
              if ind == level and l.lstrip().startswith("- ")]
    out = []
    for n, st in enumerate(starts):
        end = starts[n + 1] if n + 1 < len(starts) else len(lines)
        first = lines[st][1].lstrip()[2:].lstrip()
        item = [(level + 2, " " * (level + 2) + first)] + lines[st + 1:end]
        step = {"id": None, "name": None, "uses": None, "if": None,
                "run": None, "with": {}}
        for k in ("id", "name", "uses", "if", "run"):
            ki = find_key(item, k, level + 2)
            if ki is not None:
                step[k] = scalar(item, ki)
        wi = find_key(item, "with", level + 2)
        if wi is not None:
            step["with"] = submap(item, wi)
        out.append(step)
    return out


def describe(step):
    run = step["run"] and "run: " + step["run"][:50]
    return repr(step["name"] or step["uses"] or step["id"] or run or "?")


def check_supersession(fname, found, problems):
    """Append problems for the supersession shape of ci.yml or checks.yml;
    return the number of heavy jobs that carry the in-job re-check."""
    gate = SUPERSESSION_JOB[fname]
    run_wide = RUN_WIDE[fname]
    # The composite defaults to ci.yml, so ci.yml may leave `workflow:` out.
    own_wf = (fname, None) if fname == "ci.yml" else (fname,)
    sup = found.get(gate)
    if sup is None:
        problems.append(
            "%s: no `%s` job. Copy it from ci.yml; see the header of this "
            "guard." % (fname, gate))
    else:
        body = sup["body"]
        want_name = SUPERSESSION_NAME.get(fname)
        if want_name and sup["name"] != want_name:
            problems.append(
                "%s: %s's `name:` is %r, not %r. Keep it: it is how the "
                "jobs API history tells a gate-skipped run from a pre-gate "
                "one (release-cut.sh keyed on it until 2026-11-03)."
                % (fname, gate, sup["name"], want_name))
        ro = find_key(body, "runs-on", 4)
        if ro is None or scalar(body, ro) != SUPERSESSION_RUNS_ON:
            problems.append(
                "%s: %s must run on `%s`; its whole point is to not claim a "
                "node." % (fname, gate, SUPERSESSION_RUNS_ON))
        sup_steps = steps_of(body) or []
        calls = [st for st in sup_steps if st["uses"] == SUPERSESSION_ACTION]
        if not calls:
            problems.append("%s: %s does not call %s."
                            % (fname, gate, SUPERSESSION_ACTION))
        else:
            want = "steps.%s.outputs.superseded == 'true'" % calls[0]["id"]
            after = sup_steps[sup_steps.index(calls[0]) + 1:]
            fired = [st for st in after if st["name"] == SUPERSESSION_FIRED]
            if not calls[0]["id"] or not fired or fired[0]["if"] != want:
                problems.append(
                    "%s: %s needs a step named %r after the %s call, with "
                    "`if: %s` exactly (the call needs an `id:`). It is how "
                    "release-deploy.sh sees that the gate fired: a path-gated job "
                    "can still build in such a run, so a sentinel `success` "
                    "alone would read it as green."
                    % (fname, gate, SUPERSESSION_FIRED,
                       SUPERSESSION_ACTION, want))
        for st in calls:
            if st["with"].get("workflow") not in own_wf:
                problems.append(
                    "%s: %s calls %s without `workflow: %s`. The composite "
                    "defaults to ci.yml, so it would ask whether a newer "
                    "CI.YML run exists." % (fname, gate,
                                             SUPERSESSION_ACTION, fname))
            if st["with"].get("job"):
                problems.append(
                    "%s: %s passes `job:`; the gate job's verdict is "
                    "run-wide and must not." % (fname, gate))

    checked = 0
    for job_id, spec in found.items():
        if job_id in exempt(fname) or spec["if"] == "false":
            continue
        where = "%s: %s" % (fname, job_id)
        body = spec["body"]
        # No `if:` at all is checked as an ungated job, so it is reported for
        # lacking the run-wide verdict rather than skipped. The draft-gate
        # rule reports it too today, but this must not rest on that rule.
        cond = spec["if"] or ""
        gated = path_gated(fname, cond)
        if gated:
            if run_wide.split(" != ")[0] in cond:
                problems.append(
                    "%s is path-gated (%s) and also reads the run-wide "
                    "supersession verdict. Drop it: a newer run need not run "
                    "a path-gated job, so deferring to one can leave this "
                    "change unchecked. The in-job re-check with `job:` covers "
                    "it." % (where, gated))
        else:
            if gate not in needs_of(body):
                problems.append("%s: `needs:` lacks `%s`." % (where, gate))
            for term, why in ((run_wide, "the run-wide supersession verdict"),
                              (NOT_CANCELLED, "`!cancelled()`, without which "
                               "a failed gate job would SKIP this job instead "
                               "of failing open")):
                v = anded(cond, term)
                if v != "ok":
                    problems.append(
                        "%s's `if:` %s %s. AND in, written exactly so:\n"
                        "      %s" % (where, "lacks" if v == "missing"
                                       else "does not AND", why, term))
        pi = find_key(body, "permissions", 4)
        if pi is None or submap(body, pi).get("actions") not in ("read", "write"):
            problems.append(
                "%s: no `permissions: actions: read`; the in-job re-check "
                "lists this workflow's runs and jobs." % where)

        steps = steps_of(body) or []
        at = next((i for i, st in enumerate(steps)
                   if st["uses"] == SUPERSESSION_ACTION), None)
        if at is None:
            problems.append(
                "%s: no in-job supersession re-check (a step using %s). The "
                "gate job answers before the queue wait; this is the half "
                "that sees the pushes made during it." % (where,
                                                           SUPERSESSION_ACTION))
            continue
        checked += 1
        call = steps[at]
        if call["id"] != "superseded":
            problems.append("%s: the re-check step's `id:` is %r, not "
                            "`superseded`; every guard reads that id."
                            % (where, call["id"]))
        if call["with"].get("workflow") not in own_wf:
            problems.append("%s: the re-check lacks `workflow: %s`."
                            % (where, fname))
        # GitHub names a job with no `name:` after its id. ci.yml's ungated
        # jobs (every newer run runs them) may use whole-run semantics and
        # leave `job:` out; everywhere else it is required.
        own_name = spec["name"] or job_id
        job = call["with"].get("job")
        if job != own_name and (job or gated or fname != "ci.yml"):
            problems.append(
                "%s: the re-check's `job:` is %r but the job is named %r. "
                "They must match exactly: the composite looks for a newer "
                "run's job of THAT name, and a mismatch finds none (no "
                "saving) or the wrong one; no `job:` at all falls back to "
                "whole-run semantics, which is unsound for a path-gated job."
                % (where, call["with"].get("job"), own_name))
        before = steps[:at]
        if not before or not all((st["uses"] or "").startswith("actions/checkout@")
                                 for st in before):
            problems.append(
                "%s: only `actions/checkout` may run before the re-check; "
                "anything else is spent on superseded runs too." % where)
        sentinel_seen = False
        for st in steps[at + 1:]:
            if st["name"] == DECISION and not sentinel_seen:
                continue
            g = anded(st["if"] or "", STEP_GUARD)
            if g != "ok":
                problems.append(
                    "%s: step %s is %s after the re-check. AND in:\n      %s"
                    % (where, describe(st),
                       "unguarded" if g == "missing" else "not ANDed",
                       STEP_GUARD))
            if not sentinel_seen:
                sentinel_seen = True
                if st["name"] != SENTINEL or st["if"] != STEP_GUARD:
                    problems.append(
                        "%s: the first guarded step must be `%s` with `if: "
                        "%s` and nothing else, not %s. Readers (release-deploy.sh) "
                        "key on its conclusion." % (where, SENTINEL, STEP_GUARD,
                                                    describe(st)))
        if not sentinel_seen:
            problems.append("%s: no `%s` step after the re-check."
                            % (where, SENTINEL))
    if checked == 0:
        problems.append(
            "%s: no heavy job carries the supersession re-check, or this "
            "guard's parser read none." % fname)
    return checked


def path_gated(fname, cond):
    """Why a job with this `if:` is path-gated per push, or None if every
    newer run of the workflow runs it too."""
    if fname != "ci.yml":
        return "its `if:` reads `%s`" % PATH_GATED_MARK \
            if PATH_GATED_MARK in cond else None
    try:
        why = push_skips(cond)
    except Unreadable as e:
        # check_schedule reports it; path-gated is the side that cannot
        # defer to a run that never runs this job.
        return "its `if:` cannot be evaluated (%s)" % e
    return why and "its `if:` is false on a main push when %s" % why


def check_no_pr(fname, lines, problems):
    """Append problems if fname's `on:` lists a pull request trigger, or
    cannot be read well enough to say it does not."""
    on = find_key(lines, "on", 0)
    if on is None:
        problems.append("%s: no top-level `on:` block" % fname)
        return
    inline = (KEY.match(lines[on][1]).group(3) or "").strip()
    if inline:
        named = [unquote(t) for t in inline.strip("[]").split(",")]
    else:
        named = [KEY.match(l).group(2) for ind, l in block(lines, on)
                 if ind == 2 and KEY.match(l)]
    if not named:
        problems.append("%s: `on:` lists no trigger this guard can read"
                        % fname)
    for t in NO_PR_TRIGGERS:
        if t in named:
            problems.append(
                "%s: `on:` lists `%s`. Its PR run built no image and fed "
                "nothing (`images` is not a required check), at one light pod "
                "per PR push; the release tag's run is the image net "
                "(gd-bjj3robkjs). Remove the trigger." % (fname, t))


def check_workflow(fname, problems, rechecked):
    """Append problems for one workflow; return (gated jobs, jobs proven to
    run on schedule, jobs proven to run on a release tag). rechecked[0] is bumped by the jobs carrying the
    supersession re-check."""
    path = os.path.join(WF_DIR, fname)
    if not os.path.exists(path):
        problems.append("%s: does not exist (looked in %s)" % (fname, WF_DIR))
        return 0, 0, 0
    with open(path) as fh:
        lines = logical_lines(fh.read())

    if fname in NO_PR_WORKFLOWS:
        check_no_pr(fname, lines, problems)
        return 0, 0, 0

    types, problem = pull_request_types(lines)
    if problem:
        problems.append("%s: %s" % (fname, problem))
    else:
        extra = sorted(set(types) - set(PR_TYPES))
        missing = [x for x in PR_TYPES if x not in types]
        if extra:
            problems.append(
                "%s: `pull_request.types` lists %s. A PR's draft state no "
                "longer changes what runs (gd-19ECZxvFQw): `ready_for_review` "
                "would re-run the light tier on a head that already has a run, "
                "and `converted_to_draft` needs no run. It must be exactly "
                "[%s]." % (fname, ", ".join("`%s`" % x for x in extra),
                           ", ".join(PR_TYPES)))
        if missing:
            problems.append(
                "%s: `pull_request.types` lacks %s. Every head a PR can have "
                "needs a run, or the required checks never report on it. It "
                "must be exactly [%s]."
                % (fname, ", ".join("`%s`" % x for x in missing),
                   ", ".join(PR_TYPES)))

    group, problem = concurrency_group(lines)
    if problem:
        problems.append("%s: %s" % (fname, problem))
    elif DRAFT_REF in group:
        problems.append(
            "%s: the run-level `concurrency.group` reads `%s`. Every PR run is "
            "the same light tier since gd-19ECZxvFQw, so splitting a PR's "
            "slot by draft state only lets a superseded run of the PR keep "
            "running beside its successor. The pull_request arm must be, "
            "exactly:\n      %s" % (fname, DRAFT_REF, PR_GROUP_ARM))
    elif PR_GROUP_ARM not in group:
        problems.append(
            "%s: the run-level `concurrency.group` has no one-slot-per-PR arm, "
            "so a newer push to a PR does not cancel its older run. The "
            "pull_request arm must be, exactly:\n      %s"
            % (fname, PR_GROUP_ARM))

    found = jobs(lines)
    if found is None:
        problems.append("%s: no top-level `jobs:` block" % fname)
        return 0, 0, 0

    gates = GATE_JOBS.get(fname, ())
    for job_id in gates:
        if job_id not in found:
            if job_id == LIGHT_JOB:
                continue  # reported below, with its own explanation
            problems.append(
                "%s: no `%s` job, but GATE_JOBS exempts it. If the gate was "
                "renamed, update GATE_JOBS in this guard to match."
                % (fname, job_id))
        elif job_id not in FULL_CLONE_OK:
            problems.extend(checkout_problems(fname, job_id,
                                              found[job_id]["body"]))

    gated = 0
    for job_id, spec in found.items():
        if job_id in exempt(fname):
            continue
        cond = spec["if"]
        if cond is not None and cond == "false":
            continue
        if cond is None:
            problems.append(
                "%s: %s has no job-level `if:`, so it runs on every pull "
                "request. Add\n      if: %s" % (fname, job_id, CONJUNCT))
            continue
        try:
            why = pr_runs(cond)
        except Unreadable as e:
            problems.append(
                "%s: %s's `if:` cannot be evaluated for a pull_request (%s). "
                "Keep it to ==, !=, &&, ||, !, (), literals, github.*, needs.* "
                "and the status functions, or teach this guard."
                % (fname, job_id, e))
            continue
        if why is not None:
            problems.append(
                "%s: %s can run on a pull_request (for example with %s). A PR "
                "runs the light tier only (owner rule 2026-10-31, "
                "gd-19ECZxvFQw). AND this into its `if:`, written exactly so:"
                "\n      %s" % (fname, job_id, why, CONJUNCT))
            continue
        gated += 1

    if gated == 0:
        problems.append(
            "%s: no gated jobs found at all. Either every job is a gate job, "
            "or this guard's parser read nothing -- either way it cannot "
            "vouch for this file." % fname)

    if fname in SUPERSESSION_WORKFLOWS:
        rechecked[0] += check_supersession(fname, found, problems)

    if fname == LIGHT_WORKFLOW:
        light = found.get(LIGHT_JOB)
        if light is None:
            problems.append(
                "%s: no `%s` job. A PR gets no feedback at all without it; "
                "see gd-JGkmjFfv3B for its shape." % (fname, LIGHT_JOB))
        else:
            if not (light["name"] or "").startswith(LIGHT_NAME_PREFIX):
                problems.append(
                    "%s: %s's `name:` is %r; it must start with `%s` so the "
                    "checks list shows it is not the full suite."
                    % (fname, LIGHT_JOB, light["name"], LIGHT_NAME_PREFIX))
            try:
                bad = light_runs(light["if"] or "false")
            except Unreadable as e:
                bad = ["it cannot be evaluated (%s)" % e]
            for why in bad:
                problems.append(
                    "%s: %s's `if:` is %r and %s. It must run on every pull "
                    "request, draft or ready, and on nothing else."
                    % (fname, LIGHT_JOB, light["if"], why))
            t = light["timeout"]
            if t is None or not t.isdigit() or int(t) > LIGHT_TIMEOUT_MAX:
                problems.append(
                    "%s: %s's `timeout-minutes` is %r; the light tier's budget "
                    "is %d (target 10)." % (fname, LIGHT_JOB, t, LIGHT_TIMEOUT_MAX))

    nightly = tagged = 0
    if fname in SCHEDULE_WORKFLOWS:
        nightly = check_schedule(fname, lines, found, problems)
        tagged = check_tag(fname, found, problems)
    return gated, nightly, tagged


def main():
    problems = []
    gated = nightly = tagged = 0
    rechecked = [0]
    for fname in WORKFLOWS:
        g, n, t = check_workflow(fname, problems, rechecked)
        gated += g
        nightly += n
        tagged += t

    if problems:
        e = sys.stderr
        print("check-ci-draft-gate: FAIL", file=e)
        print("", file=e)
        for p in problems:
            print("  %s" % p, file=e)
        print("", file=e)
        print("See the header of %s for why this matters." % __file__, file=e)
        return 1

    print("check-ci-draft-gate: OK (%d workflows, %d heavy jobs never run "
          "on a pull_request, %s present in %s, %d heavy jobs run on schedule "
          "in %s, %d heavy jobs run on a release tag push, %d heavy jobs in "
          "%s carry the supersession re-check, no pull_request trigger in %s)"
          % (len(WORKFLOWS), gated, LIGHT_JOB, LIGHT_WORKFLOW, nightly,
             " and ".join(SCHEDULE_WORKFLOWS), tagged,
             rechecked[0], ", ".join(SUPERSESSION_WORKFLOWS),
             ", ".join(NO_PR_WORKFLOWS)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
