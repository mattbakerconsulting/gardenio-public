# Go build/module cache prototype for ephemeral CI runners (gd-0gIRcZbXHY)

Task: gd-0gIRcZbXHY, epic: gd-irs5C9BEBi, lever #4 of the phase-2 ranking
(gd-503hW3MMJN / `gardenio/docs/ci-minutes-phase2-ranking.md`). Measured
2026-09-21/22 on branch `agent/ci-speed/gd-0gIRcZbXHY`.

## TL;DR

- **Shape (a), free/node-local:** already in effect today (`ci.yml`'s go
  job relies on `GOCACHE`/`GOMODCACHE` persisting on the runner's own disk —
  see below) and it is **measurably delivering ~0% effective benefit**: 8
  real `ci.yml` production runs each for kbeads and controller all land
  within the "cold" range this prototype measured, none anywhere near the
  "warm" numbers below.
- **Shape (b), real/paid, approximated with `actions/cache`:** works and
  is a **real, large** win when the cache actually lands — kbeads
  build+vet+test drops 142s → 18s (**-87%**), controller drops ~470s
  (median of 3 measured cold runs) → 161s (**-66%**) — but the *upload*
  (cache-save) step that makes it a "real" persistent cache is **wildly
  unreliable**: the same ~700MB controller payload took 83s to save in one
  run and had not finished after >15.5 minutes in another (job hit its
  20min timeout mid-upload and was cancelled). That's the exact failure
  mode `ci.yml`'s own comment already names ("the actions cache upload
  took 18 minutes over a home uplink and timed out an otherwise green
  job") — this prototype reproduces it directly, with numbers.
- **Recommendation:** the *paid* shape's payoff is real and worth
  pursuing, but not via GitHub's hosted `actions/cache` — its
  upload-over-the-internet step is the demonstrated failure point. An
  in-cluster cache server (registry-backed proxy or small PVC) or an
  S3-backed proxy in the same AWS region avoids that internet hop
  entirely and should not exhibit this variance, though that specific
  claim is un-measured (building it is out of this task's scope — see
  Standing rules). A decision bead is filed for operator sign-off before
  anyone builds it (see below).
- **Orthogonal finding, not this task's lever but directly relevant:**
  queue-wait for a job to even *start* on `gardenio-k3s` grew from ~2min to
  ~23min to ~50min across three pushes on this same branch over about 3
  hours (2026-09-21 22:50Z → 2026-09-22 01:30Z), while other `ci`/`checks`/
  `images` runs piled up fleet-wide. That dwarfs anything a Go cache can
  save and directly corroborates the ranking doc's #3 (runner concurrency,
  gd-uGKVgBc63f) as the lever that actually fixes wall-clock time-to-green.

## Method

Per the task's guidance, no `.github/workflows/{ci,checks,images}.yml`
edit was made. A throwaway workflow,
`.github/workflows/_proto_go_cache.yml`, was added on this branch only —
triggered on `push` to `agent/ci-speed/gd-0gIRcZbXHY` and path-filtered to
itself, so it never runs on any other ref. It mirrors `ci.yml`'s `go` job
(same matrix: kbeads + `gardenio/controller`, same `build`/`vet`/`test`
steps, same `runs-on: gardenio-k3s`) but adds an explicit
`actions/cache@v4` step around `GOCACHE`+`GOMODCACHE`, keyed on each
module's `go.sum` hash, as an instrumented stand-in for "a persistent
cache the ephemeral runner pods can read/write" (`actions/cache` is a
GitHub-hosted service, not literally an in-cluster server, but it exercises
the same shape: compress → upload/download over a network hop → restore
into `GOCACHE`/`GOMODCACHE`).

Three pushes to that branch produced three real runs on the actual
`gardenio-k3s` ARC pool:

