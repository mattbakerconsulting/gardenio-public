#!/usr/bin/env python3
"""check-sigpipe-producers.py

LOUD guard against re-introducing `echo "$x" | grep -q PAT` in shell
(gd-yrZ1WOryGP).

WHY THIS EXISTS
---------------
`grep -q` exits at the FIRST match without draining its input (`grep -m N`
likewise, after N). Whatever is upstream may still be mid-write when that
happens, and a write() to a pipe whose read end is gone fails immediately with
EPIPE -- free buffer space is irrelevant. Under `set -o pipefail` the
producer's status then becomes the pipeline's, so the branch flips even though
grep matched perfectly.

Not theoretical. `shell guards + script self-tests` was the single most common
red on main: 7 failures in 95 runs (7.4%) across the last 100 checks.yml runs,
and SIX of the seven were this one race wearing four different names, which is
why it read as four unrelated flakes:

    a release-bug selector self-test (deleted)     printf: write error: Broken pipe
    scripts/inbox/tarboat-validate_test.sh:171    echo: write error: Broken pipe
    scripts/inbox/acceptance-smoke_test.sh:59     echo: write error: Broken pipe
    scripts/check-e2e-module-failure-gate.sh:94   printf: write error: Broken pipe

Note the fourth: a release GATE, not a test. The same race can red a gate on a
tree that is perfectly fine.

The tell, every time, is that the assertion reports exactly the value it
expected -- `got=[]` where empty is what was asserted. The reader was right;
the WRITER died.

THE FIX IS A HERE-STRING, which has no producer process at all:

    echo "$out"        | grep -q  PAT   ->   grep -q  PAT <<<"$out"
    printf '%s\\n' "$x" | grep -qE PAT   ->   grep -qE PAT <<<"$x"

Equivalent: `echo`, `printf '%s\\n'` and `<<<` all append exactly one newline,
and grep treats a final unterminated line the same either way. 423 sites were
converted in one sweep, each one checked by reconstructing the pipeline from
the rewritten line and requiring it to reproduce the original token for token,
then re-running all 16 affected suites and diffing their output byte for byte.

Do NOT "fix" it with `{ echo ...; } | grep -q` plus `|| true` on the producer.
These are assertion harnesses; suppressing a writer's exit status is how a real
error gets swallowed.

IT HAS ALREADY BEEN REINTRODUCED ONCE -- THREE MINUTES AFTER THIS GUARD LANDED
-----------------------------------------------------------------------------
02e0b37b added `printf '%s' "$out" | grep -qF "$want_msg"` to
scripts/security/gitleaks-history_test.sh and reddened main for two commits
(runs 35690812428, 35691227477). Fixed in 69b3b0da.

That commit message says the author "pushed at 05:28Z against a tree that
didn't have it". THAT IS WRONG, and the true version is the whole reason this
file is a guard and not a paragraph of documentation. 02e0b37b's DIRECT PARENT
is ff188fc2: this script and its checks.yml step were both already in the tree
the line was written in. `git merge-base --is-ancestor ff188fc2 02e0b37b` is
true. It was not a push race.

What actually happened is that someone who had read the whole diagnosis, agreed
with it, and was holding this guard in their working copy wrote the banned
construct anyway -- because they ran the suite they were editing and nothing
else. Two seconds of `python3 scripts/check-sigpipe-producers.py` would have
printed the violation.

So: RUN THE REPO'S GUARDS OVER THE FILES YOU TOUCHED, not just the one test you
edited. Knowing about this race is demonstrably not enough to avoid writing it.

When the input is not a shell variable (a here-string needs one), redirect from
a file: `grep -q PAT < "$f"`. When a script under test may exit WITHOUT reading
stdin at all -- a script that `exit 0`s on a missing title is where this first
bit us -- feed it from a file too, so the fixture stays
byte-exact (a here-string would append a newline, and one case there feeds
deliberately empty stdin).

SCOPE, AND WHAT IS DELIBERATELY NOT COVERED
-------------------------------------------
Only an `echo`/`printf` producer is flagged, because that is the case with a
mechanical, always-correct fix. `<some command> | grep -q ...` is racy for
exactly the same reason, but the fix there is a judgement call: hoisting it to
`out="$(cmd)"` also discards cmd's exit status, which the caller may want.

That uncovered class was surveyed on 2026-09-21. Do not re-derive it:

  244  `cmd | grep -q` sites in tracked shell, minus docs/archive
   80  ... of which are in a script that sets `-o pipefail`   <- rest are inert
   22  ... of which ALSO have a producer that can outlive grep's first match

Without pipefail the producer's EPIPE never reaches the pipeline's status, so
those 164 are noise. Of the 80 that survive, a producer whose ENTIRE output
fits one write() has already finished before grep exits -- `go version`,
`node -v`, `cmd --help`, `head -c 8`. Those cannot lose the race. What is left
is 22 sites whose producer streams or can exceed the 64K pipe buffer: kubectl
logs/exec, `ctr images ls`, curl, scrape_metrics, tmux_capture, `git show`.
They are listed by rerunning the triage in git log for this file.

(Two more look like hits and are not: step-e-repoint.sh:140 is a dry-run
`plan "..."` MESSAGE, not a pipeline, and test-tls-certs.sh:111 is already a
here-string whose `||` a naive scan reads as a pipe. Any future census that
reports 24 has made both mistakes.)

The four worst of the 22 -- `node_exec 30 '...' | grep -q 'RC=0'` in the
gvisor e2e scripts -- were fixed, because there the cost was concrete: the
status drove a 4-attempt retry that re-shipped a multi-MB base64 binary each
time, and four losses failed the module. The other 18 are still open; if one
starts flaking, this header is the explanation.

`docs/archive/` is excluded: 18 sites, archived RWX-era scripts that nothing
runs. Long options (`--quiet`, `--max-count=`) are not matched; nothing in the
tree uses them.

Usage:
    ./scripts/check-sigpipe-producers.py            # scan tracked shell files
    ./scripts/check-sigpipe-producers.py <file..>   # scan specific files

Exit status: 0 if clean, 1 if any producer-into-early-exit-grep site is found.
"""
import re
import subprocess
import sys

