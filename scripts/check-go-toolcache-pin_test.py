#!/usr/bin/env python3
"""check-go-toolcache-pin_test.py — unit tests for check-go-toolcache-pin.py

Each case copies a minimal synthetic tree into a tmpdir, applies ONE mutation,
and asserts the guard's exit status. GARDENIO_ROOT points the guard at it.

The mutations are not hypothetical. They are the two ways the Go tool-cache
seed stops working, both of which are SILENT -- CI stays green and simply gets
slower, which is indistinguishable from a busy day:

  * the pin drifts away from GO_VERSION (cases 2, 6, 7)
  * the RUNNER_TOOL_CACHE wiring is dropped, so the seed is written and then
    ignored (case 3)

and one that is LOUD, which is why it must red the PR rather than main: a
go.work or go.mod go line bumped above the agent image's Go, which the image's
GOTOOLCHAIN=local turns into every go command failing on every crew pod
(case 10d).

The clean fixture (case 1) is the load-bearing negative control: without it, a
guard that rejected EVERY input would pass every other case here. And the last
case runs the guard against the REAL tree with no GARDENIO_ROOT, which is what
proves the paths in its two lists still exist -- a guard whose inputs have all
been renamed reports no problems in exactly the same words as a clean tree,
unless it also counts what it read.

Usage:  python3 scripts/check-go-toolcache-pin_test.py
Exit status: 0 if the guard behaves as specified on every fixture, 1 otherwise.
"""
import os
import shutil
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.join(HERE, os.pardir)
GUARD = os.path.join(HERE, 'check-go-toolcache-pin.py')

SHA = 'a21ae5633a269bcd7e90cf767e48225633795e99d831742cbf3397064fee7712'

PASS = FAIL = 0


def record(name, ok, detail=''):
    global PASS, FAIL
    if ok:
        print('ok   %s' % name)
        PASS += 1
    else:
        print('FAIL %s' % name)
        for line in detail.split('\n'):
            if line:
                print('       | %s' % line)
        FAIL += 1


def values(version=None, sha=SHA, tool_cache_env=True, extra=''):
    """A values file with just the parts the guard reads."""
    out = ['runnerScaleSetName: fixture', 'template:', '  spec:',
           '    initContainers:', '      - name: prepare-build-cache',
           '        command:', '          - sh', '          - -c',
           '          - |', '            set -e']
    if version is not None:
        out.append('            GO_TOOLCACHE_VERSION=%s' % version)
    if sha is not None:
        out.append('            GO_TOOLCACHE_SHA256=%s' % sha)
    out.append('            shared=/cache/tool-cache/go/$GO_TOOLCACHE_VERSION/x64')
    if extra:
        out.append('            %s' % extra)
    out += ['    containers:', '      - name: runner', '        env:',
            '          - name: NODE_NAME',
            '            value: fixture']
    if tool_cache_env:
        out += ['          - name: RUNNER_TOOL_CACHE',
                '            value: /toolcache']
    return '\n'.join(out) + '\n'


def workflow(go_version):
    return ('name: fixture\nenv:\n  GO_VERSION: "%s"\njobs:\n  a:\n'
            '    runs-on: gardenio-k3s\n    steps:\n      - run: go build ./...\n'
            % go_version)


