#!/usr/bin/env python3
"""workflow-changed-jobs.py

Report WHICH JOB BLOCKS of a GitHub Actions workflow differ between two
revisions of the file, so a path gate can stop treating "the workflow file
changed" as "every gated job must run".

WHY THIS EXISTS
---------------
Every filter in checks.yml's `changed-path gate` opens with
`- '.github/workflows/checks.yml'`. That self-reference is correct in
intent -- if you change how `golangci-lint` runs, `golangci-lint` must run --
and far too coarse in effect, because checks.yml is ONE file defining twelve
jobs. Editing a comment on the `security` job re-runs `helm lint + chart
tests` and `cargo audit + deny`.

Measured on main, 2026-09-22:

  * 40 of the 408 non-merge commits in the last 7 days touch checks.yml
    (~6/day, ~10% of all commits).
  * In 13 of the last 14 of those, NO gated path was touched at all -- the
    workflow file alone fired every filter. Commit 1b51d297 is the worked
    example: it changed `.github/workflows/checks.yml` and
    `gardenio/helm/ci/README.md`, and the gate's own log shows all four
    filters true with the SAME matching file, checks.yml, listed under each.
  * Mean heavy-tier exec time of the four jobs that fan out:
        cargo audit + deny (coop)            347.2 s
        golangci-lint (gardenio/controller)  250.2 s
        helm lint + chart tests              236.7 s
        golangci-lint (kbeads)               110.0 s
    = 944 s, ~15.7 heavy-runner-minutes per checks.yml-only commit, or
    ~94 heavy-minutes/day, on the tier measured pinned at its cap 65% of the
    time. Slot-claims, not node-minutes, are what this pool's queue wait is
    made of (gardenio/helm/ci/README.md).

This is a BURST-SHAPE fix, not a capacity one, and deliberately so: the
recorded finding for this pool is that raising the ceiling does not move
unmet demand, and that the shape of each arrival does.

WHAT IT DOES
------------
Splits both revisions of a workflow into (preamble, {job: block}, postamble)
and reports the jobs whose block differs byte-for-byte. Everything that is not
a job block -- `name`, `on`, `concurrency`, top-level `env` -- is compared as
one unit, and ANY difference there fails open, because a top-level `env` or
`on` change can alter a job whose own block is untouched.

FAILING OPEN IS THE POINT, exactly as it is for the gate that consumes this.
`scope=ALL` is printed, and every caller must run everything, whenever:

  * either file is missing or unreadable;
  * either file does not parse into a recognisable jobs mapping;
  * the two revisions declare different SETS of job names (a rename, an
    addition or a deletion -- the mapping between old and new blocks is then
    not well defined, so no narrowing is safe);
  * anything outside the jobs mapping differs;
  * a job named by `--fail-open-on` differs. This is how a caller declares a
    job that OTHER jobs depend on. checks.yml passes `--fail-open-on changes`,
    because all four gated jobs `needs: changes` and read its outputs, so a
    change to the gate itself can alter a gated job whose own block is
    identical.

NO YAML PARSER. checks.yml's `shell guards` job has no pip step and the runner
image ships no PyYAML -- `import yaml` killed a guard here at import once
already. This splits lexically instead, which is sound here for a
reason worth stating: a job key is the only construct that can appear at
exactly two-space indent inside the jobs mapping. Block scalars (`run: |`) are
indented under a key that is itself at four or more, so no line of shell can
impersonate a job header without breaking the YAML first. The splitter still
refuses to guess: anything it does not recognise raises and fails open.

COMMENTS ATTACH FORWARD. A comment or blank line at the boundary between two
jobs is attributed to the job that FOLLOWS it, so editing the long comment
above `gitleaks:` marks `gitleaks` changed. That is arbitrary but safe in one
direction only, which is the direction that matters: the worst case is that a
comment edit marks the neighbouring job changed and it runs for nothing. A
comment can never change a job's behaviour, so no attribution of a comment can
cause a job to be skipped that should have run.

USAGE
-----
    workflow-changed-jobs.py [--fail-open-on JOB]... BASE_FILE HEAD_FILE

Prints, on stdout:

    scope=ALL
    reason=<one line>

or:

    scope=JOBS
    changed=<job>            (zero or more, in head file order)

Exit status is 0 for both -- this is a reporter, not a guard. Exit 2 is a
usage error, which callers must also treat as fail-open.
"""

import re
import sys

JOBS_LINE = re.compile(r"^jobs:[ \t]*(#.*)?$")
JOB_KEY = re.compile(r"^  ([A-Za-z_][A-Za-z0-9_.-]*):[ \t]*(#.*)?$")


