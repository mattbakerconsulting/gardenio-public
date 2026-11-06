#!/usr/bin/env python3
"""check-retry-lost-runners-gate_test.py

Regression test for the annotation GATE in .github/workflows/retry-lost-runners.yml.

WHY THIS FILE EXISTS
--------------------
That workflow decides whether a red job lost its runner to a spot reclaim (and
should be re-run for free) or failed for real (and must not be). Its classifier
is inline JavaScript in a `github-script` step, so it has never had a harness,
and it has now been WRONG ABOUT THE ANNOTATION SHAPE TWICE:

  1. It knew only the ABRUPT signature ("lost communication with the server")
     and silently dropped every gracefully-drained runner.
  2. Fixed, but the fix assumed a gracefully-drained runner always leaves
     "The operation was canceled.". It does not. On 2026-09-23 three jobs
     carried "Process completed with exit code 130." as their ONLY failure
     annotation, all three had the shutdown line in their log, and all three
     were classified `genuine` and never retried -- one of them on a push to
     main.

  3. A third shape leaves no matching annotation at all: `go test` answers the
     forwarded SIGINT by exiting 1, so the only annotation is "exit code 1.",
     identical to an ordinary failure. Its tell is the job's own conclusion,
     `cancelled` (gd-m0SzimA103, run 41395002610). CANCELLED_FIXTURES below.

Both bugs were invisible because the workflow's own output says `genuine` in a
perfectly cheerful tone either way. Nothing distinguishes "read the log and
concluded it was a real failure" from "never read the log at all".

WHAT IS TESTED, AND WHAT IS DELIBERATELY NOT
--------------------------------------------
The gate is not the verdict. A job's LOG is the verdict -- it either contains
"The runner has received a shutdown signal" or it does not, and a genuine
failure cannot produce that line. The gate only decides whether the log is
worth one HTTP request. So the two things that can actually go wrong are:

  * the gate is too NARROW -- a real reclaim never gets its log read, and is
    dropped. That is bug 2 above, and it costs a manual rerun or a red main.
  * the gate is too WIDE -- log fetches on jobs that were never going to
    match, during exactly the churn storm that caused the reclaim.

So this test asserts, per job, whether the gate FIRES -- not the final verdict,
which needs a log this test does not fetch. It runs offline against real
annotation text captured from the API; no network, no token.

The fixtures are verbatim API output, not hand-written strings. The six
`exit code 1.` rows are the load-bearing negative controls: a sloppier pattern
(`/exit code 1/`) passes every positive case here and fails those six, which is
the whole reason the regex is anchored with an alternation and a word boundary.

The regexes are LIFTED OUT OF THE WORKFLOW by text rather than retyped, so
editing the workflow is what this test sees. A retyped copy would test this
file's typing instead. The workflow is read as text, not parsed as YAML: these
CI runners have no PyYAML.

Usage:  python3 scripts/check-retry-lost-runners-gate_test.py
Exit status: 0 if the gate behaves as specified on every fixture, 1 otherwise.
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
WORKFLOW = os.path.join(HERE, os.pardir, '.github', 'workflows',
                        'retry-lost-runners.yml')

# (job id, failure annotations, gate must fire?, why)
#
# Captured 2026-09-23 from the last 40 runs each of ci, checks, e2e and images
# -- every failed or cancelled job in that window. Sources for the expectations:
# the three exit-130 jobs were confirmed reclaims (shutdown line present in
# their logs, with a positive and a negative grep control on each file); the
# rest are genuine failures or concurrency cancels.
FIXTURES = [
    ('107375983294', ['Process completed with exit code 130.'], True,
     'reclaim, exit-130 only -- run 35918405782 att 1, classified genuine, '
     'human re-ran it by hand'),
    ('107319672783', ['Process completed with exit code 130.'], True,
     'reclaim, exit-130 only -- run 35901753507, a PUSH TO MAIN, still red'),
    ('107374096449', ['Process completed with exit code 130.'], True,
     'reclaim, exit-130 only -- run 35917870753, never retried'),

    ('107345218595',
     ['Canceling since a higher priority waiting request for '
      'images-refs/pull/143/merge exists',
      'The operation was canceled.'], True,
     'concurrency cancel -- gate FIRES (this is the ambiguous shape), and the '
     'log then says no shutdown line, so the verdict is genuine. Pre-existing '
     'behaviour; listed so a future narrowing of the gate cannot drop it '
     'silently.'),

    # A superseded run that never reached the cancellation annotation. Note it
    # is NOT matched by CANCELED: "Canceling since..." and "The operation was
    # canceled." are different strings, and only the second one is the gate.
    ('107312306328',
     ['Canceling since a higher priority waiting request for '
      'images-refs/pull/144/merge exists'], False,
     'superseded by a newer run; no cancellation annotation'),

    # ── negative controls ───────────────────────────────────────────────────
    # Ordinary test failures. `exit code 1.` is a prefix of nothing the gate
    # matches, and proving that is this file's main job: these six are what
    # rule out the tempting `/exit code 1/`.
    ('107316680027', ['Process completed with exit code 1.'], False,
     'genuine failure -- helm lint + chart tests'),
    ('107323085355', ['Process completed with exit code 1.'], False,
     'genuine failure -- helm lint + chart tests'),
    ('107338474976', ['Process completed with exit code 1.'], False,
     'genuine failure -- helm lint + chart tests'),
    ('107348825692', ['Process completed with exit code 1.'], False,
     'genuine failure -- helm lint + chart tests'),
    ('107369268330', ['Process completed with exit code 1.'], False,
     'genuine failure -- viewer-e2e (playwright)'),

    # Failed with no failure annotation at all. Common, and the reason the gate
    # cannot be "no annotation means something went wrong invisibly".
    ('107304383908', [], False, 'genuine failure, no failure annotation'),
    ('107305025552', [], False, 'genuine failure, no failure annotation'),
    ('107309835058', [], False, 'genuine failure, no failure annotation'),
    ('107351105763', [], False, 'genuine failure, no failure annotation'),

    # The ABRUPT shape. Not from this census -- no abrupt loss happened in the
    # window -- so the text is the service wording quoted in the workflow's own
    # header, and it is here to pin that LOST still matches and that an abrupt
    # loss is handled BEFORE the gate (the gate must not be what catches it).
    ('synthetic-abrupt',
     ['The self-hosted runner: gardenio-k3s-xxxxx-runner-yyyyy lost '
      'communication with the server. Verify the machine is running and has a '
      'healthy network connection.'], False,
     'abrupt loss -- caught by LOST before the gate is consulted at all'),
]

# Jobs whose OWN conclusion is `cancelled` -- reachable only on a run that
# concluded `cancelled`. (job id, runner name, failure annotations, gate must
# fire?, why). The annotation gate alone stays shut on the first row; the
# cancelled-on-a-runner clause is what opens it.
CANCELLED_FIXTURES = [
    ('125114628608', 'gardenio-k3s-medium-574dd-runner-q9njz',
     ['Process completed with exit code 1.'], True,
     'reclaim, exit-1 only -- run 41395002610 att 1, helm lint + chart tests; '
     'go test exited 1 on SIGINT, shutdown line in the log, read genuine'),
    ('synthetic-queued-cancel', None, [], False,
     'cancelled before a runner took it (mass cancel) -- no runner, no log, '
     'no fetch'),
]

PASS = FAIL = 0


def record(name, ok, detail=''):
    global PASS, FAIL
    if ok:
        print('ok   %s' % name)
        PASS += 1
    else:
        print('FAIL %s' % name)
        if detail:
            for line in detail.split('\n'):
                print('       | %s' % line)
        FAIL += 1


def lift(src, name):
    """Pull a `const NAME = /pattern/flags;` literal out of the workflow.

    Raises if it is absent: a renamed or deleted constant must break this test
    loudly rather than leave it passing over a regex it invented itself.
    """
    m = re.search(r'const %s = /(.*?)/([a-z]*);' % name, src)
    if not m:
        raise SystemExit('check-retry-lost-runners-gate: could not find '
                         '`const %s = /.../` in %s -- if it was renamed, '
                         'update this test rather than deleting it.'
                         % (name, WORKFLOW))
    pattern, flags = m.group(1), m.group(2)
    # These patterns are in the intersection of JS and Python regex syntax.
    # Anything outside it would translate silently and wrongly, so refuse.
    for bad, why in ((r'(?<', 'lookbehind'), (r'\\p{', 'unicode property'),
                     (r'(?<name>', 'JS named group')):
        if bad in pattern:
            raise SystemExit('check-retry-lost-runners-gate: %s uses %s, which '
                             'this test cannot translate to Python re. Port '
                             'the check rather than trusting it.' % (name, why))
    return re.compile(pattern, re.I if 'i' in flags else 0)


def main():
    with open(WORKFLOW, encoding='utf-8') as f:
        src = f.read()

    LOST = lift(src, 'LOST')
    SHUTDOWN = lift(src, 'SHUTDOWN')
    CANCELED = lift(src, 'CANCELED')
    INTERRUPTED = lift(src, 'INTERRUPTED')
    print('lifted from %s:' % os.path.relpath(WORKFLOW, os.path.join(HERE, os.pardir)))
    for n, r in (('LOST', LOST), ('SHUTDOWN', SHUTDOWN),
                 ('CANCELED', CANCELED), ('INTERRUPTED', INTERRUPTED)):
        print('  %-12s %s' % (n, r.pattern))
    print('')

    # The gate is a source line, not a constant. Pin that it consults BOTH
    # annotation patterns: a future "simplification" back to CANCELED alone is
    # exactly the regression this file exists to catch, and it would otherwise
    # leave every fixture below passing (the expectations are about the gate,
    # and a narrowed gate simply stops firing on rows that assert it does).
    gate = re.search(r'if \(!lost && \((.*?)\)\) \{', src, re.S)
    record('the gate is still `if (!lost && (...))` before the log fetch',
           gate is not None,
           'could not find the `if (!lost && (...))` line')
    if gate:
        gate_src = gate.group(1)
        for name in ('cancelledOnRunner', 'CANCELED', 'INTERRUPTED'):
            record('the gate consults %s' % name, name in gate_src,
                   'gate is: %s' % gate_src)
    # The Python model of that clause below is retyped, so pin its source.
    record('cancelledOnRunner is still a cancelled job that had a runner',
           "const cancelledOnRunner = job.conclusion === 'cancelled' && "
           "!!job.runner_name;" in src,
           'the model of the clause in this test no longer matches the workflow')

    # And that the log, not the annotation, remains the verdict.
    record('SHUTDOWN is tested against the LOG, not against an annotation',
           re.search(r'SHUTDOWN\.test\(await downloadLog\(', src) is not None,
           'the verdict must stay `SHUTDOWN.test(await downloadLog(job.id))`; '
           'if that moved, this test is checking the wrong thing')

    # The judge job (ci / checks aggregate) is recognised by its step name. If
    # that step is renamed in ci.yml or checks.yml and not here, every judge
    # reads as genuine again and every lost-runner run of those workflows is
    # SKIPped -- gd-RiDlMItXQq, silent in exactly the way described above.
    m = re.search(r"const JUDGE_STEP = '([^']+)';", src)
    record('JUDGE_STEP is still a string constant in the workflow',
           m is not None, "could not find `const JUDGE_STEP = '...';`")
    if m:
        judge_step = m.group(1)
        for wf in ('ci.yml', 'checks.yml'):
            path = os.path.join(HERE, os.pardir, '.github', 'workflows', wf)
            with open(path, encoding='utf-8') as f:
                named = re.search(r'^\s*- name: %s\s*$' % re.escape(judge_step),
                                  f.read(), re.M) is not None
            record('%s still has a step named %r' % (wf, judge_step), named,
                   'rename JUDGE_STEP in retry-lost-runners.yml to match the '
                   'aggregate job step in %s' % wf)
    record('the classifier loops over the non-judge jobs, not all failed jobs',
           re.search(r'for \(const job of judged\)', src) is not None,
           'expected `for (const job of judged)`; a loop over `failed` counts '
           'the judge as genuine')

    fires = 0
    for job, msgs, want, why in FIXTURES:
        abrupt = any(LOST.search(m) for m in msgs)
        got = (not abrupt) and any(CANCELED.search(m) or INTERRUPTED.search(m)
                                   for m in msgs)
        if got:
            fires += 1
        record('job %s: gate %s -- %s'
               % (job, 'fires' if want else 'stays shut', why),
               got == want,
               'annotations: %r\n  expected gate=%s, got %s' % (msgs, want, got))

    for job, runner, msgs, want, why in CANCELLED_FIXTURES:
        abrupt = any(LOST.search(m) for m in msgs)
        got = (not abrupt) and (bool(runner) or any(
            CANCELED.search(m) or INTERRUPTED.search(m) for m in msgs))
        record('cancelled job %s: gate %s -- %s'
               % (job, 'fires' if want else 'stays shut', why),
               got == want,
               'runner %r, annotations: %r\n  expected gate=%s, got %s'
               % (runner, msgs, want, got))

    # A gate that fires on everything is as useless as one that fires on
    # nothing, and both would pass a test that only checked the positives.
    record('the gate does not fire on every fixture',
           fires < len(FIXTURES),
           'fired on all %d -- it is not discriminating at all' % fires)
    record('the gate fires on at least one fixture', fires > 0,
           'fired on none -- it can no longer catch anything')

    # A self-test nothing invokes is a docstring. checks.yml lists its tests
    # explicitly -- there is no glob -- so a new file runs only if someone
    # remembered to add the line, and forgetting is silent in both directions.
    checks = os.path.join(HERE, os.pardir, '.github', 'workflows', 'checks.yml')
    try:
        with open(checks, encoding='utf-8') as f:
            wired = os.path.basename(__file__) in f.read()
    except OSError as e:
        wired, checks = False, '%s (%s)' % (checks, e)
    record('this self-test is itself in the script self-tests list', wired,
           'add `python3 scripts/%s` to the script self-tests step in %s'
           % (os.path.basename(__file__), checks))

    print('')
    print('gate fired on %d of %d fixtures' % (fires, len(FIXTURES)))
    print('check-retry-lost-runners-gate: %d passed, %d failed' % (PASS, FAIL))
    return 1 if FAIL else 0


if __name__ == '__main__':
    sys.exit(main())
