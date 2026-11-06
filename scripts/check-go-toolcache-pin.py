#!/usr/bin/env python3
"""check-go-toolcache-pin.py — the ci runner pods seed the Go toolchain into
the node-local GitHub tool cache. This keeps that seed pointing at the Go
version the workflows actually ask for, and keeps the wiring that makes it
take effect from being removed silently.

WHY THIS GUARD EXISTS
---------------------
`actions/setup-go` looks for $RUNNER_TOOL_CACHE/go/<version>/<arch> next to a
`<arch>.complete` marker. With RUNNER_TOOL_CACHE unset, the runner defaults it
into _work/_tool -- the pod's `work` emptyDir -- so 243 MB of Go was thrown
away with every pod and re-downloaded by every job. Measured over 99
`Run actions/setup-go@v5` steps on 2026-09-23: p50 18s, p90 35s, max 56s,
2386 of 27034 pool cpu-seconds. That is 8.8% of the pool spent re-fetching an
identical toolchain, and it was the 4th largest consumer on it.

gardenio/helm/ci/arc-runners{,-medium}-values.yaml now seed that cache from an
init container and point RUNNER_TOOL_CACHE at a pod-local dir holding a
symlink to it.

That arrangement has exactly two silent failure modes, and both are why this
file exists rather than a comment:

  1. THE PIN DRIFTS. Someone bumps GO_VERSION in ci.yml, the seed still holds
     the old version, every setup-go misses the cache and downloads again.
     Nothing goes red. CI just quietly gets 8.8% more expensive, which is the
     same shape as the bug being fixed and just as invisible.

  2. THE WIRING GOES AWAY. Someone drops the RUNNER_TOOL_CACHE env from a
     values file -- during an unrelated edit, or resolving a merge. The init
     container keeps seeding perfectly, the runner keeps ignoring it, and the
     only symptom is, again, a slower pool.

Neither is a correctness bug, so no test anywhere would catch either. Both are
pure cost, and cost regressions on this pool are measured in queue wait.

WHAT IS CHECKED
---------------
  * both values files pin a version and a 64-hex sha256, and they agree
  * that version is exactly GO_VERSION in ci.yml and in checks.yml
  * that version satisfies the looser GO_VERSION spec in e2e.yml (which pins
    a minor series, not a patch)
  * the agent image's GO_FULL_VERSION (install/versions.env) is that same
    version, and its GO_SHA256_AMD64 (install/checksums.env) is the seed's
    sha256 -- both name the one go<version>.linux-amd64.tar.gz. The image
    pinned 1.25.9 while go.work said 1.25.12, so the first go command on every
    fresh agent pod downloaded a 1.25.12 toolchain into GOMODCACHE, and the
    image's prewarmed GOCACHE (keyed on the compiler) could never have hit
    (gd-j2ABjjrEL0). Pinning the image to what CI runs keeps crews compiling
    with the CI compiler too
  * no `go` or `toolchain` line in go.work or in any go.mod in the tree asks
    for more than that GO_FULL_VERSION. The agent image sets GOTOOLCHAIN=local
    (gd-auZ6V08nR1), so a go line above the image's Go is not a quiet download
    on a pod but a hard failure of every go command on every crew pod until a
    new image ships. This makes such a bump red on its PR instead: bump the
    image (and so CI) first. A toolchain line is ignored under local, but CI's
    setup-go Go runs with the default GOTOOLCHAIN=auto and would download it
  * both values files set RUNNER_TOOL_CACHE on the runner container
  * the seeding block derives its paths from $GO_TOOLCACHE_VERSION rather
    than repeating the literal, so there is one pin per file and not three
  * the two tiers' seeding blocks are byte-identical once comments and the
    common indent are removed. Both files SAY they are copies of each other,
    and they write the same hostPath under the same lock, so a fix applied to
    one and not the other is a live hazard rather than a tidiness issue --
    and it is invisible, because the tier that still works keeps the node
    seeded for both

WHAT IS NOT CHECKED
-------------------
That the sha256 is the RIGHT one for that version. Verifying it means
downloading 60 MB from go.dev, and a guard that needs the network is a guard
that reds main when go.dev has a bad minute. A wrong sha fails closed anyway:
the seed's `sha256sum -c` refuses, the pod logs a WARN, and jobs download Go
exactly as they did before this change. Wrong-and-loud, not wrong-and-silent,
which is the property that makes it safe to leave out.

The workflow files are read as TEXT, not parsed as YAML: these runners have no
PyYAML, and the values files are Helm values whose embedded shell is not
addressable as YAML structure anyway.

Usage:  python3 scripts/check-go-toolcache-pin.py
        GARDENIO_ROOT=/some/tree python3 scripts/check-go-toolcache-pin.py
Exit status: 0 if the pins agree, 1 otherwise.
"""
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.environ.get('GARDENIO_ROOT') or os.path.join(HERE, os.pardir)

