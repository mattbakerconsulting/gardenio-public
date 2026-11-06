#!/usr/bin/env python3
"""check-cmd-shipped_test.py — unit tests for check-cmd-shipped.py

Each case builds a synthetic repo in a tmpdir and asserts the guard's verdict.
GARDENIO_ROOT points the guard at it. The positive control is `a new binary
nothing builds or runs`: it is what a PR adding an unbuilt cmd/<x> looks like,
and it must fail.

Usage:  python3 scripts/check-cmd-shipped_test.py
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-cmd-shipped.py')

PASS = FAIL = 0

BASE = """FROM golang:1.25-bookworm AS builder
RUN for bin in \\
      alpha; \\
    do \\
      go build -o "/out/${bin}" "./cmd/${bin}/" || exit 1; \\
    done
"""
LEAF = 'FROM ${CONTROLLER_BASE} AS controller_base\nCOPY --from=controller_base /out/alpha /alpha\n'


def write(root, rel, text):
    p = os.path.join(root, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, 'w') as f:
        f.write(text)


def check(name, bins, files, want_hit, want_substr=None):
    """bins: cmd dirs to create; files: extra {relpath: text}."""
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix='ccs-test-')
    try:
        write(root, 'gardenio/images/_controller-base/Dockerfile', BASE)
        write(root, 'gardenio/images/alpha/Dockerfile', LEAF)
        write(root, '.github/images.json', json.dumps([
            {'image': 'alpha', 'context': 'gardenio',
             'dockerfile': 'gardenio/images/alpha/Dockerfile'}]))
        write(root, '.github/workflows/ci.yml', 'on: push\n')
        os.makedirs(os.path.join(root, 'gardenio/tests/e2e'))
        for b in ['alpha'] + bins:
            write(root, 'gardenio/controller/cmd/%s/main.go' % b, 'package main\n')
        for rel, text in files.items():
            write(root, rel, text)

        env = dict(os.environ, GARDENIO_ROOT=root)
        p = subprocess.run([sys.executable, GUARD], capture_output=True,
                           text=True, env=env)
        out = p.stdout + p.stderr
        ok = (p.returncode != 0) == want_hit
        if ok and want_substr and want_substr not in out:
            ok = False
            out += '\n(expected output to contain %r)' % want_substr
        if ok:
            print('PASS: %s' % name)
            PASS += 1
        else:
            print('FAIL: %s: expected %s, got rc=%d\n%s'
                  % (name, 'a hit' if want_hit else 'no hit', p.returncode, out))
            FAIL += 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


# ── passes ─────────────────────────────────────────────────────────────────
check('shipped through controller-base and a leaf COPY', [], {}, False)
check('compiled by a leaf Dockerfile itself', ['zeta'],
      {'gardenio/images/alpha/Dockerfile': LEAF + 'RUN go build -o /z ./cmd/zeta/\n'},
      False)
check('run by a workflow', ['zeta'],
      {'.github/workflows/ci.yml': 'jobs:\n  x:\n    steps:\n'
       '      - run: go run ./cmd/zeta\n'}, False)
check('run by an e2e module', ['zeta'],
      {'gardenio/tests/e2e/scripts/test-zeta.sh': 'go build -o /tmp/z ./cmd/zeta/\n'},
      False)
check('a dir with only test files is not a binary', [],
      {'gardenio/controller/cmd/helpers/x_test.go': 'package helpers\n'}, False)

# ── the positive control: a new binary nothing builds or runs ──────────────
check('a new binary nothing builds or runs', ['zeta'], {}, True,
      'cmd/zeta: no image builds it')
check('named only in a workflow comment', ['zeta'],
      {'.github/workflows/ci.yml': '# TODO: run cmd/zeta here\non: push\n'},
      True, 'cmd/zeta')
check('named only in an e2e comment', ['zeta'],
      {'gardenio/tests/e2e/scripts/t.sh': '# cmd/zeta is not run yet\n'},
      True, 'cmd/zeta')
check('a longer name does not count for a prefix', ['zeta'],
      {'.github/workflows/ci.yml': 'run: go run ./cmd/zeta-probe\n'},
      True, 'cmd/zeta')
check('built by the base but COPYed by no leaf', ['beta'],
      {'gardenio/images/_controller-base/Dockerfile':
       BASE.replace('alpha;', 'alpha beta;')}, True, 'cmd/beta')
check('built by a Makefile only', ['zeta'],
      {'Makefile': 'build:\n\tgo build ./cmd/zeta/\n'}, True, 'cmd/zeta')

# ── grandfathered rows ─────────────────────────────────────────────────────
check('a grandfathered binary passes', ['coverage-baselines'], {}, False)
check('a grandfathered row that now ships is reported', ['coverage-baselines'],
      {'.github/workflows/ci.yml': 'run: go run ./cmd/coverage-baselines\n'},
      False, 'drop its UNSHIPPED row')

print('\n%d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