| Run | Commit | Cache state going in |
|---|---|---|
| 1 | `a0d8d47` | cold — no cache exists for either module |
| 2 | `f63e1bb` | kbeads warm (run 1 saved it); controller still cold (run 1's save never finished) |
| 3 | `829b80c` | both warm |

All timings below are step-level (`started_at`/`completed_at` from the
GitHub Actions API, i.e. real wall-clock, not self-reported).

## Real numbers

### kbeads

| | cold (run 1) | warm (run 2) | warm (run 3) |
|---|---|---|---|
| cache restore | 1s (miss) | 8s (hit, 327MB) | 7s (hit) |
| build | 70s | 4s | 4s |
| vet | 27s | 3s | 3s |
| test | 45s | 11s | 11s |
| **build+vet+test** | **142s** | **18s** | **18s** |
| cache save | 39s (327MB, succeeded) | 1s (no-op, same key) | — |

build+vet+test warm is a consistent **18s both times** (-87% vs cold) —
the measurement is stable, not a fluke.

### controller

| | cold (run 1) | cold-ish (run 2)† | warm (run 3) |
|---|---|---|---|
| cache restore | 0s (miss) | 0s (miss — run 1's save never landed) | 19s (hit, 736MB) |
| build | 242s | 181s | 53s |
| vet | 58s | 44s | 11s |
| test | 241s | 202s | 97s |
| **build+vet+test** | **541s** | **427s** | **161s** |
| cache save | started, **cancelled at 20min job timeout, still in-flight** | 83s (736MB, succeeded) | — |

† run 2's 427s vs run 1's 541s (both cache-miss, i.e. both "cold") is
run-to-run variance from node placement/contention, not caching — a
reminder that any single before/after pair on this pool has real noise;
this is why the cold side has two independent samples here, not one.

warm build+vet+test (run 3, cache hit) is 161s vs a two-sample cold
average of 484s — **-67%** (-70% vs run 1, -62% vs run 2).

### The cache-save reliability problem (the actual finding)

Both controller cache-save attempts were saving effectively the same
payload (736MB — go.sum unchanged across all three runs, so same key,
same module set):

- Run 1: started at cache-save, **still running after 15m33s**, when the
  job's 20-minute total timeout killed it. No cache object exists from
  this save (confirmed via `gh api .../actions/caches` — only kbeads'
  key existed after run 1).
- Run 2 (timeout bumped to 40min to find out if it "just needed more
  time"): completed in **83s**.

That's not "slow," it's **unpredictable by more than an order of
magnitude for the same payload on the same runner class**. This is the
identical shape of problem `ci.yml`'s own comment already flags for
`setup-go`'s built-in cache (`cache: false` — "the actions cache upload
took 18 minutes over a home uplink and timed out an otherwise green
job"). This prototype didn't just corroborate that comment qualitatively,
it reproduced the same failure with fresh timestamps three years^H^H^H one
day later, on a different (matrix-scoped, hand-rolled) cache step.

## Cross-check against real production `ci.yml` runs

Per the task's suggested method, real baseline durations were pulled from
the GitHub Actions API for `ci.yml`'s actual `go` job (8 most recent
successful `main`-branch runs each, 2026-09-21 20:40Z–2026-09-22 01:24Z):

| Module | Production `ci.yml` (8 samples) | This prototype's cold | This prototype's warm |
|---|---|---|---|
| kbeads | 102–191s, median 156s, mean 156s | 142s | **18s** |
| controller | 338–685s, median 472s, mean 493s | 541s / 427s | **161s** |

Two things this confirms:

1. **The prototype's cold numbers are representative** — they land inside
   the real production range for both modules, not an outlier.
2. **Shape (a) — the node-local `GOCACHE`/`GOMODCACHE` persistence `ci.yml`
   already relies on — is not delivering any observable benefit today.**
   `ci.yml`'s own comment says "GOCACHE/GOMODCACHE persist on the node",
   which is true only as long as the *same* runner pod handles consecutive
   jobs; across 8 samples spanning ~5 hours of real traffic, **every single
   one** falls in the cold range. None is anywhere close to this
   prototype's 18s/161s warm numbers. On a pool that's 100% spot and scales
   to zero after ~10 idle minutes, with queue-waits now observed up to
   ~50 minutes between this task's own pushes (see below), the odds of the
   same physical node handling two related jobs back-to-back are
   apparently close enough to zero to be invisible in this sample.

## Orthogonal but directly relevant: queue-wait grew across this prototype's own three runs

| Run | Pushed | Job started | Queue wait |
|---|---|---|---|
| 1 | 22:50:23Z | 22:52:23Z (kbeads) / 22:52:48Z (controller) | ~2min |
| 2 | ~00:06:3xZ | 00:29:13Z (controller) / 00:30:15Z (kbeads) | ~23min |
| 3 | 00:40:30Z | 01:28:03Z (kbeads) / 01:30:03Z (controller) | ~47–50min |

`gh run list` during run 3's wait showed several other `ci`/`checks`/
`images` runs from unrelated branches also queued on the same
`gardenio-k3s` pool. This is not this task's lever to fix (that's
gd-uGKVgBc63f / gd-yZcr1vYhpw / gd-q6e62AXt3y), but the magnitude matters
for prioritization: a ~50-minute queue wait to *start* a job dwarfs the
~1–6 minutes a warm Go cache saves once it starts. Caching shrinks *how
long the job takes once it runs*; it does nothing for *how long it waits
to run*. This is exactly the distinction the ranking doc's "why this
order" section already draws between lever #3 and lever #4 — this
prototype's own run history is a live, timestamped example of it.

## Generalization to cargo (gd-7TZUtKhGcw)

gd-7TZUtKhGcw is blocked on this task's cache-mechanism choice for
cargo's registry+target dir, not on a second mechanism. The finding that
generalizes directly: **whatever persistent-cache mechanism gets built,
route it through the cluster's own network, not through an external
hosted service reached over the CI pool's internet uplink.** The failure
mode measured here (unreliable multi-hundred-MB uploads, 83s–15.5min+ for
the same payload) is a property of the upload path, not of Go
specifically — cargo's registry+target-dir cache for `coop` (avg 517s/run,
per gd-0gIRcZbXHY's tracking comment) is a comparable or larger payload and
would be expected to hit the identical variance if pointed at the same
kind of external hosted-cache service. An in-cluster registry-backed proxy
or S3-in-region bucket sidesteps this for both languages; this was not
built or measured here (out of scope — see Standing rules below), only
inferred from where the bottleneck actually is.

## Standing rules honored

- No edit to `.github/workflows/{ci,checks,images}.yml` — only the
  throwaway, branch-scoped `_proto_go_cache.yml` (diff vs
  `origin/main`'s merge-base is exactly one new file, verified via
  `git diff $(git merge-base origin/main HEAD) HEAD --stat`).
- No edit under `gardenio/terraform/**`, `gardenio/helm/**`,
  `deploy-values/**`, or any RBAC/ServiceAccount/secret — the node-local
  (shape a) question above is answered from real `ci.yml` production
  history, not by deploying anything.
- No PR opened: the throwaway workflow only triggers on push to this
  branch, so it already executed on the real ARC pool without needing a
  PR. Per the task's acceptance note, a pushed branch + doc bead is
  therefore enough to close.
- Recommending the paid shape without deciding it — a decision bead is
  filed for the operator (see gd-0gIRcZbXHY's summary comment for the
  bead id), matching how gd-uGKVgBc63f and gd-eRwlok5zKY did it.
