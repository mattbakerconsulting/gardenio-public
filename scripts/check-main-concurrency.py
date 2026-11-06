#!/usr/bin/env python3
"""check-main-concurrency.py

Guard the run-level concurrency and the path-gate wiring of
.github/workflows/ci.yml and checks.yml for the events they now run on: a
release TAG push, pull_request, workflow_dispatch and the nightly schedule.
(The name is historical: until 2026-11-03 this guarded the batched main-push
group of gd-RZopn9lf5I.)

WHY THIS EXISTS
---------------
Owner, 2026-11-03: "we don't care for ci on main, rather on the release
branch". Main moves every few minutes, so the waiting main run was nearly
always replaced by the next merge: of the last 30 ci.yml main push runs 19
were cancelled and 9 succeeded (checks.yml: 10 of 15 cancelled), and
release-cut.sh, which tagged only a commit with a green main run, cut commits
hours old. Now main pushes run neither workflow; the calver tag push does,
and release-deploy.sh rolls a tag only on its own green ci.yml, checks.yml
and images.yml runs. The nightly schedule is the one full main-head run.

Each part of that fails SILENTLY when it breaks:

  * `branches: [main]` coming back onto `on.push` restores the run that never
    finishes, on every merge. Nothing goes red; it just costs.
  * `tags:` losing the calver pattern means a tag has no ci.yml / checks.yml
    run, so release-deploy.sh skips every tag forever ("ci.yml not green").
  * a tag run sharing a concurrency group (per-sha, say, as it was) can be
    replaced while it waits, by a second tag on the same commit; a tag run
    with cancel-in-progress true can be cancelled. Either way the tag never
    gets the run its deploy waits for.
  * a dispatch or nightly run sharing a group can be evicted while it waits.
  * the PR arm no longer cancelling in progress puts every superseded PR
    push back on the pool.
  * a path gate that does not fail open on a tag push skips a job on the
    release run, and the tag rolls untested by it. The ONE exception is
    ci.yml's browser suite (owner 2026-11-01, gd-rmHsVjmKse: "skip tests
    even on release cut unless relevant files are impacted"), and it is
    safe only while its base is a tag whose suite PASSED and every doubt
    runs it. A base that counted a skipped suite would chain skips forever:
    tag 2 skips because tag 1 passed, and tag 3 diffing from tag 2 would
    never test tag 2's UI change.

WHAT IT CHECKS
--------------
For ci.yml and checks.yml:

  `on.push`                    lists `tags:` with '20*' (as images.yml) and
                               no `branches:` / `branches-ignore:`.

and the top-level `concurrency:` block is EVALUATED (a small evaluator for
==, !=, &&, ||, !, (), format(), startsWith(), literals and context paths;
anything else is reported, not passed) for each event:

  push of a calver tag         every run has its own group (two tags on one
                               commit, and the same tag pushed twice, too),
                               shared with no other event; cancel-in-progress
                               false.
  pull_request                 two pushes to one PR share a group, another PR
                               gets a different one, and cancel-in-progress
                               is true.
  workflow_dispatch, schedule  every run has its own group; cancel-in-progress
                               false.

And in each workflow's path-gate job (ci.yml `gate`, checks.yml `changes`):

  * a step uses ./.github/actions/last-run-base with `continue-on-error:
    true` and `workflow: <this file>`, the job's sparse checkout includes the
    action, and the job has `actions: read`;
  * the dorny/paths-filter step's `base:` evaluates to that step's `base`,
    and its `if:`, if any, is true on a pull_request and a branch push (a
    filter skipped on PRs would close every PR path gate);
  * ci.yml: every job output read from `steps.filter` evaluates to 'true' on
    a tag push (whatever the filter said, or with the filter skipped) and on
    a push whose base is empty (fail open), and to the filter's own answer
    when a base was found or on a pull_request (POSITIVE CONTROL: the gate
    can still say false);
  * ci.yml's tag exception (TAG_DIFFED): a step uses
    ./.github/actions/last-tag-base with `continue-on-error: true`, in the
    sparse checkout; the filter's `base:` gives that step's base on a tag,
    its `if:` is true on a tag with one, and it is continue-on-error on a
    tag (a failure there must not redden the release run). The output
    `gardenhub_browser` is 'false' on a tag ONLY when that base is set, the
    filter's outcome is `success` and it said 'false' (POSITIVE CONTROL: the
    exception still skips); every other tag case -- no base, the filter
    failed or skipped, an empty answer -- is 'true'. Every other output
    stays 'true' on a tag even then;
  * checks.yml: every `BASE_SHA:` evaluates, on a push, to the base step's
    output -- the sha when there is one, empty (so `gate` fails open to
    scope=ALL) when not, as on every tag -- and never to
    `github.event.before`.

And in the action itself (gd-5VoRbArSS8), which earlier run may be a base:

  * an ACCEPT-list, `const RAN = [...]` for a completed run and
    `const RUNNING = [...]` for one that has not completed, decides it; RAN
    holds only `success` and RUNNING only `in_progress`. A deny-list let
    `startup_failure` -- a run that started no job -- become a base, which
    drops its push from every path gate until the nightly. A `failure` base
    (gd-NkRBUlh9Ar) lets a later docs-only push skip the gated job that
    failed and go green over it. Nothing goes red when either happens, so it
    is checked here. (No branch push runs these workflows today; the action
    is kept, and guarded, for when one does.)

And in ./.github/actions/last-tag-base (gd-rmHsVjmKse), which tag may be the
browser base:

  * an accept-list `const PASSED = [...]` holding only `success` decides on
    the job's conclusion (`PASSED.includes(job.conclusion)`), so `skipped`,
    `failure` and anything new are stepped over; its `JOB` is the `name:` of
    ci.yml's gardenhub-browser job and its `WORKFLOW` is 'ci.yml'. A renamed
    job would never be found and every tag would run the suite, which is
    safe but silently undoes the saving.

Usage:  python3 scripts/check-main-concurrency.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-main-concurrency.py
Self-test: python3 scripts/check-main-concurrency_test.py
"""
import os
import re
import sys

