#!/usr/bin/env python3
"""check-controller-base_test.py — unit tests for check-controller-base.py

Each case builds a synthetic repo in a tmpdir (a .github/images.json, a
_controller-base Dockerfile, and one consumer Dockerfile) and asserts the
guard's verdict. GARDENIO_ROOT points the guard at it.

The case that matters most is `unflagged, self-compiles a base binary`: that
is the drift the guard exists for, and it is silent -- the image builds fine,
it just does the shared compile a second time on its own node. Run against the
real tree at the commit before the migration, the guard names exactly the
fourteen images that had it.

The mirror-image case matters nearly as much: an image whose build context is
the repo root (images/agent/) legitimately compiles its own Go binaries,
because it needs them inside a nine-stage image next to a Rust toolchain and
Playwright. It must NOT be flagged, and it is exempted by the context rule
rather than by a name list -- a name list is the same drift mechanism the
migration removed from images.yml.

Usage:  python3 scripts/check-controller-base_test.py
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, 'check-controller-base.py')

PASS = FAIL = 0

BASE_TEMPLATE = """FROM golang:1.25-bookworm AS builder
WORKDIR /build
COPY controller/ ./
RUN mkdir -p /out && \\
    for bin in \\
      %s; \\
    do \\
      CGO_ENABLED=0 go build -o "/out/${bin}" "./cmd/${bin}/" || exit 1; \\
    done
%s
FROM scratch AS artifacts
COPY --from=builder /out/ /out/
"""

MIGRATED = """# synthetic
ARG CONTROLLER_BASE

FROM ${CONTROLLER_BASE} AS controller_base

FROM gcr.io/distroless/static:nonroot
COPY --from=controller_base /out/%(bin)s /%(bin)s
ENTRYPOINT ["/%(bin)s"]
"""

SELF_COMPILING = """# synthetic
FROM golang:1.25-bookworm AS builder
WORKDIR /build
COPY controller/ ./
COPY clicreds/ /clicreds/
RUN CGO_ENABLED=0 go build -o /%(bin)s ./cmd/%(bin)s/

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /%(bin)s /%(bin)s
ENTRYPOINT ["/%(bin)s"]
"""


def check(name, entry, dockerfile, want_hit, want_substr=None,
          loop_bins='alpha beta', extra_base=''):
    """Build a one-image synthetic repo and assert the guard's verdict."""
    global PASS, FAIL
    root = tempfile.mkdtemp(prefix='ccb-test-')
    try:
        os.makedirs(os.path.join(root, '.github'))
        base_dir = os.path.join(root, 'gardenio', 'images', '_controller-base')
        os.makedirs(base_dir)
        with open(os.path.join(base_dir, 'Dockerfile'), 'w') as f:
            f.write(BASE_TEMPLATE % (loop_bins, extra_base))
        df = os.path.join(root, entry['dockerfile'])
        os.makedirs(os.path.dirname(df), exist_ok=True)
        with open(df, 'w') as f:
            f.write(dockerfile)
        with open(os.path.join(root, '.github', 'images.json'), 'w') as f:
            f.write(json.dumps([entry], indent=2) + '\n')

        env = dict(os.environ, GARDENIO_ROOT=root)
        p = subprocess.run([sys.executable, GUARD], capture_output=True,
                           text=True, env=env)
        out = p.stdout + p.stderr
        got_hit = p.returncode != 0
        ok = got_hit == want_hit
        if ok and want_substr and want_substr not in out:
            ok = False
            out += '\n(expected message to contain %r)' % want_substr
        if ok:
            print('PASS: %s' % name)
            PASS += 1
        else:
            print('FAIL: %s: expected %s, got rc=%d\n%s'
                  % (name, 'a hit' if want_hit else 'no hit', p.returncode, out))
            FAIL += 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


def entry(image='alpha', ctx='gardenio', flagged=False, df=None):
    e = {'image': image, 'context': ctx,
         'dockerfile': df or 'gardenio/images/%s/Dockerfile' % image}
    if flagged:
        e['controllerBase'] = True
    return e


# ── the clean state ────────────────────────────────────────────────────────
check('flagged and rewired', entry(flagged=True), MIGRATED % {'bin': 'alpha'},
      False)
check('unflagged, compiles a binary the base does NOT build',
      entry(image='zeta'),
      SELF_COMPILING % {'bin': 'zeta'}, False)
check('unflagged, context is the repo root (the images/agent/ case)',
      entry(ctx='.'), SELF_COMPILING % {'bin': 'alpha'}, False)

# ── rule C: the silent drift the guard exists for ──────────────────────────
check('unflagged, self-compiles a base binary',
      entry(), SELF_COMPILING % {'bin': 'alpha'},
      True, 'already builds that binary')
check('unflagged, self-compiles a base binary built outside the loop',
      entry(image='gamma'), SELF_COMPILING % {'bin': 'gamma'},
      True, 'already builds that binary',
      extra_base='RUN go build -o /out/gamma ./cmd/gamma/')

# ── rule B: flag and Dockerfile disagree the other way ─────────────────────
check('rewired but unflagged (build-arg never passed)',
      entry(), MIGRATED % {'bin': 'alpha'},
      True, 'never passes the build-arg')

# ── rule A: flagged but the Dockerfile does not hold up its end ────────────
check('flagged but still self-compiling',
      entry(flagged=True), SELF_COMPILING % {'bin': 'alpha'},
      True, 'never declares')
check('flagged, COPYs a binary the base does not build',
      entry(image='omega', flagged=True), MIGRATED % {'bin': 'omega'},
      True, 'does not build')
check('flagged, has the stage but COPYs nothing from it',
      entry(flagged=True),
      'ARG CONTROLLER_BASE\n\nFROM ${CONTROLLER_BASE} AS controller_base\n\n'
      'FROM gcr.io/distroless/static:nonroot\nENTRYPOINT ["/alpha"]\n',
      True, 'nothing is COPYed')

# ── the base Dockerfile changing shape must fail loudly, not silently pass ──
check('base Dockerfile no longer parseable',
      entry(flagged=True), MIGRATED % {'bin': 'alpha'},
      True, 'could not parse any binary',
      loop_bins='', extra_base='')

print('\n%d passed, %d failed' % (PASS, FAIL))
sys.exit(1 if FAIL else 0)
