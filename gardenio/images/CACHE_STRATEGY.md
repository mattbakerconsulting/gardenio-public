# Image build cache strategy: measurement + a shared base-layer prototype

Prototype for gd-B2c8OinSs7 (epic gd-irs5c9bebi, "CI build speed", phase 2). Scope
is `gardenio/images/**` Dockerfiles only. Wiring this into CI required editing
`.github/workflows/images.yml`, normally outside the crew lane ("never modify
.github/workflows" — gardenio working rules, 2026-09-19); decision gd-t5tyQQLq9n
granted a scoped, PR-only exception for this task, and the wiring (a
`controller-base` job + `--build-arg CONTROLLER_BASE=...` on the 5 rewired
images) is included in this PR, validated by the PR's own `images` CI run.

## TL;DR

- The registry buildx cache (`cache-from`/`cache-to type=registry`,
  `mode=max`, one namespace per image) essentially never hits the expensive
  layer in normal operation. `ARG COMMIT` is baked into the `go build`
  `-ldflags`, so that RUN instruction's cache key changes on every commit —
  the only way to see a `CACHED` build layer is to literally rebuild the same
  SHA (a retry), which is what the one real hit in the sampled data turned
  out to be.
- 19 of the ~25 images share byte-identical builder-stage shape: `FROM
  golang:1.25-bookworm` → `COPY controller/ ./` + `COPY clicreds/
  /clicreds/` → `go build … ./cmd/<name>/`. Each pays its own full compile of
  the shared `controller` package graph and its own full `mode=max` cache
  export, independently, in a separate registry cache namespace
  (`cache/<image>`) that nothing else ever reads from.
- Measured (real GitHub Actions run, tag `2026.263.1`, run
  [35489211122](https://github.com/mattbakerconsulting/gardenio/actions/runs/35489211122)):
  14 of those 19 images completed in the sample window, 58s–221s each,
  averaging 127s. `go build` alone: ~89–91s. `mode=max` cache export alone:
  ~100–103s — on the slower images, cache bookkeeping costs *more* than the
  compile it's nominally caching.
- Proposed common base layer: one shared `controller-base` image
  (`images/_controller-base/Dockerfile`) that builds all 19 binaries in a
  single `go build` loop sharing one `GOCACHE`, pushed once per commit. Each
  per-image Dockerfile shrinks to a two-line `COPY --from=<controller-base>`
  + distroless final stage. Prototyped for 5 representative images here
  (fastest, slowest, and three in between); the same edit applies mechanically
  to the other 14.
- Projected full-tag minute count for the 19-image family: ~40.2 min of
  summed job time today → **projected ~12.4 min** after consolidation
  (~69% reduction), driven almost entirely by cutting 18 redundant compiles
  and 18 redundant cache exports down to 1 each. The compile-sharing part of
  this projection is backed by a real local measurement (not just CI
  numbers) — see "Projection method and caveats" below for the full
  breakdown and what still needs a real Docker build to confirm.

## Why the registry cache doesn't hit today

`images/<name>/Dockerfile` (all 19 of the family):

```dockerfile
FROM golang:1.25-bookworm AS builder
ARG VERSION=dev
ARG COMMIT=unknown
ARG COVER=0
WORKDIR /build
COPY controller/ ./
COPY clicreds/ /clicreds/
RUN CGO_ENABLED=0 go build ... -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /<name> ./cmd/<name>/
```

`.github/workflows/images.yml` passes `--build-arg
COMMIT=${{ github.sha }}` on every build. BuildKit's cache key for a `RUN`
layer is a hash of the resolved instruction text plus its input layer
digests — and the resolved instruction here *contains* `COMMIT`. So the
`go build` RUN layer's cache key is different on every commit, by
construction. Layer caching can only ever help the two `COPY` layers above
it (fast already — a few seconds), never the ~90s compile.

Confirmed directly in the job log for `slack-bridge` on the sampled tag
build (`ghcr.io/joyousaversion/gardenio/cache/slack-bridge`):

```
#9 importing cache manifest from ghcr.io/joyousaversion/gardenio/cache/slack-bridge
#10 [builder 2/5] WORKDIR /build          CACHED
#11 [builder 3/5] COPY controller/ ./     CACHED
#12 [builder 4/5] COPY clicreds/ /clicreds/  CACHED
#13 [builder 5/5] RUN ... go build ...    (NOT cached — full execution, ~91s)
```

This run is an unusually *favorable* case for the cache: it's a re-run of an
already-built tag (same commit, same everything), so even the `COPY` layers
happened to hit. On a normal commit-to-commit build, `controller/` has
almost always changed (the `changes`/`paths-filter` job only triggers these
images when it has), so even the `COPY controller/ ./` layer misses too —
meaning steady-state cache hit rate on the layer that costs anything is
**effectively 0%**, not just low.

## What consuming the cache actually costs

From the same run, `gitlab-bridge` (221s total job, the slowest sampled):

| Step | Duration |
|---|---|
| checkout, buildx setup, ghcr auth, cache import | ~20s |
| `go build` (RUN, uncached) | 89.1s |
| image export + push | ~9s |
| `mode=max` cache export ("preparing" + "sending") | 33.5s + 69.6s = **103.1s** |

Cache export is 47% of this job's wall time — more than the compile step it
exists to cache, for a cache that (per above) essentially never gets read
back on a normal commit. Multiplied across 19 images with independent cache
namespaces, that's ~19 × ~100s ≈ 32 minutes of every full-tag build spent
purely on cache bookkeeping that nothing downstream reuses.

## Measured baseline (real data, not simulated)

Source: tag build `2026.263.1`, run
[35489211122](https://github.com/mattbakerconsulting/gardenio/actions/runs/35489211122)
(job durations via the GitHub Actions API, `started_at`/`completed_at`).

| image | duration |
|---|---|
| claudemore | 58s |
| edge-auth | 59s |
| roles-server | 76s |
| scif-portal | 90s |
| repro-cockpit | 92s |
| bridge-log-shipper | 102s |
| traefik-log-shipper | 103s |
| core-log-shipper | 111s |
| slack-bridge | 163s |
| alertmanager-bridge | 168s |
| grafana-bridge | 170s |
| trigger-engine | 170s |
| beads-viewer | 194s |
| gitlab-bridge | 221s |

14 of 19 family images sampled (the other 5 — `wl-bridge`, `idp-log-shipper`,
`jira-bridge`, `push-bridge`, `ava-log-shipper` — were still queued behind
this session's runner-concurrency ceiling when sampled and are estimated at
the family average below, not separately measured).

- n=14, sum=1777s, mean=127s
- 19-image family projected total: 1777s + 5×127s ≈ **2412s ≈ 40.2 min**
  summed job time (this is aggregate compute, not wall clock — actual wall
  clock depends on the runner concurrency ceiling, currently 4).

## Proposed common base layer

`images/_controller-base/Dockerfile` (new): one `golang:1.25-bookworm`
builder stage that runs `go build` in a loop over all 19
`controller/cmd/<name>` targets with a single shared `GOCACHE`, writing each
binary to `/out/<name>`. A `FROM scratch AS artifacts` final stage holds just
`/out/` so the pushed image is small — pulling it doesn't drag the Go
toolchain or `controller/` source along.

Each per-image Dockerfile becomes:

```dockerfile
ARG CONTROLLER_BASE
FROM gcr.io/distroless/static-debian12:nonroot
ARG CONTROLLER_BASE
COPY --from=${CONTROLLER_BASE} /out/<name> /<name>
USER nonroot:nonroot
EXPOSE <port>
ENTRYPOINT ["/<name>"]
```

`CONTROLLER_BASE` has no default: a build that forgets to pass it fails loudly
(invalid `--from=` reference) instead of silently pulling a stale or
nonexistent `:latest` — the same "merged-but-unappliable" failure mode called
out on PR #19's review.

Prototyped on 5 representative images spanning the observed range:
`roles-server` (76s, fast), `core-log-shipper` (111s, mid-fast),
`slack-bridge` (163s, mid), `alertmanager-bridge` (168s, mid), `gitlab-bridge`
(221s, slowest). The other 14 family Dockerfiles are mechanically identical
edits (swap the `FROM golang:1.25-bookworm AS builder` + `COPY` + `RUN go
build` block for the `ARG CONTROLLER_BASE` + `COPY --from` block above), not
applied here to keep the diff reviewable.

## Projection method and caveats

**What's real, from CI:** every "before" number above is measured from an
actual GitHub Actions run via the API — not simulated (except the 5
still-queued images, which use the family average and are flagged as such).
The `go build` (89-91s) vs `mode=max` cache export (~100-103s) breakdown is
read directly from job log timestamps.

**What's real, from a local experiment on this sandbox:** this sandbox has
no Docker daemon (`docker version` succeeds for the client, fails to
connect — no `dockerd`; no `buildx` plugin either) and no way to install one
(unprivileged container, no root), and `.github/workflows/images.yml` is off
limits to crew regardless — so the shared-`GOCACHE` claim was tested
directly with the Go toolchain instead of through Docker, from
`gardenio/controller`:

| build (in order) | wall time | note |
|---|---|---|
| `go build ./cmd/slack-bridge/` | 9m31s | cold `GOMODCACHE` *and* cold `GOCACHE` — dominated by `go: downloading …` (dozens of modules); not comparable to CI's 89-91s, which clearly runs against warm/fast module resolution every time (CI never persists `GOCACHE` either, so its 89-91s is the fair "before" number, not this one) |
| `go build ./cmd/gitlab-bridge/` | 25.0s | modules now warm, `GOCACHE` partially warm from the previous build |
| `go build ./cmd/roles-server/` | 3.4s | both warm |
| `go build ./cmd/core-log-shipper/` | 3.2s | both warm (one small extra module download) |
| `go build ./cmd/alertmanager-bridge/` | 4.1s | both warm |

Once the shared `controller` package graph is compiled once, each
*additional* binary that imports overlapping internal packages costs
single-digit seconds, not ~90s — a 4-image average of **8.9s marginal**,
directly against real `cmd/` targets in this repo, not a synthetic example.
This is the mechanism the shared base layer leans on, and it's now measured,
not just asserted.

**Combining the two measurements into one projection:** CI's 89-91s
`go build` step is the right stand-in for "first binary, warm module
resolution, cold `GOCACHE`" (since CI never carries a warm `GOCACHE`
between jobs either) — call it 90s. Each of the other 18 binaries, sharing
that `GOCACHE` the way `gitlab-bridge`/`roles-server`/`core-log-shipper`/
`alertmanager-bridge` did above, is estimated at the measured 8.9s average
(rounded up to 15s for slack in the estimate, since the local hardware and
CI's runner aren't the same machine):

- compile all 19: 90s + 18 × 15s = **360s**
- cache export, once instead of 19 times: **~100s**
- 19 trivial per-image final stages (`COPY --from=<base>` + distroless),
  single-digit seconds each: **19 × 15s ≈ 285s**
- **total ≈ 745s ≈ 12.4 min**, against the ~2412s ≈ 40.2 min measured
  baseline — a **~69% reduction**.

This is still a projection, not a CI run: the per-binary marginal cost was
measured with the Go toolchain directly, not inside `docker buildx build`,
and on this sandbox's hardware rather than a `gardenio-k3s` runner. Both of
those could shift the real number in either direction. What it is *not* is
guesswork about whether `GOCACHE` sharing helps at all — that part is now a
measured fact on this repo's actual `cmd/` binaries, not an assumption.
Confirming the Docker-level number still needs a real
`docker buildx build` of `images/_controller-base/Dockerfile` on a machine
with Docker, which this sandbox cannot do.

## What's still needed

1. ~~Validate `images/_controller-base/Dockerfile` builds and its binaries
   come out correct~~ — done: no Docker was available in the prototyping
   sandbox, so this PR's own `images` CI run was the validation. Run
   https://github.com/mattbakerconsulting/gardenio/actions/runs/35500200278
   passed end-to-end, including `controller-base` and all 5 rewired images.
   That run also caught a real bug the sandbox couldn't have: BuildKit
   rejects `COPY --from=${ARG}` referencing an external image directly
   ("variable expansion is not supported for --from"); fixed by pulling
   `CONTROLLER_BASE` through a named `FROM ${CONTROLLER_BASE} AS
   controller_base` stage first, per BuildKit's own suggested workaround.
2. ~~Wire `.github/workflows/images.yml`~~ — done by this PR: a
   `controller-base` job builds+pushes that image (tagged by
   `${{ github.sha }}`, never `:latest` — a concurrent build on another ref
   could overwrite it between the base build and a downstream image build
   reading it), and the 5 rewired images' matrix entries depend on it and
   pass `--build-arg CONTROLLER_BASE=ghcr.io/.../controller-base:${{ github.sha }}`.
3. Decide how `COVER=1` coverage builds interact with a shared base (today
   each image can independently opt into an instrumented build; a shared
   base would need to build both variants, or coverage builds keep using the
   old per-image path). Not resolved in this prototype — `COVER=1` isn't
   wired into `images.yml` at all today, so this is unaffected by this PR.
4. ~~Rewire the remaining 14 family Dockerfiles onto `CONTROLLER_BASE` now that
   the workflow job exists — a second task, kept separate so a workflow
   revert never strands half-rewired images (gd-t5tyQQLq9n).~~ — done
   2026-09-22, see "Phase 3" below. It stayed undone for long enough to cost
   real queue time, which is why it now has a guard.
5. The `controller` and `kbeads` images (outside `gardenio/images/**`) show
   the identical `golang:1.25-bookworm` + `COPY controller/` shape and are
   strong follow-on candidates for the same base layer, but are out of this
   task's stated scope (`gardenio/images/**`) and untouched here.

## Precedent: the split-COPY optimization already exists, just not here

`gardenio/controller/Dockerfile` and `kbeads/Dockerfile` (both outside this
task's `gardenio/images/**` scope) already split `COPY go.mod go.sum ./` +
`RUN go mod download` from the later `COPY controller/ ./` — the standard
Docker-Go layering that lets dependency downloads cache independently of
source edits. None of the 19 `images/**` Dockerfiles do this; they `COPY
controller/ ./` as one undifferentiated layer. This is a smaller,
independent fix (helps `go mod download` caching, not `go build` compile
time — the bottleneck measured above) and is not applied here to keep this
prototype's diff focused on the base-layer consolidation, but it is a
low-risk complementary change with established precedent elsewhere in the
repo, and one that doesn't require any workflow change to land.

## Phase 3 (2026-09-22): the other fourteen, and why it needed a guard

Phase 2 landed `images/_controller-base/` building **all twenty** binaries and
wired **six** images onto it. The remaining fourteen kept their own
`FROM golang:1.25-bookworm AS builder` + `COPY controller/ ./` + `go build`,
so the shared package graph was compiled a second time, in each image's own
pod, on a whole 4-vCPU node each. Nothing was broken — that is the problem.
A self-compiling image produces a correct image; it just puts a ~150 s job
where a ~50 s one belongs, and the symptom shows up as queue wait in some
other workflow entirely.

The cost was concentrated, not spread. `gardenio/controller/**` matches 21 of
the 26 path filters in `images.yml`, so any controller change fans the matrix
out to 21–26 heavy jobs at once against a 10-slot pool. Sampled over 17
`images` runs, the matrix size was bimodal — 2, or 21–26 — and the big shape
was the majority. The largest single push held 27 heavy jobs.

What landed:

- The fourteen Dockerfiles rewired (`ava-log-shipper`, `beads-viewer`,
  `bridge-log-shipper`, `claudemore`, `edge-auth`, `grafana-bridge`,
  `idp-log-shipper`, `jira-bridge`, `push-bridge`, `repro-cockpit`,
  `scif-portal`, `traefik-log-shipper`, `trigger-engine`, `wl-bridge`) and
  flagged `"controllerBase": true` in `.github/images.json`. Twenty of the
  twenty-six images are now on the shared base. The six that are not —
  `kbeads`, `controller`, `agent`, `coop-claude`, `coopmux`,
  `ziti-ctrl-backup` — either build from a different context or build
  binaries the base does not.
- `beads-viewer` also needed `gb`, which the base did not build. It is now
  built there, deliberately **outside** the `COVER_FLAGS` loop: `beads-viewer`
  runs `gb` as a subprocess to render the agent prime view, and an
  instrumented `gb` would write coverage files from inside a request path.
- `wl-bridge` got a second, independent fix. Its dolt install was a `RUN` in
  the golang builder placed *after* the `go build` that takes `ARG COMMIT` in
  its `-ldflags`. A stage's layer cache is invalidated from the first changed
  instruction downwards, so every commit re-downloaded, re-verified and
  re-installed a ~100 MB tarball — which is why it was the slowest image in
  the matrix at 285 s median. dolt now has its own stage, depending on
  nothing that changes per commit, so it caches until `DOLT_VERSION` does.
- The `controller-base` job's `if:` no longer enumerates image names. It
  reads `needs_base`, which the `changes` job derives from `images.json` with
  `jq 'any(.[]; .controllerBase == true)'`. That hand-kept list *was* the
  drift mechanism: it named six images while `images.json` was the source of
  truth, and duplicating it is what let fourteen images go un-migrated
  without anything noticing.
- `scripts/check-controller-base.py` (+ `_test.py`, run by the
  `shell guards + script self-tests` job) now enforces the invariant in both
  directions: a flagged image must actually pull from the base, and an
  unflagged image built from the `gardenio` context must not compile a binary
  the base already builds. Run against the tree as it stood immediately
  before this change, it names exactly those fourteen images — the guard
  retrodicts the drift it was written for.

Expected effect, stated as a checkable claim rather than a measurement: a
full-matrix run should go from ~2700 node-seconds / ~790 s wall to ~1340 /
~440, and a 26-image burst should drain in about three rounds through the
10-slot pool instead of holding 21–26 whole nodes for the length of a Go
compile. Verify against the first full-matrix `images` run on `main` after
this lands.

### Verified: dispatch 35694871982, all 26 green

| | before | projected | measured |
|---|---|---|---|
| base | 337 s | 337 s | **228 s** (warm cache) |
| the twenty | 14×~150 + 6×~50 | 20×~50 = 1000 s | **713 s** (mean 35.6, max 44) |
| family node-seconds | ~2700 | ~1340 | **941** (~1050 pricing a cold base) |
| family wall | ~790 s | ~440 s | 590 s |

Compute beat the projection by ~30%; both terms came in under it. Wall did
not, and it is worth being precise about why rather than averaging it away:
the same run accumulated 2612 summed queue-seconds across those 21 jobs
against concurrent crew PR traffic. Node-seconds is what this change
controls. Wall is pool contention, and belongs to `ci_max_nodes`.

Two results worth keeping:

- **`wl-bridge`: 285 s → 44 s.** The largest single drop in the matrix, and
  only partly the rewiring — it also stopped re-downloading a ~100 MB dolt
  tarball on every build. That `RUN` sat after a `go build` whose `-ldflags`
  embed `ARG COMMIT`, so its layer could never cache across commits; moving
  dolt to its own stage ahead of the compile fixed it. Cache invalidation
  cascades *downward* within a stage, so where an expensive `RUN` sits
  relative to a commit-varying one is not a style question.
- **The base is now the majority of the family's wall.** 324 s of the 590 is
  the base building plus its own queue wait, all serial, ahead of twenty
  ~36 s jobs. Having cut the compute, the remaining wall lever is the base
  itself — not the leaves.

### The base's build cache was costing more than it saved

Pulling that lever turned up a cache that had been running at a loss. The
`controller-base` job carried
`cache-to: type=registry,...,mode=max`, and it never once cached the thing
that matters.

It cannot. The expensive layer is the 20-binary `go build`, and `COMMIT` is
interpolated into that `RUN`'s own command text through `-ldflags`, so the
layer digest is unique per commit *by construction* — the same mechanism
documented above for `wl-bridge`'s dolt tarball, one level up. `mode=max`
faithfully exported cache for a layer that can never be hit.

Measured across the 11 successful `controller-base` builds in the 30 `images`
runs to 2026-09-22 06:29Z:

| | |
|---|---|
| cache export | 64–90 s (mean ~75) |
| job total | 221–341 s |
| builds where the compile step was `CACHED` | **0 of 11** |
| steps that ever hit | `WORKDIR` + the two `COPY`s, ~13 s — and missed in 4 of 11 |

On dispatch 35694871982 the export ran 06:31:39–06:32:43 while the image
export had already finished at 06:31:44: **~59 s of pure serial tail**, on the
one job all twenty leaves wait behind. So it was removed.

Two things this does *not* generalise to. The leaf images keep `mode=max`
(gated off on PRs) because their runtime stages have apt layers that genuinely
repeat across commits; the base has no such layer. And with no cache to
restore from, splitting `go.mod`/`go.sum` into their own `go mod download`
layer is no longer worth a layer either — module download is only ~10–20 s of
the compile, with 152 of 169 `go: downloading` lines landing in its first 10
seconds.

The honest general lesson is narrower than "caches are good": **a registry
cache is only worth its export when some expensive layer's key can actually
repeat.** Check that before paying for it, and check it by reading `CACHED` out
of real build logs rather than by assuming.