def build(tmp, heavy='1.25.14', medium='1.25.14', ci='1.25.14',
          checks='1.25.14', e2e='1.25', agent='1.25.14', agent_sha=SHA,
          work_go='1.25.12', work_use=('./mod',), mod_go='1.25.12',
          mod_extra='', outside_go='1.25.9', **kw):
    """Write a complete fixture tree; kw is forwarded to values().

    A tier version of None deletes the file; False writes the file with no
    GO_TOOLCACHE_VERSION line in it. Those are different failures and the
    guard says different things about them. work_go=None deletes go.work.

    The Go modules: mod/ is in go.work, outside/ is not (like the helm chart
    tests, run with GOWORK=off), and node_modules/dep/ asks for Go 1.99 and
    must be pruned -- so the clean fixture passing proves the prune too.
    """
    hk = dict((k[6:], v) for k, v in kw.items() if k.startswith('heavy_'))
    mk = dict((k[7:], v) for k, v in kw.items() if k.startswith('medium_'))
    d = os.path.join(tmp, 'gardenio', 'helm', 'ci')
    os.makedirs(d)
    w = os.path.join(tmp, '.github', 'workflows')
    os.makedirs(w)
    if heavy is not None:
        with open(os.path.join(d, 'arc-runners-values.yaml'), 'w') as f:
            f.write(values(heavy or None, **hk))
    if medium is not None:
        with open(os.path.join(d, 'arc-runners-medium-values.yaml'), 'w') as f:
            f.write(values(medium or None, **mk))
    for name, v in (('ci.yml', ci), ('checks.yml', checks), ('e2e.yml', e2e)):
        with open(os.path.join(w, name), 'w') as f:
            f.write(workflow(v))
    a = os.path.join(tmp, 'gardenio', 'images', 'agent', 'install')
    os.makedirs(a)
    with open(os.path.join(a, 'versions.env'), 'w') as f:
        f.write('export GO_VERSION=1.25\n')
        if agent is not None:
            f.write('export GO_FULL_VERSION=%s\n' % agent)
    with open(os.path.join(a, 'checksums.env'), 'w') as f:
        f.write('export GO_SHA256_AMD64=%s\nexport GO_SHA256_ARM64=%s\n'
                % (agent_sha, 'b' * 64))
    if work_go is not None:
        with open(os.path.join(tmp, 'go.work'), 'w') as f:
            f.write('go %s\n\nuse (\n%s)\n'
                    % (work_go, ''.join('\t%s // fixture\n' % u
                                        for u in work_use)))
    for d, go, extra in (('mod', mod_go, mod_extra),
                         ('outside', outside_go, ''),
                         ('node_modules/dep', '1.99.0', '')):
        os.makedirs(os.path.join(tmp, d))
        with open(os.path.join(tmp, d, 'go.mod'), 'w') as f:
            f.write('module example.com/%s\n\ngo %s\n%s'
                    % (d, go, extra and extra + '\n'))
    return tmp


def run(root=None):
    env = dict(os.environ)
    if root:
        env['GARDENIO_ROOT'] = root
    else:
        env.pop('GARDENIO_ROOT', None)
    p = subprocess.run([sys.executable, '-S', '-E', GUARD],
                       capture_output=True, text=True, env=env, cwd=REPO)
    return p.returncode, p.stdout + p.stderr