ROOT = os.environ.get("GARDENIO_ROOT") or os.path.dirname(
    os.path.dirname(os.path.abspath(__file__)))

BASE_ACTION = "./.github/actions/last-run-base"
BASE_ACTION_DIR = ".github/actions/last-run-base"
TAG_ACTION = "./.github/actions/last-tag-base"
TAG_ACTION_DIR = ".github/actions/last-tag-base"
# The one ci.yml output a calver tag may answer 'false' for, and the job it
# gates, whose name the tag action matches (gd-rmHsVjmKse).
TAG_DIFFED = {"gardenhub_browser": "gardenhub-browser"}
TAG_PASSED_OK = {"success"}
# What the action's accept-lists may hold: a run whose jobs ran and passed,
# or one still running its own gates.
RAN_OK = {"success"}
RUNNING_OK = {"in_progress"}
# Workflow file -> its path-gate job.
WORKFLOWS = {"ci.yml": "gate", "checks.yml": "changes"}


class Unreadable(Exception):
    pass


# ── A small evaluator for GitHub Actions expressions ─────────────────────────
TOKEN = re.compile(r"""
    \s*(?:
      (?P<str>'(?:[^']|'')*')
    | (?P<num>-?\d+(?:\.\d+)?)
    | (?P<op>==|!=|&&|\|\||!|\(|\)|,)
    | (?P<word>[A-Za-z_][A-Za-z0-9_.-]*)
    )""", re.X)


def tokenize(expr):
    out, i = [], 0
    expr = expr.strip()
    while i < len(expr):
        m = TOKEN.match(expr, i)
        if not m or m.end() == i:
            raise Unreadable("cannot read %r at %r" % (expr, expr[i:i + 20]))
        i = m.end()
        kind = m.lastgroup
        out.append((kind, m.group(kind)))
    return out


def truthy(v):
    return not (v is None or v is False or v == "" or v == 0)


def to_str(v):
    if v is None:
        return ""
    if v is True:
        return "true"
    if v is False:
        return "false"
    return str(v)


def equal(a, b):
    if isinstance(a, str) and isinstance(b, str):
        return a.lower() == b.lower()  # Actions compares strings ignoring case
    return a == b


class Parser:
    def __init__(self, expr, ctx):
        self.toks = tokenize(expr)
        self.i = 0
        self.ctx = ctx
        self.expr = expr

    def peek(self):
        return self.toks[self.i] if self.i < len(self.toks) else (None, None)

    def take(self, value=None):
        tok = self.peek()
        if tok[0] is None or (value is not None and tok[1] != value):
            raise Unreadable("expected %r in %r" % (value, self.expr))
        self.i += 1
        return tok

    def parse(self):
        v = self.or_()
        if self.i != len(self.toks):
            raise Unreadable("trailing %r in %r" % (self.peek()[1], self.expr))
        return v

    def or_(self):
        v = self.and_()
        while self.peek() == ("op", "||"):
            self.take()
            rhs = self.and_()
            v = v if truthy(v) else rhs
        return v

    def and_(self):
        v = self.eq()
        while self.peek() == ("op", "&&"):
            self.take()
            rhs = self.eq()
            v = rhs if truthy(v) else v
        return v

    def eq(self):
        v = self.unary()
        while self.peek() in (("op", "=="), ("op", "!=")):
            op = self.take()[1]
            rhs = self.unary()
            v = equal(v, rhs) if op == "==" else not equal(v, rhs)
        return v

    def unary(self):
        if self.peek() == ("op", "!"):
            self.take()
            return not truthy(self.unary())
        return self.primary()

    def primary(self):
        kind, val = self.take()
        if kind == "str":
            return val[1:-1].replace("''", "'")
        if kind == "num":
            return float(val) if "." in val else int(val)
        if kind == "op" and val == "(":
            v = self.or_()
            self.take(")")
            return v
        if kind == "word":
            if val in ("true", "false"):
                return val == "true"
            if val == "null":
                return None
            if self.peek() == ("op", "("):
                return self.call(val)
            return self.ctx.get(val)
        raise Unreadable("unexpected %r in %r" % (val, self.expr))

    def call(self, fn):
        self.take("(")
        args = []
        if self.peek() != ("op", ")"):
            args.append(self.or_())
            while self.peek() == ("op", ","):
                self.take()
                args.append(self.or_())
        self.take(")")
        if fn.lower() == "startswith" and len(args) == 2:
            # Actions compares ignoring case, as `equal` does.
            return to_str(args[0]).lower().startswith(to_str(args[1]).lower())
        if fn != "format" or not args:
            raise Unreadable("function %s() is not supported" % fn)
        return re.sub(r"\{(\d+)\}",
                      lambda m: to_str(args[int(m.group(1)) + 1]),
                      to_str(args[0]))


