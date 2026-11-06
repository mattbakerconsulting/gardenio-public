#!/usr/bin/env python3
"""check-controller-base.py

Guard the images/_controller-base/ invariant.

WHY THIS EXISTS
---------------
gd-B2c8OinSs7 (ci-speed phase 2) added images/_controller-base/Dockerfile: ONE
`go build` of every controller/cmd binary, in one GOCACHE, pushed to the
registry by the `controller-base` job, from which each service image then does
a trivial `COPY --from=controller_base /out/<bin>`. Before it, each of ~20
images independently ran `COPY controller/ ./` + `go build`, recompiling the
whole shared package graph in its own pod -- and `go build`'s output is
invalidated by ARG COMMIT (it is linked in via -ldflags), so the registry
buildx cache never hit that layer across commits.

The base landed building all 20 binaries. Only SIX images were then wired onto
it. The other fourteen kept their own builder stage and nobody noticed,
because nothing was broken -- they were merely doing, on a whole 4-vCPU node
each, work the base had already done.

That cost was not small and it was not spread evenly. `gardenio/controller/**`
appeared in 21 of the 26 path filters .github/workflows/images.yml had then
(retired by gd-whdmfznixO; a release tag builds every enabled image), so any
controller change fanned the matrix out to 21-26 heavy jobs at once. Measured
over 17 images runs on 2026-09-22 the matrix size was bimodal -- 2, or 21-26,
with the big shape in the majority -- and the heavy pool has ten slots. The
rewiring took a full-matrix run from ~2700 node-seconds to a measured 941
(dispatch 35694871982: base 228 s plus twenty images totalling 713 s, mean
35.6 s each -- call it ~1050 if you price the base cold at 337 s).

So the failure this guards against is SILENT: an image that compiles
controller/ itself still produces a correct image. It just quietly puts a
150-second job where a 50-second one belongs, and the only symptom is queue
wait somewhere else entirely.

THE RULES
---------
For every entry in .github/images.json:

  A. "controllerBase": true  =>  the Dockerfile declares ARG CONTROLLER_BASE,
     has `FROM ${CONTROLLER_BASE} AS controller_base`, COPYs at least one
     binary from it, every binary it COPYs is one the base actually builds,
     and it does NOT also compile controller/ itself.

  B. no controllerBase flag  =>  the Dockerfile must not mention
     CONTROLLER_BASE. .github/workflows/images.yml only passes that build-arg
     when the flag is set, and a `FROM ${CONTROLLER_BASE}` with nothing passed
     fails with an unhelpful "invalid reference format".

  C. an image whose build context is `gardenio` must not `go build ./cmd/<x>/`
     for an <x> the base already builds. That is the exact drift above.

     Context matters: images/agent/ builds from the repo root (context "."),
     needs its Go binaries inside a 9-stage image alongside a Rust toolchain,
     Playwright and Whisper, and its Go compile is not what makes it a
     10-minute job. It is exempt by the context rule, not by a name list.

Usage:
    ./scripts/check-controller-base.py

Exit status: 0 if clean, 1 on any violation.
"""
import json
import os
import re
import sys

# GARDENIO_ROOT exists for check-controller-base_test.py, which builds synthetic
# repos in a tmpdir to prove each rule fires. Nothing else sets it.
ROOT = os.environ.get('GARDENIO_ROOT') or os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
IMAGES_JSON = os.path.join(ROOT, '.github', 'images.json')
BASE_DF = os.path.join(ROOT, 'gardenio', 'images', '_controller-base', 'Dockerfile')


def base_binaries(text):
    """Names the base image writes to /out/.

    Two forms: the `for bin in a b c; do ... -o "/out/${bin}"` loop, and any
    literal `-o /out/<name>` (gb is built outside the loop, deliberately -- see
    the comment there)."""
    names = set()
    loop = re.search(r'for bin in\s+(.*?);\s*\\?\s*\n\s*do\b', text, re.S)
    if loop:
        names |= set(loop.group(1).replace('\\', ' ').split())
    names |= set(re.findall(r'-o\s+/out/([A-Za-z0-9._-]+)', text))
    return names


def main():
    base_text = open(BASE_DF).read()
    built = base_binaries(base_text)
    if not built:
        print('check-controller-base: FAIL -- could not parse any binary out of\n'
              '  %s\nThe loop or the -o flags changed shape; fix base_binaries().'
              % BASE_DF, file=sys.stderr)
        return 1

    entries = json.loads(open(IMAGES_JSON).read())
    problems = []

    for e in entries:
        name, ctx = e['image'], e['context']
        df = os.path.join(ROOT, e['dockerfile'])
        try:
            text = open(df).read()
        except OSError as exc:
            problems.append('%s: cannot read %s (%s)' % (name, e['dockerfile'], exc))
            continue

        flagged = bool(e.get('controllerBase'))
        mentions = 'CONTROLLER_BASE' in text
        copied = re.findall(r'COPY --from=controller_base /out/([A-Za-z0-9._-]+)', text)
        compiles = re.findall(r'-o\s+\S*?/?([A-Za-z0-9._-]+)\s+\./cmd/([A-Za-z0-9._-]+)/', text)

        if flagged:
            if 'ARG CONTROLLER_BASE' not in text:
                problems.append('%s: controllerBase is true but the Dockerfile never '
                                'declares `ARG CONTROLLER_BASE`' % name)
            if 'FROM ${CONTROLLER_BASE} AS controller_base' not in text:
                problems.append('%s: controllerBase is true but there is no '
                                '`FROM ${CONTROLLER_BASE} AS controller_base` stage' % name)
            if not copied:
                problems.append('%s: controllerBase is true but nothing is COPYed '
                                'from the controller_base stage' % name)
            for b in copied:
                if b not in built:
                    problems.append('%s: COPYs /out/%s, which images/_controller-base/'
                                    'Dockerfile does not build. Add it to the bin list '
                                    'there -- do not add a second builder stage.'
                                    % (name, b))
            if 'COPY controller/ ./' in text:
                problems.append('%s: controllerBase is true but it still does '
                                '`COPY controller/ ./` and compiles the tree itself'
                                % name)
        else:
            if mentions:
                problems.append('%s: the Dockerfile mentions CONTROLLER_BASE but '
                                '.github/images.json does not set "controllerBase": true, '
                                'so images.yml never passes the build-arg' % name)
            if ctx == 'gardenio':
                for _, cmd in compiles:
                    if cmd in built:
                        problems.append(
                            '%s: compiles ./cmd/%s/ itself, but images/_controller-base/ '
                            'already builds that binary. Set "controllerBase": true in '
                            '.github/images.json and COPY --from=controller_base '
                            '/out/%s instead.' % (name, cmd, cmd))

    if problems:
        e = sys.stderr
        print('check-controller-base: FAIL', file=e)
        print('', file=e)
        for p in problems:
            print('  %s' % p, file=e)
        print('', file=e)
        print('See the header of %s for why this matters.' % __file__, file=e)
        return 1

    n = sum(1 for x in entries if x.get('controllerBase'))
    print('check-controller-base: OK (%d images, %d on controller-base, '
          '%d binaries in the shared build)' % (len(entries), n, len(built)))
    return 0


if __name__ == '__main__':
    sys.exit(main())