VALUES = [
    'gardenio/helm/ci/arc-runners-values.yaml',
    'gardenio/helm/ci/arc-runners-medium-values.yaml',
]
# (workflow, exact?) -- e2e.yml pins the minor series, the other two pin the
# patch. An exact file must equal the seed; a series file must contain it.
WORKFLOWS = [
    ('.github/workflows/ci.yml', True),
    ('.github/workflows/checks.yml', True),
    ('.github/workflows/e2e.yml', False),
]

# The agent image's own pins. Its Go is installed by install/go-tools.sh from
# these, not by setup-go, so it has no GO_VERSION line to match above.
AGENT_VERSIONS = 'gardenio/images/agent/install/versions.env'
AGENT_CHECKSUMS = 'gardenio/images/agent/install/checksums.env'
AGENT_GO_RE = re.compile(r'^export GO_FULL_VERSION=(\S+)\s*$', re.M)
AGENT_SHA_RE = re.compile(r'^export GO_SHA256_AMD64=(\S+)\s*$', re.M)

# The Go the tree asks for. go.work's `use` modules must each have a go.mod;
# every other go.mod is found by walking the tree (the out-of-workspace
# modules run with GOWORK=off on the same pods), pruned like go-warm-mods.
GO_WORK = 'go.work'
GO_MOD_PRUNE = frozenset(('.git', 'node_modules', 'target', 'testdata',
                          'vendor', '.terraform'))
USE_BLOCK_RE = re.compile(r'^use\s*\(([^)]*)\)', re.M)
USE_LINE_RE = re.compile(r'^use\s+([^\s(]\S*)\s*$', re.M)
GO_LINE_RE = re.compile(r'^go\s+(\S+)\s*$', re.M)
TOOLCHAIN_LINE_RE = re.compile(r'^toolchain\s+go(\S+)\s*$', re.M)
GO_VER_RE = re.compile(r'^(\d+)\.(\d+)(?:\.(\d+)|(?:rc|beta)\d+)?$')

VERSION_RE = re.compile(r'^\s*GO_TOOLCACHE_VERSION=(\S+)\s*$', re.M)
SHA_RE = re.compile(r'^\s*GO_TOOLCACHE_SHA256=(\S+)\s*$', re.M)
TOOLCACHE_ENV_RE = re.compile(r'-\s*name:\s*RUNNER_TOOL_CACHE\s*\n\s*value:\s*(\S+)')
GO_VERSION_RE = re.compile(r'^\s*GO_VERSION:\s*["\']?([0-9][^"\'\s#]*)["\']?', re.M)
HEX64_RE = re.compile(r'^[0-9a-f]{64}$')

problems = []
# Counted so the report can prove the guard actually looked at something. A
# loop over files that all failed to open is indistinguishable from a clean
# run if you only count problems.
read = []


def fail(msg):
    problems.append(msg)


def load(rel):
    path = os.path.join(ROOT, rel)
    try:
        with open(path, encoding='utf-8') as f:
            src = f.read()
    except OSError as e:
        fail('%s: cannot read (%s). This guard cannot pass on a tree it '
             'cannot see; if the file moved, update this list.' % (rel, e))
        return None
    read.append(rel)
    return src


def load_quiet(rel):
    """Re-read a file load() already counted; '' if it is gone."""
    try:
        with open(os.path.join(ROOT, rel), encoding='utf-8') as f:
            return f.read()
    except OSError:
        return ''


def series_matches(spec, version):
    """Does `version` satisfy a dotted-prefix `spec` like 1.25 or 1.25.14?"""
    a, b = spec.split('.'), version.split('.')
    return len(a) <= len(b) and a == b[:len(a)]


def go_key(v):
    """Order Go versions as the go command does: 1.25 < 1.25rc1 < 1.25.0."""
    m = GO_VER_RE.match(v)
    if not m:
        return None
    if m.group(3) is not None:
        patch = int(m.group(3))
    else:
        patch = -1 if ('rc' in v or 'beta' in v) else -2
    return (int(m.group(1)), int(m.group(2)), patch)


def work_modules(src):
    """The module dirs go.work `use`s, relative and cleaned."""
    mods = []
    for block in USE_BLOCK_RE.findall(src):
        for line in block.splitlines():
            line = line.split('//', 1)[0].strip()
            if line:
                mods.append(line)
    mods += USE_LINE_RE.findall(src)
    return [os.path.normpath(m) for m in mods]


