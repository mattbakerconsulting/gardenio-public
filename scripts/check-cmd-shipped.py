#!/usr/bin/env python3
"""check-cmd-shipped.py

Every directory under gardenio/controller/cmd/ must be shipped or run.

A binary nothing builds still costs: it compiles and lints in every Go job,
counts against the line budget, and its tests run on every controller change.
The code audit (gd-eRFLdnAEeB) found six of them, ~10k lines; this stops the
count growing back (gd-kCs3VDmMWR). The rule is in the repo-root CLAUDE.md.

A cmd/<x> directory passes if ANY of these holds:

  1. An image ships it: images/_controller-base/Dockerfile builds <x> and a
     Dockerfile listed in .github/images.json COPYs /out/<x> from it, or a
     Dockerfile listed there compiles ./cmd/<x> itself.
  2. A workflow runs it: a non-comment line of .github/workflows/*.yml names
     cmd/<x>.
  3. An e2e module runs it: a non-comment line of a file under
     gardenio/tests/e2e/ names cmd/<x>.

A Makefile target does not count: nothing runs it unless a person does.

UNSHIPPED lists the binaries that predate the rule, each with the bead that
wires or deletes it. Do not add to it; a row whose binary now passes, or is
gone, is reported so the row can be dropped.

Usage:  python3 scripts/check-cmd-shipped.py
Exit status: 0 if clean, 1 on any violation.
"""
import importlib.util
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# GARDENIO_ROOT exists for check-cmd-shipped_test.py; nothing else sets it.
ROOT = os.environ.get('GARDENIO_ROOT') or os.path.dirname(HERE)
CMD_DIR = os.path.join(ROOT, 'gardenio', 'controller', 'cmd')
IMAGES_JSON = os.path.join(ROOT, '.github', 'images.json')
BASE_DF = os.path.join(ROOT, 'gardenio', 'images', '_controller-base', 'Dockerfile')
WORKFLOWS = os.path.join(ROOT, '.github', 'workflows')
E2E = os.path.join(ROOT, 'gardenio', 'tests', 'e2e')

UNSHIPPED = {
    'coverage-baselines': 'gd-roE5roXBKQ',
}

COMMENT = re.compile(r'^\s*(#|//)')
PRUNE = frozenset(('node_modules', '.git'))


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def read(path):
    try:
        with open(path, errors='replace') as f:
            return f.read()
    except OSError:
        return ''


def binaries():
    """cmd/ subdirectories holding a non-test Go file."""
    out = []
    for name in sorted(os.listdir(CMD_DIR)):
        d = os.path.join(CMD_DIR, name)
        if os.path.isdir(d) and any(
                f.endswith('.go') and not f.endswith('_test.go')
                for f in os.listdir(d)):
            out.append(name)
    return out


def image_shipped():
    base = _load('ccb', os.path.join(HERE, 'check-controller-base.py'))
    built = base.base_binaries(read(BASE_DF))
    shipped = set()
    for e in json.loads(read(IMAGES_JSON)):
        text = read(os.path.join(ROOT, e['dockerfile']))
        shipped |= {b for b in re.findall(
            r'COPY --from=controller_base /out/([A-Za-z0-9._-]+)', text)
            if b in built}
        shipped |= set(re.findall(r'\./cmd/([A-Za-z0-9._-]+)', text))
    return shipped


def run_text():
    """Non-comment lines of every workflow and every e2e module file."""
    lines = []
    for top in (WORKFLOWS, E2E):
        for dirpath, dirnames, filenames in os.walk(top):
            dirnames[:] = [d for d in dirnames if d not in PRUNE]
            for f in filenames:
                if top == WORKFLOWS and not f.endswith(('.yml', '.yaml')):
                    continue
                lines += [ln for ln in read(os.path.join(dirpath, f)).splitlines()
                          if not COMMENT.match(ln)]
    return '\n'.join(lines)


def main():
    bins = binaries()
    shipped = image_shipped()
    text = run_text()
    problems, stale = [], []
    for b in bins:
        ok = b in shipped or re.search(
            r'cmd/%s(?![A-Za-z0-9_.-])' % re.escape(b), text)
        if ok and b in UNSHIPPED:
            stale.append('%s now ships or runs; drop its UNSHIPPED row (%s)'
                         % (b, UNSHIPPED[b]))
        elif not ok and b not in UNSHIPPED:
            problems.append(
                'gardenio/controller/cmd/%s: no image builds it and no workflow '
                'or e2e module runs it. Add it to images/_controller-base/'
                'Dockerfile and COPY it in a leaf image, run it from '
                '.github/workflows/ or gardenio/tests/e2e/, or delete it.' % b)
    stale += ['%s is gone; drop its UNSHIPPED row (%s)' % (b, bead)
              for b, bead in sorted(UNSHIPPED.items()) if b not in bins]

    for s in stale:
        print('check-cmd-shipped: note: %s' % s)
    if problems:
        print('check-cmd-shipped: FAIL', file=sys.stderr)
        for p in problems:
            print('  %s' % p, file=sys.stderr)
        print('Rule: repo-root CLAUDE.md, "New binaries and guard tests".',
              file=sys.stderr)
        return 1
    print('check-cmd-shipped: OK (%d binaries, %d grandfathered)'
          % (len(bins), sum(1 for b in bins if b in UNSHIPPED)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