def case(name, want_rc, want_text=None, **kw):
    tmp = tempfile.mkdtemp(prefix='toolcache-pin-')
    try:
        build(tmp, **kw)
        rc, out = run(tmp)
        ok = rc == want_rc
        if ok and want_text:
            ok = want_text in out
        record(name, ok, 'rc=%d (wanted %d)%s\n%s'
               % (rc, want_rc,
                  '' if not want_text else '\nwanted text: %r' % want_text,
                  out.strip()))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def main():
    # 1 -- negative control. Everything below is meaningless without it.
    case('clean fixture passes', 0)

    # 2 -- the drift this guard exists for.
    case('ci.yml bumped past the seed is caught', 1, 'ci.yml pins GO_VERSION',
         ci='1.26.0')
    case('checks.yml bumped past the seed is caught', 1,
         'checks.yml pins GO_VERSION', checks='1.26.0')

    # 3 -- the wiring going away. The seed still runs; nothing is faster.
    case('dropping RUNNER_TOOL_CACHE from heavy is caught', 1,
         'sets no RUNNER_TOOL_CACHE', heavy_tool_cache_env=False)
    case('dropping RUNNER_TOOL_CACHE from medium is caught', 1,
         'sets no RUNNER_TOOL_CACHE', medium_tool_cache_env=False)

    # 4 -- the download would go unverified onto every job's PATH.
    case('a missing sha256 is caught', 1, 'no GO_TOOLCACHE_SHA256',
         heavy_sha=None)
    case('a truncated sha256 is caught', 1, 'not 64 lowercase hex',
         heavy_sha=SHA[:32])
    case('an uppercased sha256 is caught', 1, 'not 64 lowercase hex',
         heavy_sha=SHA.upper())

    # 6 -- the tiers share one hostPath, so they must want the same tree.
    case('tiers seeding different versions are caught', 1,
         'seed different Go versions', medium='1.25.13')

    # 7 -- the looser series spec.
    case('e2e.yml on a different series is caught', 1, 'does not satisfy',
         e2e='1.26')
    case('e2e.yml on the same series passes', 0, e2e='1.25')

    # 8 -- inputs that vanished. Must not read as "nothing wrong".
    case('a deleted values file is caught', 1, 'cannot read', heavy=None)

    # 9 -- one pin per file.
    case('a repeated literal version is caught', 1, 'written literally',
         heavy_extra='dest=/cache/tool-cache/go/1.25.14/x64')
    case('the seed line itself is not mistaken for a repeat', 0)

    # 10 -- the seed removed outright, with the file still present.
    case('a missing GO_TOOLCACHE_VERSION is caught', 1,
         'no GO_TOOLCACHE_VERSION=... line', medium=False)

    # 10b -- the tiers drifting apart. Both files claim to be copies of each
    # other and they write one hostPath under one lock, so a fix landing in
    # only one of them is a hazard that the working tier hides. `waited=0` is
    # chosen because it carries no version literal: it must trip the identity
    # check and nothing else, or this case would pass for the wrong reason.
    case('one tier gaining a line the other lacks is caught', 1,
         'drifted apart', heavy_extra='waited=0')
    case('the same extra line in BOTH tiers passes', 0,
         heavy_extra='waited=0', medium_extra='waited=0')

    # 10c -- the agent image pin. It pinned 1.25.9 under a go.work of 1.25.12,
    # so every fresh agent pod downloaded a second toolchain (gd-j2ABjjrEL0).
    case('the agent image pinned below CI is caught', 1,
         'pins GO_FULL_VERSION 1.25.9', agent='1.25.9')
    case('a missing agent GO_FULL_VERSION is caught', 1,
         'expected one `export GO_FULL_VERSION=`', agent=None)
    case('an agent sha256 for a different tarball is caught', 1,
         'GO_SHA256_AMD64 is not the sha256', agent_sha='c' * 64)

    # 10d -- a go line above the agent image's Go (gd-auZ6V08nR1). The image
    # sets GOTOOLCHAIN=local, so this is every go command failing on every
    # crew pod until a new image ships, not a quiet toolchain download.
    case('a go.work go line above the image is caught', 1,
         'go.work says `go 1.25.15`', work_go='1.25.15')
    case('a workspace go.mod go line above the image is caught', 1,
         'mod/go.mod says `go 1.26.0`', mod_go='1.26.0')
    case('an out-of-workspace go.mod go line above the image is caught', 1,
         'outside/go.mod says `go 1.26`', outside_go='1.26')
    case('a release candidate of the next Go is caught', 1,
         'says `go 1.26rc1`', work_go='1.26rc1')
    case('a toolchain line above the image is caught', 1,
         'says `toolchain go1.26.0`', mod_extra='toolchain go1.26.0')
    case('a go line equal to the image passes', 0, work_go='1.25.14',
         mod_go='1.25.14')
    case('a bare minor go line passes', 0, mod_go='1.25')
    case('a toolchain line equal to the image passes', 0,
         mod_extra='toolchain go1.25.14')
    case('a go line the guard cannot order is caught', 1,
         'not a Go version this guard can order', mod_go='1.25.x')
    case('go.work using a module with no go.mod is caught', 1,
         'uses gone, which has no go.mod', work_use=('./mod', './gone'))
    case('a deleted go.work is caught', 1, 'go.work: cannot read',
         work_go=None)

    # 11 -- the real tree. This is what proves the guard's file lists still
    # name files that exist; a fixture can never show that.
    rc, out = run(None)
    record('the guard passes on the real tree', rc == 0, out.strip())
    record('the guard read all eight real inputs on the real tree',
           'read 8 files' in out, out.strip())
    for base in ('arc-runners-values.yaml', 'arc-runners-medium-values.yaml',
                 'ci.yml', 'checks.yml', 'e2e.yml', 'versions.env',
                 'checksums.env', 'go.work'):
        record('real tree: %s was read' % base, base in out, out.strip())

    # 12 -- a self-test nothing invokes is a docstring. checks.yml lists its
    # tests explicitly; there is no glob, so forgetting the line is silent.
    checks = os.path.join(REPO, '.github', 'workflows', 'checks.yml')
    try:
        with open(checks, encoding='utf-8') as f:
            src = f.read()
    except OSError as e:
        src, checks = '', '%s (%s)' % (checks, e)
    record('this self-test is in the script self-tests list',
           os.path.basename(__file__) in src,
           'add `python3 scripts/%s` to the script self-tests step in %s'
           % (os.path.basename(__file__), checks))
    record('the guard itself is invoked by checks.yml',
           'scripts/check-go-toolcache-pin.py' in src,
           'the guard runs nowhere; a green self-test is not coverage')

    print('')
    print('check-go-toolcache-pin_test: %d passed, %d failed' % (PASS, FAIL))
    return 1 if FAIL else 0


if __name__ == '__main__':
    sys.exit(main())