def tree_go_mods():
    """Every go.mod under ROOT, outside GO_MOD_PRUNE dirs, as relpaths."""
    out = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = sorted(d for d in dirnames if d not in GO_MOD_PRUNE)
        if 'go.mod' in filenames:
            out.append(os.path.relpath(os.path.join(dirpath, 'go.mod'), ROOT))
    return out


def check_go_lines(agent_go):
    """No go.work/go.mod asks for more Go than the agent image ships."""
    work = load(GO_WORK)
    mods = set(tree_go_mods())
    if work is not None:
        for m in work_modules(work):
            rel = os.path.normpath(os.path.join(m, 'go.mod'))
            if rel not in mods:
                fail('%s uses %s, which has no go.mod.' % (GO_WORK, m))
    have = go_key(agent_go)
    if have is None:
        fail('%s: GO_FULL_VERSION %s is not a Go release version.'
             % (AGENT_VERSIONS, agent_go))
        return 0
    checked = 0
    for rel in [GO_WORK] + sorted(mods):
        src = work if rel == GO_WORK else load_quiet(rel)
        if not src:
            continue
        checked += 1
        for kind, vs in (('go ', GO_LINE_RE.findall(src)),
                         ('toolchain go', TOOLCHAIN_LINE_RE.findall(src))):
            for v in vs:
                need = go_key(v)
                if need is None:
                    fail('%s: `%s%s` is not a Go version this guard can '
                         'order; teach go_key() about it.' % (rel, kind, v))
                elif need > have:
                    fail('%s says `%s%s`, above the agent image\'s Go %s '
                         '(GO_FULL_VERSION in %s). The image sets '
                         'GOTOOLCHAIN=local, so every go command on every '
                         'crew pod would refuse to run here until a new image '
                         'ships. Bump GO_FULL_VERSION (and so CI and the '
                         'runner seed) first, release it, then this line.'
                         % (rel, kind, v, agent_go, AGENT_VERSIONS))
    return checked


def seed_block(src):
    """The seeding shell, as code lines only.

    Starts at the GO_TOOLCACHE_VERSION pin and ends where the embedded shell
    ends -- detected by the first non-blank line indented LESS than the pin,
    which is the next YAML key out of the `- |` literal. Comments are dropped
    (the two tiers deliberately carry different prose) and the common indent
    is removed, so what is compared is the code and its relative nesting.
    """
    lines = src.splitlines()
    start = None
    for i, line in enumerate(lines):
        if line.lstrip().startswith('GO_TOOLCACHE_VERSION='):
            start = i
            break
    if start is None:
        return None
    base = len(lines[start]) - len(lines[start].lstrip())
    out = []
    for line in lines[start:]:
        if not line.strip():
            continue
        if len(line) - len(line.lstrip()) < base:
            break
        if line.lstrip().startswith('#'):
            continue
        out.append(line[base:].rstrip())
    return out


