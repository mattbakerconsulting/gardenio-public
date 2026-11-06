#!/usr/bin/env python3
"""check-guard-provenance_test.py — unit tests for check-guard-provenance.py

Each case builds a synthetic repo in a tmpdir and asserts the guard's verdict.
GARDENIO_ROOT points the guard at it.

Usage:  python3 scripts/check-guard-provenance_test.py
"""
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-guard-provenance.py')

PASS = FAIL = 0


def check(name, files, want_hit, want_substr=None):
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix='cgp-test-')
    try:
        for rel, text in files.items():
            p = os.path.join(root, rel)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            with open(p, 'w') as f:
                f.write(text)
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


CITED = '# protects against gd-AbCdEfGhIj\n'
CITED_KA = '// regression for ka-0000001234\n'

# ── passes ─────────────────────────────────────────────────────────────────
check('script guard cites a gd- bead', {'scripts/check-x.sh': CITED}, False)
check('Go guard cites a ka- bead',
      {'kbeads/internal/server/x_guard_test.go': CITED_KA}, False)
check('a _test file of a guard script needs no citation',
      {'scripts/check-x.sh': CITED, 'scripts/check-x_test.sh': 'true\n'}, False)
check('an ordinary Go test needs no citation',
      {'gardenio/controller/internal/x/x_test.go': 'package x\n'}, False)
check('check-* outside scripts/ is not a guard',
      {'gardenio/images/agent/check-x.sh': 'true\n'}, False)

# ── fails ──────────────────────────────────────────────────────────────────
check('script guard with no citation', {'scripts/check-x.sh': 'true\n'},
      True, 'scripts/check-x.sh: a guard test that cites no bead')
check('nested lint script with no citation',
      {'scripts/security/lint-x.py': 'pass\n'}, True, 'lint-x.py')
check('gardenio/scripts guard with no citation',
      {'gardenio/scripts/check-y.sh': 'true\n'}, True, 'check-y.sh')
check('drift test with no citation',
      {'kbeads/internal/server/schema_drift_test.go': 'package server\n'},
      True, 'schema_drift_test.go')
check('invariant test with no citation',
      {'gardenio/controller/internal/x/order_invariant_test.go': 'package x\n'},
      True, 'order_invariant_test.go')
check('a malformed id is not a citation',
      {'scripts/check-x.sh': '# see gd-short\n'}, True, 'check-x.sh')

# ── grandfathered rows ─────────────────────────────────────────────────────
check('a grandfathered guard passes uncited',
      {'scripts/lint-onboarding.sh': 'true\n'}, False)
check('a grandfathered guard that now cites a bead is reported',
      {'scripts/lint-onboarding.sh': CITED}, False, 'drop its UNCITED row')

print('\n%d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
