#!/usr/bin/env python3
"""check-sigpipe-producers_test.py — unit tests for check-sigpipe-producers.py

The guard is a line scanner, so the cases that matter are the ones a plain
regex gets WRONG. Both classes below were observed on the real tree while the
guard was still written in bash:

  * a quoted `echo ... | grep -q` inside another command's single-quoted script
    argument (three sites in gardenio/tests/e2e/scripts/test-gvisor-*.sh of the
    form `node_exec 30 '... ; echo RC=$?' | grep -q 'RC=0'`) -- the INNER
    pipeline runs on a remote node, is not this file's shell, and must not be
    flagged;
  * a COMMENT that documents the very pattern being banned (two sites in
    test-triggers.sh) -- a guard that cannot read its own documentation
    without failing is useless.

Usage:  python3 scripts/check-sigpipe-producers_test.py
"""
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-sigpipe-producers.py')

PASS = FAIL = 0


def check(name, body, want_hit):
    """Run the guard over a one-file fixture; want_hit=True means it must flag."""
    global PASS, FAIL
    with tempfile.NamedTemporaryFile('w', suffix='.sh', delete=False) as f:
        f.write(body + '\n')
        path = f.name
    try:
        p = subprocess.run([sys.executable, GUARD, path],
                           capture_output=True, text=True)
        got_hit = p.returncode == 1
        if got_hit == want_hit:
            print('PASS: %s' % name)
            PASS += 1
        else:
            print('FAIL: %s: expected %s, got rc=%d\n%s'
                  % (name, 'a hit' if want_hit else 'no hit',
                     p.returncode, p.stdout + p.stderr))
            FAIL += 1
    finally:
        os.unlink(path)


# ── MUST FLAG: a real producer into a grep that stops reading early ─────────
check('echo into grep -q', 'echo "$out" | grep -q PAT', True)
check('echo into grep -qE', 'echo "$out" | grep -qE "a|b"', True)
check('echo into grep -q, single quotes', "echo '$out' | grep -q 'PAT'", True)
check('printf %s into grep -q', """printf '%s' "$out" | grep -q PAT""", True)
check('printf %s\\n into grep -q', r"""printf '%s\n' "$out" | grep -q PAT""", True)
check('braced var', 'echo "${out}" | grep -q PAT', True)
check('bare var', 'echo $out | grep -q PAT', True)
check('inside an if', 'if echo "$out" | grep -q PAT; then :; fi', True)
check('grep -m1 also exits early', 'echo "$out" | grep -m1 PAT', True)
check('option bundle with q', 'echo "$out" | grep -qiE PAT', True)
check('-q after another option', 'echo "$out" | grep -i -q PAT', True)
check('second site on the line only',
      'echo "$a" | grep -o X; echo "$b" | grep -q Y', True)

# ── MUST NOT FLAG ───────────────────────────────────────────────────────────
# grep without -q/-m drains its input; there is no early exit, so no race.
check('grep -o drains', 'echo "$out" | grep -o PAT', False)
check('grep -c drains', 'echo "$out" | grep -c PAT', False)
check('grep -iE drains', 'echo "$out" | grep -iE PAT', False)
check('plain grep drains', 'echo "$out" | grep PAT', False)

# The fixed form. If this ever flags, the guard is telling people to un-fix.
check('here-string is the fix', 'grep -q PAT <<<"$out"', False)
check('file redirect is the fix', 'grep -q PAT < "$f"', False)

# A comment describing the banned pattern must not trip the guard.
check('comment mentioning the pattern',
      '# Use a here-string rather than `echo "$out" | grep -qF` -- see below.',
      False)
check('trailing comment after real code',
      'grep -q PAT <<<"$out"  # was: echo "$out" | grep -q PAT', False)

# `echo` inside ANOTHER command\'s quoted script argument belongs to a
# different shell (a remote node, a container, a here-doc body).
check('echo inside a single-quoted argument',
      """node_exec 30 'probe 2>&1 | grep -q "x"; echo RC=$?' | grep -q 'RC=0'""",
      False)
check('echo inside a double-quoted argument',
      '''ssh host "echo $v | grep -q PAT"''', False)

# A `#` inside a grep PATTERN is not a comment; the scanner must not truncate
# the line there and miss a site that follows.
check('hash inside a pattern, site after it',
      """grep -c '#' <<<"$a"; echo "$b" | grep -q Z""", True)


# ── the guard must say what it did NOT scan ─────────────────────────────────
# The guard enumerates `git ls-files`, so an unstaged script is invisible to
# it. On 2026-09-23 it printed `OK (458 shell files)` over four brand-new files
# it had never opened; staged, it reported 462 and FAILED on two real races in
# them. The verdict word was identical in both runs -- only the count differed.
# So it now names the untracked files it skipped, and that note is load-bearing
# enough to test: without it, the false green is silent again.
#
# Both directions are pinned. A note that always appeared would be as useless
# as one that never did.

def record(name, ok, detail=''):
    global PASS, FAIL
    if ok:
        print('PASS: %s' % name)
        PASS += 1
    else:
        print('FAIL: %s\n%s' % (name, detail))
        FAIL += 1


def check_untracked_note():
    repo = tempfile.mkdtemp()
    git = ['git', '-c', 'user.email=t@example.invalid', '-c', 'user.name=t',
           '-c', 'commit.gpgsign=false']
    run = lambda *a: subprocess.run(git + ['-C', repo] + list(a),
                                    capture_output=True, text=True)
    try:
        run('init', '--quiet', '-b', 'main')
        with open(os.path.join(repo, 'tracked.sh'), 'w') as f:
            f.write('#!/bin/sh\ngrep -q PAT <<<"$x"\n')
        run('add', 'tracked.sh')
        run('commit', '--quiet', '-m', 'seed')

        guard = lambda: subprocess.run([sys.executable, GUARD], cwd=repo,
                                       capture_output=True, text=True)

        p = guard()
        record('clean tree: no untracked note',
               p.returncode == 0 and 'NOT scanned' not in p.stdout + p.stderr,
               'rc=%d\n%s' % (p.returncode, p.stdout + p.stderr))

        # An untracked file with a REAL violation: still not enforced (it is
        # outside the guard's contract) but it must not be silently skipped.
        with open(os.path.join(repo, 'untracked.sh'), 'w') as f:
            f.write('#!/bin/sh\necho "$out" | grep -q PAT\n')
        p = guard()
        out = p.stdout + p.stderr
        record('untracked file is named as NOT scanned',
               p.returncode == 0 and 'NOT scanned' in out and 'untracked.sh' in out,
               'rc=%d\n%s' % (p.returncode, out))
    finally:
        shutil.rmtree(repo, ignore_errors=True)


check_untracked_note()

print('')
print('check-sigpipe-producers: %d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
