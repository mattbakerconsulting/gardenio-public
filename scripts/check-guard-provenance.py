#!/usr/bin/env python3
"""check-guard-provenance.py

Every guard, lint, drift or invariant test must cite the bead it protects
against (gd-kCs3VDmMWR; rule in the repo-root CLAUDE.md). A guard with no
incident behind it is a test nobody can judge or retire, and they pile up:
the code audit (gd-eRFLdnAEeB) found guard tests that re-asserted what a unit
test beside them already proved.

What counts as a guard, by file name:
  * scripts/**/check-*.{sh,py} and scripts/**/lint-*.{sh,py} (also under
    gardenio/scripts/), not their _test files;
  * any *_test.go whose name contains guard, drift or invariant.

A citation is a bead id anywhere in the file: gd- plus ten letters or digits,
or ka- plus ten digits. The script cannot check the other half of the rule
(why a normal unit test would not do); review does.

UNCITED lists the guards that predate the rule. Do not add to it; a row whose
file now cites a bead, or is gone, is reported so the row can be dropped.

Usage:  python3 scripts/check-guard-provenance.py
Exit status: 0 if clean, 1 on any violation.
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# GARDENIO_ROOT exists for check-guard-provenance_test.py; nothing else sets it.
ROOT = os.environ.get('GARDENIO_ROOT') or os.path.dirname(HERE)

UNCITED = frozenset((
    'gardenio/controller/internal/spawnguard/spawnguard_test.go',
    'scripts/lint-onboarding.sh',
))

PRUNE = frozenset(('.git', 'node_modules', 'target', 'vendor', '.terraform'))
SCRIPT = re.compile(r'^(check|lint)-.*\.(sh|py)$')
GO_GUARD = re.compile(r'(guard|drift|invariant).*_test\.go$')
BEAD = re.compile(r'\b(gd-[A-Za-z0-9]{10}|ka-[0-9]{10})\b')


def is_guard(rel):
    name = os.path.basename(rel)
    if name.endswith('_test.go'):
        return bool(GO_GUARD.search(name))
    if re.search(r'_test\.(sh|py)$', name):
        return False
    return bool(SCRIPT.match(name)) and rel.startswith(('scripts/', 'gardenio/scripts/'))


def guards():
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = sorted(d for d in dirnames if d not in PRUNE)
        for f in sorted(filenames):
            rel = os.path.relpath(os.path.join(dirpath, f), ROOT).replace(os.sep, '/')
            if is_guard(rel):
                yield rel


def main():
    problems, stale, n = [], [], 0
    seen = set()
    for rel in guards():
        n += 1
        seen.add(rel)
        with open(os.path.join(ROOT, rel), errors='replace') as f:
            cited = bool(BEAD.search(f.read()))
        if cited and rel in UNCITED:
            stale.append('%s now cites a bead; drop its UNCITED row' % rel)
        elif not cited and rel not in UNCITED:
            problems.append('%s: a guard test that cites no bead. Name the incident '
                            'or bead it protects against (gd-... / ka-...), and say '
                            'why a normal unit test would not do.' % rel)
    stale += ['%s is gone; drop its UNCITED row' % r for r in sorted(UNCITED - seen)]

    for s in stale:
        print('check-guard-provenance: note: %s' % s)
    if problems:
        print('check-guard-provenance: FAIL', file=sys.stderr)
        for p in problems:
            print('  %s' % p, file=sys.stderr)
        print('Rule: repo-root CLAUDE.md, "New binaries and guard tests".',
              file=sys.stderr)
        return 1
    print('check-guard-provenance: OK (%d guards, %d grandfathered)'
          % (n, len(UNCITED & seen)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