class Unparsable(Exception):
    """The file does not have the shape this splitter is willing to trust."""


def _is_blank_or_comment(line):
    stripped = line.strip()
    return not stripped or stripped.startswith("#")


def split_workflow(text):
    """Split a workflow into (preamble, {job: block}, postamble, order).

    Raises Unparsable rather than guessing. Every raise here is a fail-open
    for the caller.
    """
    lines = text.splitlines(keepends=True)

    jobs_at = [i for i, line in enumerate(lines) if JOBS_LINE.match(line)]
    if len(jobs_at) != 1:
        raise Unparsable(
            "expected exactly one top-level `jobs:` line, found %d" % len(jobs_at)
        )
    start = jobs_at[0]

    # The jobs mapping runs to the next line that starts a new top-level key.
    # A column-0 comment is left inside the mapping rather than treated as its
    # end, because ending the mapping early would silently truncate the last
    # job's block into the postamble and compare it as top-level content.
    end = len(lines)
    for i in range(start + 1, len(lines)):
        line = lines[i]
        if line.strip() and not line[0].isspace() and not line.lstrip().startswith("#"):
            end = i
            break

    blocks = {}
    order = []
    current = None
    pending = []

    for line in lines[start + 1 : end]:
        match = JOB_KEY.match(line)
        if match:
            current = match.group(1)
            if current in blocks:
                raise Unparsable("duplicate job key %r" % current)
            order.append(current)
            blocks[current] = pending + [line]
            pending = []
            continue
        if current is None:
            if not _is_blank_or_comment(line):
                raise Unparsable(
                    "content under `jobs:` before the first job key: %r"
                    % line.rstrip("\n")
                )
            pending.append(line)
            continue
        if _is_blank_or_comment(line):
            pending.append(line)
            continue
        blocks[current].extend(pending)
        pending = []
        blocks[current].append(line)

    # Trailing blanks/comments after the last job belong to that last job.
    # There is no "current is None" survivor here: that case raised above.
    if current is not None and pending:
        blocks[current].extend(pending)

    if not blocks:
        raise Unparsable("no job keys found under `jobs:`")

    preamble = "".join(lines[: start + 1])
    postamble = "".join(lines[end:])
    return preamble, {k: "".join(v) for k, v in blocks.items()}, postamble, order


def compare(base_text, head_text, fail_open_on=()):
    """Return (scope, payload).

    scope is "ALL" with a reason string, or "JOBS" with a list of job names.
    """
    try:
        base_pre, base_jobs, base_post, _ = split_workflow(base_text)
        head_pre, head_jobs, head_post, head_order = split_workflow(head_text)
    except Unparsable as exc:
        return "ALL", "unparsable workflow: %s" % exc

    if set(base_jobs) != set(head_jobs):
        added = sorted(set(head_jobs) - set(base_jobs))
        removed = sorted(set(base_jobs) - set(head_jobs))
        return "ALL", "job set changed (added: %s; removed: %s)" % (
            ",".join(added) or "-",
            ",".join(removed) or "-",
        )

    if base_pre != head_pre or base_post != head_post:
        return "ALL", "content outside the jobs mapping changed (on/env/concurrency)"

    changed = [name for name in head_order if base_jobs[name] != head_jobs[name]]

    for name in fail_open_on:
        if name in changed:
            return "ALL", "%s changed, and gated jobs depend on its outputs" % name

    return "JOBS", changed


def main(argv):
    fail_open_on = []
    args = []
    i = 0
    while i < len(argv):
        if argv[i] == "--fail-open-on":
            if i + 1 >= len(argv):
                sys.stderr.write("--fail-open-on needs a job name\n")
                return 2
            fail_open_on.append(argv[i + 1])
            i += 2
            continue
        args.append(argv[i])
        i += 1

    if len(args) != 2:
        sys.stderr.write(__doc__.strip().split("USAGE\n-----\n", 1)[-1] + "\n")
        return 2

    texts = []
    for path in args:
        try:
            with open(path, "r", encoding="utf-8") as handle:
                texts.append(handle.read())
        except OSError as exc:
            print("scope=ALL")
            print("reason=cannot read %s: %s" % (path, exc.strerror or exc))
            return 0

    scope, payload = compare(texts[0], texts[1], fail_open_on)
    if scope == "ALL":
        print("scope=ALL")
        print("reason=%s" % payload)
    else:
        print("scope=JOBS")
        for name in payload:
            print("changed=%s" % name)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