def main():
    seeds = {}
    blocks = {}
    for rel in VALUES:
        src = load(rel)
        if src is None:
            continue
        blocks[rel] = seed_block(src)

        vs = VERSION_RE.findall(src)
        if not vs:
            fail('%s: no GO_TOOLCACHE_VERSION=... line. The tool-cache seed '
                 'is gone, or was renamed; if it was removed on purpose, '
                 'remove this guard in the same commit.' % rel)
        elif len(set(vs)) > 1:
            fail('%s: GO_TOOLCACHE_VERSION set %d times with %d different '
                 'values (%s) -- one pin per file.'
                 % (rel, len(vs), len(set(vs)), ', '.join(sorted(set(vs)))))
        else:
            seeds[rel] = vs[0]

        shas = SHA_RE.findall(src)
        if not shas:
            fail('%s: no GO_TOOLCACHE_SHA256=... line. The seed must verify '
                 'what it downloads before putting it on every job\'s PATH.'
                 % rel)
        for sha in set(shas):
            if not HEX64_RE.match(sha):
                fail('%s: GO_TOOLCACHE_SHA256=%s is not 64 lowercase hex '
                     'characters.' % (rel, sha))

        env = TOOLCACHE_ENV_RE.search(src)
        if not env:
            fail('%s: the runner container sets no RUNNER_TOOL_CACHE. Without '
                 'it the runner defaults the tool cache into the `work` '
                 'emptyDir and the seeding above it does nothing at all -- '
                 'silently, and at about 8.8%% of this pool.' % rel)

        # One pin, not three. A literal version anywhere else in the seeding
        # block is a second place to forget.
        if rel in seeds:
            v = seeds[rel]
            for line in src.splitlines():
                if 'GO_TOOLCACHE_VERSION=' in line or line.lstrip().startswith('#'):
                    continue
                if v in line:
                    fail('%s: the seeded version is written literally outside '
                         'its pin: %s. Use $GO_TOOLCACHE_VERSION so there is '
                         'one place to bump.' % (rel, line.strip()))

    if len(blocks) == len(VALUES) and all(blocks.values()):
        names = list(VALUES)
        a, b = blocks[names[0]], blocks[names[1]]
        if a != b:
            diff = [l for l in a if l not in b][:3] + \
                   [l for l in b if l not in a][:3]
            fail('the two tiers\' seeding blocks have drifted apart (%d vs %d '
                 'code lines). They write the SAME node hostPath under the '
                 'SAME lock, so a fix in one tier and not the other is a live '
                 'hazard, and an invisible one: whichever tier still works '
                 'keeps the node seeded for both. First differing lines: %s'
                 % (len(a), len(b), ' | '.join(l.strip() for l in diff)))

    if len(set(seeds.values())) > 1:
        fail('the ci tiers seed different Go versions (%s). They share one '
             'hostPath, so whichever pod starts first wins and the other '
             'tier silently misses the cache.'
             % ', '.join('%s=%s' % (os.path.basename(k), v)
                         for k, v in sorted(seeds.items())))

    seeded = sorted(set(seeds.values()))[0] if seeds else None

    for rel, exact in WORKFLOWS:
        src = load(rel)
        if src is None:
            continue
        specs = set(GO_VERSION_RE.findall(src))
        if not specs:
            fail('%s: no GO_VERSION. If this workflow stopped using Go, drop '
                 'it from WORKFLOWS here rather than leaving a check that '
                 'passes because it found nothing.' % rel)
            continue
        if seeded is None:
            continue
        for spec in sorted(specs):
            if exact and spec != seeded:
                fail('%s pins GO_VERSION %s but the runners seed %s. Every '
                     'setup-go in it will miss the node tool cache and '
                     'download Go again -- green, and ~18s of a whole node '
                     'per job.' % (rel, spec, seeded))
            elif not exact and not series_matches(spec, seeded):
                fail('%s asks for Go %s, which the seeded %s does not '
                     'satisfy.' % (rel, spec, seeded))

    agent_src = load(AGENT_VERSIONS)
    sums_src = load(AGENT_CHECKSUMS)
    agent_go = AGENT_GO_RE.findall(agent_src or '')
    agent_sha = AGENT_SHA_RE.findall(sums_src or '')
    if agent_src is not None and len(agent_go) != 1:
        fail('%s: expected one `export GO_FULL_VERSION=` line, found %d.'
             % (AGENT_VERSIONS, len(agent_go)))
    elif agent_go and seeded and agent_go[0] != seeded:
        fail('%s pins GO_FULL_VERSION %s but CI runs %s. Every fresh agent '
             'pod then either downloads a second toolchain (when the pin is '
             'below the go.work go line) or compiles with a different '
             'compiler than CI, and the image\'s prewarmed GOCACHE misses '
             'either way. Bump GO_FULL_VERSION and GO_SHA256_* together.'
             % (AGENT_VERSIONS, agent_go[0], seeded))
    if sums_src is not None and len(agent_sha) != 1:
        fail('%s: expected one `export GO_SHA256_AMD64=` line, found %d.'
             % (AGENT_CHECKSUMS, len(agent_sha)))
    elif agent_sha and agent_go and agent_go[0] == seeded:
        seed_shas = set()
        for rel in VALUES:
            seed_shas.update(SHA_RE.findall(load_quiet(rel)))
        if seed_shas and agent_sha[0] not in seed_shas:
            fail('%s: GO_SHA256_AMD64 is not the sha256 the runners seed for '
                 'the same go%s.linux-amd64.tar.gz. One of them is wrong.'
                 % (AGENT_CHECKSUMS, seeded))

    checked = check_go_lines(agent_go[0]) if len(agent_go) == 1 else 0

    print('check-go-toolcache-pin: read %d files (%s)'
          % (len(read), ', '.join(os.path.basename(r) for r in read)))
    if seeded:
        print('check-go-toolcache-pin: seeded Go %s' % seeded)
    if checked:
        print('check-go-toolcache-pin: checked the go lines of %d '
              'go.work/go.mod files against the agent image\'s Go %s'
              % (checked, agent_go[0]))
    want = len(VALUES) + len(WORKFLOWS) + 3
    if len(read) != want:
        fail('expected to read %d files, read %d. A guard that skips its '
             'inputs passes for the wrong reason.' % (want, len(read)))

    for p in problems:
        print('check-go-toolcache-pin: %s' % p, file=sys.stderr)
    if problems:
        print('check-go-toolcache-pin: %d problem(s)' % len(problems),
              file=sys.stderr)
        return 1
    print('check-go-toolcache-pin: ok')
    return 0


if __name__ == '__main__':
    sys.exit(main())