def evaluate(expr, ctx):
    return Parser(expr, ctx).parse()


def interpolate(value, ctx):
    """A workflow value: literal text with ${{ }} spans substituted."""
    return re.sub(r"\$\{\{(.*?)\}\}",
                  lambda m: to_str(evaluate(m.group(1), ctx)), value)


def as_bool(value, ctx):
    """cancel-in-progress: a literal or a single ${{ }} expression."""
    m = re.fullmatch(r"\s*\$\{\{(.*)\}\}\s*", value)
    if m:
        return truthy(evaluate(m.group(1), ctx))
    v = value.strip().lower()
    if v in ("true", "false"):
        return v == "true"
    raise Unreadable("cancel-in-progress %r is neither a boolean nor an "
                     "expression" % value)


# ── Reading the workflow ─────────────────────────────────────────────────────
def unquote(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "'\"":
        return v[1:-1]
    return v


def top_block(lines, key):
    """Lines of the top-level `key:` block, or None."""
    for i, line in enumerate(lines):
        if line.rstrip() == key + ":":
            out = []
            for nxt in lines[i + 1:]:
                if nxt.strip() and not nxt.startswith((" ", "#")):
                    break
                out.append(nxt)
            return out
    return None


def scalar(block, key, indent):
    pat = re.compile(r"^%s%s:\s*(.*?)\s*$" % (" " * indent, re.escape(key)))
    for line in block:
        m = pat.match(line)
        if m:
            return unquote(m.group(1))
    return None


def job_block(lines, job):
    jobs = top_block(lines, "jobs")
    if jobs is None:
        return None
    for i, line in enumerate(jobs):
        if line.rstrip() == "  %s:" % job:
            out = []
            for nxt in jobs[i + 1:]:
                if re.match(r"^  [A-Za-z0-9_-]+:\s*$", nxt):
                    break
                out.append(nxt)
            return out
    return None


def steps_of(block):
    """Each step of a job as its list of lines."""
    steps, cur = [], None
    for line in block:
        if re.match(r"^      - ", line):
            cur = [line]
            steps.append(cur)
        elif cur is not None:
            if line.strip() and not line.startswith("       ") \
                    and not line.lstrip().startswith("#"):
                cur = None  # left the steps list
            else:
                cur.append(line)
    return steps


def step_key(step, key):
    pat = re.compile(r"^      (?:- |  )%s:\s*(.*?)\s*$" % re.escape(key))
    for line in step:
        m = pat.match(line)
        if m:
            return unquote(m.group(1))
    return None


def with_key(step, key):
    pat = re.compile(r"^          %s:\s*(.*?)\s*$" % re.escape(key))
    for line in step:
        m = pat.match(line)
        if m:
            return unquote(m.group(1))
    return None


def outputs_of(block):
    out = {}
    inside = False
    for line in block:
        if line.rstrip() == "    outputs:":
            inside = True
            continue
        if inside:
            if line.strip() and not line.startswith("      "):
                break
            m = re.match(r"^      ([A-Za-z0-9_-]+):\s*(.*?)\s*$", line)
            if m:
                out[m.group(1)] = m.group(2)
    return out


def push_trigger(lines):
    """{key: value} of `on.push`'s direct keys, or None when there is none."""
    on = top_block(lines, "on")
    if on is None:
        return None
    for i, line in enumerate(on):
        if line.rstrip() == "  push:":
            out = {}
            for nxt in on[i + 1:]:
                if nxt.strip() and not nxt.startswith("    ") \
                        and not nxt.lstrip().startswith("#"):
                    break
                m = re.match(r"^    ([A-Za-z_-]+):\s*(.*?)\s*(#.*)?$", nxt)
                if m:
                    out[m.group(1)] = m.group(2)
            return out
    return None


TAG_REF = "refs/tags/2026.307.1"


# ── Checks ───────────────────────────────────────────────────────────────────
def ctx_for(event, ref, sha, run_id, **extra):
    ctx = {
        "github.workflow": None,  # filled in per workflow
        "github.event_name": event,
        "github.ref": ref,
        "github.sha": sha,
        "github.run_id": run_id,
        "github.event.before": "beefbeef",
    }
    ctx.update(extra)
    return ctx


def check_concurrency(fname, lines, problems, report):
    name = None
    for line in lines:
        m = re.match(r"^name:\s*(.+?)\s*$", line)
        if m:
            name = unquote(m.group(1))
            break
    if not name:
        problems.append("%s: no top-level `name:`" % fname)
        return
    conc = top_block(lines, "concurrency")
    if conc is None:
        problems.append("%s: no top-level `concurrency:` block" % fname)
        return
    group = scalar(conc, "group", 2)
    cancel = scalar(conc, "cancel-in-progress", 2)
    if group is None or cancel is None:
        problems.append("%s: `concurrency:` needs both `group:` and "
                        "`cancel-in-progress:`" % fname)
        return

    def run(ctx):
        ctx["github.workflow"] = name
        return interpolate(group, ctx), as_bool(cancel, ctx)

    push = push_trigger(lines)
    if push is None:
        problems.append("%s: no `on.push`. The release tag push is what "
                        "release-deploy.sh rolls on; it needs `push: tags: "
                        "['20*']`." % fname)
    else:
        for key in ("branches", "branches-ignore"):
            if key in push:
                problems.append(
                    "%s: `on.push` has `%s:`. Main pushes do not run this "
                    "workflow (owner 2026-11-03): the waiting main run was "
                    "replaced by the next merge 19 times in 30. Test the "
                    "release tag; the nightly schedule is the main-head run."
                    % (fname, key))
        if "'20*'" not in push.get("tags", "") and \
                '"20*"' not in push.get("tags", ""):
            problems.append(
                "%s: `on.push.tags` is %r; it must list '20*', the calver "
                "release tags, or no tag gets the run release-deploy.sh "
                "waits for." % (fname, push.get("tags")))

    try:
        tag_a = run(ctx_for("push", TAG_REF, "a" * 40, 101))
        tag_same_sha = run(ctx_for("push", "refs/tags/2026.307.2", "a" * 40,
                                   102))
        tag_again = run(ctx_for("push", TAG_REF, "a" * 40, 103))
        pr7a = run(ctx_for("pull_request", "refs/pull/7/merge", "c" * 40, 104,
                           **{"github.event.pull_request.draft": False}))
        pr7b = run(ctx_for("pull_request", "refs/pull/7/merge", "d" * 40, 105,
                           **{"github.event.pull_request.draft": False}))
        pr8 = run(ctx_for("pull_request", "refs/pull/8/merge", "c" * 40, 106,
                          **{"github.event.pull_request.draft": False}))
        per_run = {}
        for ev in ("workflow_dispatch", "schedule"):
            per_run[ev] = (run(ctx_for(ev, "refs/heads/main", "a" * 40, 107)),
                           run(ctx_for(ev, "refs/heads/main", "a" * 40, 108)))
    except Unreadable as e:
        problems.append("%s: the `concurrency:` block cannot be evaluated (%s). "
                        "Keep it to ==, !=, &&, ||, !, (), format(), "
                        "startsWith(), literals and github.* paths, or teach "
                        "this guard." % (fname, e))
        return

    tags = (tag_a, tag_same_sha, tag_again)
    if len({t[0] for t in tags}) != len(tags):
        problems.append(
            "%s: two release tag runs share a concurrency group (%r, %r, %r "
            "for two tags on one commit and a tag pushed again). A waiting "
            "tag run can then be replaced, and release-deploy.sh waits on "
            "it forever. Key a tag push per run (github.run_id)."
            % ((fname,) + tuple(t[0] for t in tags)))
    if any(t[1] for t in tags):
        problems.append(
            "%s: cancel-in-progress is true for a tag push. The tag's run is "
            "what release-deploy.sh rolls on; nothing may cancel it."
            % fname)
    others = {pr7a[0], pr8[0]} | {r[0] for pair in per_run.values()
                                  for r in pair}
    if any(t[0] in others for t in tags):
        problems.append("%s: a tag push shares a group with a pull_request, "
                        "dispatch or scheduled run." % fname)
    if pr7a[0] != pr7b[0]:
        problems.append("%s: two pushes to one PR get different groups (%r, "
                        "%r), so a new push no longer supersedes the old run."
                        % (fname, pr7a[0], pr7b[0]))
    if pr7a[0] == pr8[0]:
        problems.append("%s: two different PRs share the group %r and would "
                        "cancel each other." % (fname, pr7a[0]))
    if not (pr7a[1] and pr7b[1]):
        problems.append(
            "%s: cancel-in-progress is false for pull_request. A superseded PR "
            "run must be cancelled in progress, or every push to a PR keeps "
            "its full run on the pool." % fname)
    for ev, (r1, r2) in per_run.items():
        if r1[0] == r2[0]:
            problems.append("%s: two %s runs share the group %r. Each must "
                            "have its own (github.run_id), so none is ever "
                            "evicted while waiting." % (fname, ev, r1[0]))
        if r1[1] or r2[1]:
            problems.append("%s: cancel-in-progress is true for %s."
                            % (fname, ev))
    report.append("%s: tag push -> %r (cancel-in-progress %s); PR -> %r "
                  "(cancel-in-progress %s)"
                  % (fname, tag_a[0], str(tag_a[1]).lower(), pr7a[0],
                     str(pr7a[1]).lower()))


def check_base_wiring(fname, lines, problems, report):
    job = WORKFLOWS[fname]
    block = job_block(lines, job)
    if block is None:
        problems.append("%s: no `%s` job (the path gate). If it was renamed, "
                        "update WORKFLOWS in this guard." % (fname, job))
        return
    text = "\n".join(block)
    steps = steps_of(block)
    base_steps = [s for s in steps if step_key(s, "uses") == BASE_ACTION]
    if len(base_steps) != 1:
        problems.append(
            "%s: `%s` must have exactly one step using %s (found %d). Without "
            "it a main push diffs only `before..after`, and a push evicted "
            "from the concurrency group never reaches a path-gated job."
            % (fname, job, BASE_ACTION, len(base_steps)))
        return
    step = base_steps[0]
    sid = step_key(step, "id")
    if not sid:
        problems.append("%s: the last-run-base step in `%s` has no `id:`."
                        % (fname, job))
        return
    if (step_key(step, "continue-on-error") or "").lower() != "true":
        problems.append(
            "%s: the last-run-base step must be `continue-on-error: true`, so "
            "the step failing reads as an empty base (fail open) instead of "
            "failing the gate job and reddening the run." % fname)
    if with_key(step, "workflow") != fname:
        problems.append("%s: the last-run-base step must pass `workflow: %s` "
                        "(its own file), not %r."
                        % (fname, fname, with_key(step, "workflow")))
    if not re.search(r"^\s+%s\s*$" % re.escape(BASE_ACTION_DIR), text, re.M):
        problems.append(
            "%s: `%s`'s sparse checkout does not include %s, so the step "
            "cannot load the action and every main push fails open."
            % (fname, job, BASE_ACTION_DIR))
    if not re.search(r"^      actions:\s*read\b", text, re.M):
        problems.append("%s: `%s` needs `permissions: actions: read` to list "
                        "the workflow's runs." % (fname, job))

    base_ref = "steps.%s.outputs.base" % sid
    tag_ref = check_tag_step(fname, job, steps, text, problems) \
        if fname == "ci.yml" else None
    push = dict(ctx_for("push", "refs/heads/main", "a" * 40, 1))
    tag = dict(ctx_for("push", TAG_REF, "a" * 40, 1))
    pr = dict(ctx_for("pull_request", "refs/pull/7/merge", "a" * 40, 1),
              **{"github.event.pull_request.base.sha": "basebase"})

    filters = [s for s in steps if (step_key(s, "uses") or "")
               .startswith("dorny/paths-filter@")]
    if len(filters) != 1:
        problems.append("%s: expected one dorny/paths-filter step in `%s`, "
                        "found %d." % (fname, job, len(filters)))
    else:
        fbase = with_key(filters[0], "base")
        try:
            got = interpolate(fbase, dict(push, **{base_ref: "f00d"})) \
                if fbase is not None else None
        except Unreadable as e:
            got = "unreadable: %s" % e
        if got != "f00d":
            problems.append(
                "%s: the paths-filter step's `base:` must be "
                "`${{ %s }}` (it gave %r on a push). Its default diffs "
                "`before..after`, which drops an evicted push's changes."
                % (fname, base_ref, got))
        if tag_ref:
            try:
                got = interpolate(fbase, dict(tag, **{base_ref: "",
                                                      tag_ref: "cafe"})) \
                    if fbase is not None else None
            except Unreadable as e:
                got = "unreadable: %s" % e
            if got != "cafe":
                problems.append(
                    "%s: the paths-filter step's `base:` gave %r on a tag "
                    "push with a tag base; it must give `%s` there too, or "
                    "the browser suite's tag diff is against main."
                    % (fname, got, tag_ref))
        fif = step_key(filters[0], "if")
        if fif is not None:
            try:
                runs = [as_bool(fif if "${{" in fif else "${{ %s }}" % fif,
                                dict(c)) for c in (pr, push)]
            except Unreadable as e:
                runs = None
                problems.append("%s: the paths-filter step's `if:` cannot be "
                                "evaluated (%s)." % (fname, e))
            if runs is not None and not all(runs):
                problems.append(
                    "%s: the paths-filter step's `if:` %r is false on a "
                    "pull_request or a branch push. Skipped there, it closes "
                    "every path gate it feeds: PRs would stop running the "
                    "gated jobs." % (fname, fif))
        if tag_ref:
            fif = fif or "true"
            fcoe = step_key(filters[0], "continue-on-error") or "false"
            try:
                on_tag = as_bool(fif if "${{" in fif else "${{ %s }}" % fif,
                                 dict(tag, **{tag_ref: "cafe"}))
                coe = [as_bool(fcoe, dict(c, **{tag_ref: ""}))
                       for c in (tag, push, pr)]
            except Unreadable as e:
                on_tag, coe = None, None
                problems.append("%s: the paths-filter step's `if:` or "
                                "`continue-on-error:` cannot be evaluated "
                                "(%s)." % (fname, e))
            if on_tag is False:
                problems.append(
                    "%s: the paths-filter step's `if:` %r is false on a tag "
                    "push with a tag base, so the browser suite's tag "
                    "exception never diffs and every tag runs it."
                    % (fname, fif))
            if coe is not None and not coe[0]:
                problems.append(
                    "%s: the paths-filter step must be continue-on-error on a "
                    "tag push: it diffs there now, and its failing would "
                    "fail `%s` and so the release's run." % (fname, job))
            if coe is not None and (coe[1] or coe[2]):
                problems.append(
                    "%s: the paths-filter step is continue-on-error on a "
                    "branch push or a pull_request. A failure there would "
                    "leave every output empty and close the path gates "
                    "instead of failing `%s`." % (fname, job))

    checked = 0
    if fname == "ci.yml":
        outs = {k: v for k, v in outputs_of(block).items()
                if "steps.filter.outputs." in v}
        if not outs:
            problems.append("%s: no `%s` output reads steps.filter; the "
                            "guard read nothing." % (fname, job))
        for key, expr in sorted(outs.items()):
            diffed = key in TAG_DIFFED
            # (label, ctx, base, tag base, filter outcome, filter says, want)
            cases = [
                ("a tag push", tag, "", "", "skipped", "", "true"),
                ("a tag push", tag, "", "", "success", "false", "true"),
                ("a push with no proven base", push, "", "", "success",
                 "false", "true"),
                ("a push with a base", push, "f00d", "", "success", "false",
                 "false"),
                ("a push with a base", push, "f00d", "", "success", "true",
                 "true"),
                ("a pull_request", pr, "", "", "success", "false", "false"),
                # A tag base found: only TAG_DIFFED may take the filter's no.
                ("a tag push with a tag base", tag, "", "cafe", "success",
                 "false", "false" if diffed else "true"),
                ("a tag push with a tag base", tag, "", "cafe", "success",
                 "true", "true"),
                ("a tag push with a tag base", tag, "", "cafe", "success",
                 "", "true"),
                ("a tag push whose filter failed", tag, "", "cafe",
                 "failure", "false", "true"),
                ("a tag push whose filter failed", tag, "", "cafe",
                 "failure", "", "true"),
                ("a tag push whose filter was skipped", tag, "", "cafe",
                 "skipped", "", "true"),
            ]
            for label, ctx, base, tbase, outcome, fval, want in cases:
                c = dict(ctx, **{base_ref: base,
                                 "steps.filter.outcome": outcome,
                                 "steps.filter.outputs.%s" % key: fval})
                if tag_ref:
                    c[tag_ref] = tbase
                try:
                    got = interpolate(expr, c)
                except Unreadable as e:
                    problems.append("%s: output `%s` cannot be evaluated "
                                    "(%s)." % (fname, key, e))
                    break
                if got != want:
                    if want == "true" and (base == "" or tbase):
                        why = "it must fail open to 'true'"
                    elif tbase:
                        why = ("the browser suite's tag exception is gone: "
                               "every tag runs it (gd-rmHsVjmKse)")
                    else:
                        why = "the gate must still pass the filter's answer through"
                    if diffed:
                        shape = (
                            "%s: ${{ startsWith(github.ref, 'refs/tags/') && "
                            "(%s != '' && steps.filter.outcome == 'success' && "
                            "steps.filter.outputs.%s == 'false' && 'false' || "
                            "'true') || (github.event_name == 'push' && %s == "
                            "'') && 'true' || steps.filter.outputs.%s }}"
                            % (key, tag_ref, key, base_ref, key))
                    else:
                        shape = (
                            "%s: ${{ (startsWith(github.ref, 'refs/tags/') || "
                            "github.event_name == 'push' && %s == '') && "
                            "'true' || steps.filter.outputs.%s }}"
                            % (key, base_ref, key))
                    problems.append(
                        "%s: output `%s` is %r on %s when the filter's outcome "
                        "is %s and it says %r; %s. Write it as:\n      %s"
                        % (fname, key, got, label, outcome, fval, why, shape))
                    break
            else:
                checked += 1
        for key in sorted(set(TAG_DIFFED) - set(outs)):
            problems.append("%s: no `%s` output reading steps.filter; the "
                            "browser suite's tag exception has nothing to "
                            "gate. Update TAG_DIFFED if it moved."
                            % (fname, key))
    else:
        envs = [m.group(1) for m in
                re.finditer(r"^\s+BASE_SHA:\s*(.*?)\s*$", text, re.M)]
        if not envs:
            problems.append("%s: no BASE_SHA in `%s`; the guard read nothing."
                            % (fname, job))
        for expr in envs:
            try:
                with_base = interpolate(expr, dict(push, **{base_ref: "f00d"}))
                without = interpolate(expr, dict(push, **{base_ref: ""}))
                on_tag = interpolate(expr, dict(tag, **{base_ref: ""}))
                on_pr = interpolate(expr, dict(pr, **{base_ref: ""}))
            except Unreadable as e:
                problems.append("%s: BASE_SHA %r cannot be evaluated (%s)."
                                % (fname, expr, e))
                continue
            if with_base != "f00d" or without != "":
                problems.append(
                    "%s: BASE_SHA %r gives %r / %r on a push with / without a "
                    "proven base; it must give the `%s` step's output, and "
                    "EMPTY without one so `gate` fails open to scope=ALL -- "
                    "never github.event.before."
                    % (fname, expr, with_base, without, sid))
            elif on_tag != "":
                problems.append(
                    "%s: BASE_SHA %r gives %r on a tag push; it must be empty "
                    "there, so `gate` fails open to scope=ALL on the release "
                    "run." % (fname, expr, on_tag))
            elif on_pr != "basebase":
                problems.append("%s: BASE_SHA %r no longer gives the PR's base "
                                "sha on a pull_request (got %r)."
                                % (fname, expr, on_pr))
            else:
                checked += 1
    report.append("%s: `%s` diffs a push against step `%s`; %d fail-open "
                  "reader(s) proven" % (fname, job, sid, checked))
    if tag_ref:
        report.append("%s: `%s` diffs a calver tag's %s against `%s`; only "
                      "it may say false on a tag"
                      % (fname, job, "/".join(sorted(TAG_DIFFED)), tag_ref))


def check_tag_step(fname, job, steps, text, problems):
    """The ci.yml gate's last-tag-base step: wired to fail open. Returns its
    `steps.<id>.outputs.base` path, or None."""
    found = [s for s in steps if step_key(s, "uses") == TAG_ACTION]
    if len(found) != 1:
        problems.append(
            "%s: `%s` must have exactly one step using %s (found %d): the "
            "browser suite's tag base (gd-rmHsVjmKse)."
            % (fname, job, TAG_ACTION, len(found)))
        return None
    tid = step_key(found[0], "id")
    if not tid:
        problems.append("%s: the last-tag-base step in `%s` has no `id:`."
                        % (fname, job))
        return None
    if (step_key(found[0], "continue-on-error") or "").lower() != "true":
        problems.append(
            "%s: the last-tag-base step must be `continue-on-error: true`, so "
            "the step failing reads as an empty base (run the suite) instead "
            "of failing the gate job and reddening the release run." % fname)
    if not re.search(r"^\s+%s\s*$" % re.escape(TAG_ACTION_DIR), text, re.M):
        problems.append(
            "%s: `%s`'s sparse checkout does not include %s, so the step "
            "cannot load the action." % (fname, job, TAG_ACTION_DIR))
    return "steps.%s.outputs.base" % tid


def action_script(text):
    """The github-script body of the last-run-base action, or None."""
    lines = text.split("\n")
    for i, ln in enumerate(lines):
        if re.match(r"^\s+script:\s*\|\s*$", ln):
            body = lines[i + 1:]
            ind = len(body[0]) - len(body[0].lstrip()) if body else 0
            out = []
            for b in body:
                if b.strip() and len(b) - len(b.lstrip()) < ind:
                    break
                out.append(b[ind:])
            return "\n".join(out)
    return None


def check_base_selection(problems, report):
    path = os.path.join(ROOT, BASE_ACTION_DIR, "action.yml")
    try:
        with open(path) as fh:
            script = action_script(fh.read())
    except OSError as e:
        problems.append("%s: cannot read (%s)" % (BASE_ACTION_DIR, e))
        return
    if script is None:
        problems.append("%s/action.yml: no `script: |` block; the guard read "
                        "nothing." % BASE_ACTION_DIR)
        return
    lists = {}
    for name, ok, field in (("RAN", RAN_OK, "conclusion"),
                            ("RUNNING", RUNNING_OK, "status")):
        m = re.findall(r"^\s*const %s = \[([^\]]*)\];" % name, script, re.M)
        if len(m) != 1:
            problems.append(
                "%s: expected one `const %s = [...]` accept-list (found %d). "
                "Which earlier run may be a base must be an accept-list: a "
                "deny-list let `startup_failure` through." % (BASE_ACTION_DIR,
                                                              name, len(m)))
            continue
        got = re.findall(r"'([^']*)'", m[0])
        lists[name] = got
        bad = sorted(set(got) - ok)
        if bad:
            problems.append(
                "%s: %s holds %s, which do(es) not mean the run's jobs ran "
                "and passed. Only %s may be a base; a base whose gated jobs "
                "never ran, or failed, drops its push from every later "
                "path-gated job." % (BASE_ACTION_DIR, name,
                                                     ", ".join(bad),
                                                     ", ".join(sorted(ok))))
        if "%s.includes(cand.%s)" % (name, field) not in script:
            problems.append("%s: the candidate loop does not test "
                            "`%s.includes(cand.%s)`, so the accept-list "
                            "decides nothing." % (BASE_ACTION_DIR, name, field))
    if "success" not in lists.get("RAN", ["success"]):
        problems.append("%s: RAN lacks 'success', so no run is ever a base "
                        "and every main push fails open to a full run."
                        % BASE_ACTION_DIR)
    deny = len(re.findall(r"\.includes\(cand\.conclusion\)", script))
    if deny != 1:
        problems.append("%s: cand.conclusion is tested by %d `.includes()` "
                        "list(s); only RAN may decide it." % (BASE_ACTION_DIR,
                                                              deny))
    report.append("%s: a base is a run completed %s or %s"
                  % (BASE_ACTION_DIR, "/".join(lists.get("RAN", [])),
                     "/".join(lists.get("RUNNING", []))))


def check_tag_selection(ci_lines, problems, report):
    path = os.path.join(ROOT, TAG_ACTION_DIR, "action.yml")
    try:
        with open(path) as fh:
            script = action_script(fh.read())
    except OSError as e:
        problems.append("%s: cannot read (%s)" % (TAG_ACTION_DIR, e))
        return
    if script is None:
        problems.append("%s/action.yml: no `script: |` block; the guard read "
                        "nothing." % TAG_ACTION_DIR)
        return
    m = re.findall(r"^\s*const PASSED = \[([^\]]*)\];", script, re.M)
    if len(m) != 1:
        problems.append(
            "%s: expected one `const PASSED = [...]` accept-list (found %d). "
            "Which tag may be the browser base must be an accept-list on the "
            "job's conclusion." % (TAG_ACTION_DIR, len(m)))
        return
    got = re.findall(r"'([^']*)'", m[0])
    bad = sorted(set(got) - TAG_PASSED_OK)
    if bad:
        problems.append(
            "%s: PASSED holds %s. Only a suite that ran and passed may be the "
            "base: a skipped one would chain skips, and tag 3 diffing from a "
            "tag 2 that skipped never tests tag 2's UI change."
            % (TAG_ACTION_DIR, ", ".join(bad)))
    if "success" not in got:
        problems.append("%s: PASSED lacks 'success', so no tag is ever a base "
                        "and every tag runs the browser suite."
                        % TAG_ACTION_DIR)
    if "PASSED.includes(job.conclusion)" not in script or \
            len(re.findall(r"\.includes\(job\.conclusion\)", script)) != 1:
        problems.append("%s: the candidate loop must decide on "
                        "`PASSED.includes(job.conclusion)` and nothing else."
                        % TAG_ACTION_DIR)
    wf = re.findall(r"^\s*const WORKFLOW = '([^']*)';", script, re.M)
    if wf != ["ci.yml"]:
        problems.append("%s: `const WORKFLOW` is %r; it must be 'ci.yml', whose "
                        "tag runs hold the browser suite." % (TAG_ACTION_DIR, wf))
    jobname = re.findall(r"^\s*const JOB = '([^']*)';", script, re.M)
    for key, job_id in sorted(TAG_DIFFED.items()):
        block = job_block(ci_lines, job_id) if ci_lines else None
        want = scalar(block, "name", 4) if block else None
        if want is None:
            problems.append("ci.yml: no `%s` job with a `name:`; the tag "
                            "action cannot match it. Update TAG_DIFFED."
                            % job_id)
        elif jobname != [want]:
            problems.append(
                "%s: `const JOB` is %r but ci.yml's %s job is named %r. The "
                "action would never find a passed suite, so every tag runs "
                "it." % (TAG_ACTION_DIR, jobname, job_id, want))
    report.append("%s: a browser base is a tag whose %s concluded %s"
                  % (TAG_ACTION_DIR, "/".join(jobname) or "?",
                     "/".join(got)))


def main():
    problems, report = [], []
    ci_lines = None
    for fname in WORKFLOWS:
        path = os.path.join(ROOT, ".github", "workflows", fname)
        try:
            with open(path) as fh:
                lines = fh.read().split("\n")
        except OSError as e:
            problems.append("%s: cannot read (%s)" % (fname, e))
            continue
        if fname == "ci.yml":
            ci_lines = lines
        check_concurrency(fname, lines, problems, report)
        check_base_wiring(fname, lines, problems, report)
    check_base_selection(problems, report)
    check_tag_selection(ci_lines, problems, report)
    if problems:
        e = sys.stderr
        print("check-main-concurrency: FAIL", file=e)
        print("", file=e)
        for p in problems:
            print("  %s" % p, file=e)
        print("", file=e)
        print("See the header of %s for why this matters." % __file__, file=e)
        return 1
    print("check-main-concurrency: OK")
    for line in report:
        print("  %s" % line)
    return 0


if __name__ == "__main__":
    sys.exit(main())