# `echo ARG | grep` / `printf '%s' ARG | grep` / `printf '%s\n' ARG | grep`
ARG = r'''(?:"(?:[^"\\]|\\.)*"|'[^']*'|\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*)+'''
PRODUCER = re.compile(
    r"""\b(?:echo|printf\s+'%s(?:\\n)?')\s+(?:""" + ARG + r""")\s*\|\s*grep\b""")
OPT = re.compile(r'\s+(-[A-Za-z]+[0-9]*)')


def code_limit(s):
    """Offset of the `#` that starts a comment, or len(s). Quote-aware, so a
    `#` inside a grep pattern does not truncate the line."""
    q, i = None, 0
    while i < len(s):
        c = s[i]
        if q:
            if c == '\\' and q == '"':
                i += 2
                continue
            if c == q:
                q = None
            i += 1
            continue
        if c in ('"', "'"):
            q = c
        elif c == '\\':
            i += 2
            continue
        elif c == '#' and (i == 0 or s[i - 1].isspace()):
            return i
        i += 1
    return len(s)


def in_quotes(s, pos):
    """Is offset `pos` inside a quoted string? Keeps us off `echo` that is part
    of a quoted script argument, e.g. node_exec 30 '...; echo RC=$?'."""
    q, i = None, 0
    while i < pos and i < len(s):
        c = s[i]
        if q:
            if c == '\\' and q == '"':
                i += 2
                continue
            if c == q:
                q = None
        elif c in ('"', "'"):
            q = c
        elif c == '\\':
            i += 2
            continue
        i += 1
    return q is not None


def exits_early(s, i):
    """True if the grep whose name ends at `i` stops reading before EOF."""
    while True:
        m = OPT.match(s, i)
        if not m:
            return False
        if 'q' in m.group(1) or m.group(1).startswith('-m'):
            return True
        i = m.end()


def scan(path):
    hits = []
    try:
        lines = open(path, encoding='utf-8', errors='replace').read().split('\n')
    except OSError:
        return hits
    for no, line in enumerate(lines, 1):
        limit = code_limit(line)
        for m in PRODUCER.finditer(line):
            if m.start() >= limit or in_quotes(line, m.start()):
                continue
            if exits_early(line, m.end()):
                hits.append((no, line.strip()))
                break
    return hits


def unscanned_untracked():
    """Shell files git knows nothing about, and which this guard therefore
    never read.

    `git ls-files` lists the INDEX, so a brand-new script is invisible to this
    guard until it is staged. That is the worst-timed possible false green: the
    moment you run the guards is the moment before you commit. On 2026-09-23
    this printed `OK (458 shell files)` over four new files it had never opened;
    staged, it reported 462 and FAILED on two real races in them. The verdict
    word was identical both times -- only the COUNT differed. So say out loud
    what was skipped rather than leaving the count as the only clue.

    Not an error: untracked scratch files are normal, and a clean CI checkout
    has none, so this is silent where it should be silent.
    """
    out = subprocess.run(['git', 'ls-files', '--others', '--exclude-standard',
                          '*.sh', '*.bash'],
                         capture_output=True, text=True).stdout
    return [f for f in out.split('\n') if f and not f.startswith('docs/archive/')]


def note_skipped(skipped, stream):
    if not skipped:
        return
    shown = ', '.join(skipped[:5])
    if len(skipped) > 5:
        shown += ', (+%d more)' % (len(skipped) - 5)
    print('check-sigpipe-producers: NOTE -- %d untracked shell file(s) were NOT '
          'scanned' % len(skipped), file=stream)
    print('  (this guard enumerates `git ls-files`, i.e. the index). '
          '`git add` them first: %s' % shown, file=stream)


def main(argv):
    skipped = []
    if argv:
        files = argv
    else:
        out = subprocess.run(['git', 'ls-files', '*.sh', '*.bash'],
                             capture_output=True, text=True).stdout
        files = [f for f in out.split('\n') if f and not f.startswith('docs/archive/')]
        skipped = unscanned_untracked()
    if not files:
        print('check-sigpipe-producers: no shell files to scan')
        note_skipped(skipped, sys.stdout)
        return 0

    hits = [(f, no, txt) for f in files for no, txt in scan(f)]
    if not hits:
        print('check-sigpipe-producers: OK (%d shell files, no producer piped '
              'into an early-exit grep)' % len(files))
        note_skipped(skipped, sys.stdout)
        return 0

    e = sys.stderr
    print('check-sigpipe-producers: FAIL', file=e)
    print('', file=e)
    print('A producer is piped into a grep that stops reading early. When grep exits at', file=e)
    print('the first match the producer takes EPIPE, and `set -o pipefail` turns that into', file=e)
    print('the pipeline\'s status -- so the branch flips even though grep matched. Fix:', file=e)
    print('', file=e)
    print('    echo "$x" | grep -q PAT    ->    grep -q PAT <<<"$x"', file=e)
    print('', file=e)
    print('See the header of %s for the full story.' % __file__, file=e)
    print('', file=e)
    for f, no, txt in hits:
        print('  %s:%d: %s' % (f, no, txt), file=e)
    note_skipped(skipped, e)
    return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
