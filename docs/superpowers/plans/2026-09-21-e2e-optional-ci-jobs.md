# Optional e2e jobs in GitHub Actions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. In gardenio this plan is executed as a plan epic: one bead per task below, phase epics in `phase_order`, crews dispatched by the bound plan-gardener. Each task is one branch `agent/e2e-ci/<task-bead-id>` and one PR.

**Goal:** Every gardenio e2e suite that can run without a live cluster runs in GitHub Actions — on a PR label, on `workflow_dispatch`, and nightly — and each job is proven green on a real run before its task is done.

**Architecture:** One new workflow file `.github/workflows/e2e.yml` with a cheap `select` job that resolves which suites to run from the event and the dispatch inputs, plus five independent suite jobs gated on its output. Nothing is a required check. The shell tier gets a third curated subset inside the existing `run-shard.sh` (`HERMETIC_ONLY`) rather than a second driver. `ci.yml` and `checks.yml` are untouched; the single edit to `retry-lost-runners.yml` is the one its own comment invites.

**Tech Stack:** GitHub Actions on self-hosted ARC scale sets (`gardenio-k3s` dind, `gardenio-k3s-light`); Go 1.25 (`kbeads`, `gardenio/controller`); Rust (`coop`, workspace `coopmux`); Node 24 + Playwright 1.58.2; bun 1.3.13; PostgreSQL 16 + pgvector; bash (`run-shard.sh`).

**Spec:** `docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md`

## Global Constraints

- **Every tool install is a sha256-verified release tarball**, pins sourced from `gardenio/images/agent/install/versions.env` and `gardenio/images/agent/install/checksums.env`. Never `npm i -g`, never `curl | sh`, never `cargo install` per run. The pattern to copy is the quench gate — the `quench` job in `.github/workflows/checks.yml`, whose install step curls the pinned release tarball and pipes it through `sha256sum -c --status -`.
- Relevant pins, verbatim: `NODE_FULL_VERSION=24.15.0`, `NODE_SHA256_X64=472655581fb851559730c48763e0c9d3bc25975c59d518003fc0849d3e4ba0f6`, `GO_VERSION=1.25`, `GO_FULL_VERSION=1.25.9`, `PLAYWRIGHT_VERSION=1.58.2`, `BUN_VERSION=1.3.13`, `BUN_SHA256_X86_64=79c0771fa8b92c33aae41e15a0e0d307ea99d0e2f00317c71c6c53237a78e25a`.
- **`cache: false` on every `actions/setup-go@v5`.** An Actions cache upload once took 18 minutes and timed out an otherwise green job — see the `cache: false` rationale comment in `.github/workflows/ci.yml`'s `go` job. Cold builds are the accepted cost on ephemeral runners. Likewise no `rust-cache`, no `actions/cache` anywhere in this plan.
- **`runs-on` values are exactly three**, copied with their inline comments: `gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)`, `gardenio-k3s-light # light ARC scale set (gardenio/helm/ci) -- adversarially verified safe 2026-09-21, see arc-runners-light-values.yaml`, `gardenio-k3s-medium # medium ARC scale set (gardenio/helm/ci) -- short CPU-bound jobs, no docker; see arc-runners-medium-values.yaml`. No `ubuntu-latest`: the account's hosted-Linux minutes are exhausted until October.
- **The runner image ships nothing.** No C toolchain, no `xz`, no Node, no Rust, no `jq` beyond the light image's, no `unzip`. Every job installs what it needs. `build-essential` is required for any cgo or cargo build.
- **`go test ./...` does not work from the repo root** — the modules have local-style paths (`gardenio/kbeads`, `gardenio/controller`) and `go.work` lists five `use` dirs. Always `working-directory:` the module dir, exactly as `ci.yml`'s matrix does.
- **Suite jobs are never required checks** and never gate a merge. `continue-on-error: true` on `go-integration` only.
- **Branch convention:** one task = one branch `agent/e2e-ci/<task-bead-id>` = one PR. The `push: branches: ['agent/e2e-ci/**']` trigger exists so each crew can prove its own job green before requesting review.
- **Read CI results with `gh run list` / `gh run view`, never the check-runs API.** The crew PAT gets 403 on `/check-runs` (`gd-XldfmHq3ez`; operator decision `gd-4j5m4jtW34` still open). Keep the `gh run list` fallback until that clears.
- **A failed job with no failed step and a "lost communication with the server" annotation is a spot reclaim, not a bug.** Re-run it (`gh run rerun <id> --failed`); never change code in response to one.
- **NOT DONE UNTIL MERGED** (working-rules advice `gd-oI2AIaSzjF`). And for every task that adds or changes a CI job, not done until **a green run of that job on its own branch is linked from the bead** — not "the YAML is committed".
- Never touch terraform, helm values, RBAC, secrets or ziti config from this plan. The only file outside `.github/workflows/e2e.yml`, `gardenio/tests/e2e/scripts/`, and docs that this plan edits is `.github/workflows/retry-lost-runners.yml`, and only the two edits named in T0 and T2. **No Go source file is edited by any task** — T7 used to edit one and no longer does.
- **Cite `ci.yml` and `checks.yml` by job and step name, never by line number.** A peer session owns `.github/workflows/` and is actively editing both files — three commits landed there while this plan was being written, moving every anchor in them. Line anchors into `docs/archive/rwx/root/ci.yml` are fine: that file is frozen.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `.github/workflows/e2e.yml` | Create (T2), extend (T3, T4, T6, T7, T8, T9) | The whole optional-e2e surface: triggers, `select`, and one job per suite. |
| `.github/workflows/retry-lost-runners.yml` | Modify (T0 `:88`+guard, T2 `:88`) | Add `e2e` to the `workflow_run` allowlist; exempt scheduled runs from the staleness guard. |
| `docs/ci-e2e-postgres-recipe.md` | Create (T1) | The decided postgres+pgvector recipe for ARC dind, and why the loser lost. Consumed verbatim by T3 and T8. |
| `gardenio/tests/e2e/scripts/run-shard.sh` | Modify (T5) | Gains `HERMETIC_ONLY` — a third curated subset plus a conditional cluster preflight. |
| `gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh` | Create (T5) | Fails if any module in `HERMETIC_MODULES` acquires a `kubectl`/`kd`/`gb`/`helm` dependency. |
| the 7 `//go:build e2e` files | Read only (T7) | Measured to pass or skip cleanly as-is. No Go source file is modified by this plan — see T7. |
| `.github/known-failures.md` | Modify (T8, T10) | Records `go-integration`'s `continue-on-error` debt and its disposition. |
| `gardenio/docs/user-guide/11-e2e-testing.md` | Rewrite (T11) | Currently documents the RWX/GitLab flow deleted on 2026-09-16. |

## Phases

| `phase_order` | Phase | Tasks |
|---|---|---|
| 0 | Runner-loss retry | T0 |
| 1 | Foundation and Playwright | T1, T2, T3, T4 |
| 2 | Shell and Go tiers | T5, T6, T7, T8 |
| 3 | Make it stick | T9, T10, T11, T12 |

Dependencies: T2 blocks T3, T4, T6, T7, T8, T9. T1 blocks T3 and T8. T5 blocks T6. T8 blocks T10. T3, T4, T6, T7 and T8 all block T12, which right-sizes the runner tier of every job they add and so cannot run until all five exist. T0 is gated on operator decision O1 and blocks nothing — it is Phase 0 because it protects every later job, not because anything waits on it. T9 is blocked by `gd-IoVkE0k8ot`.

---

## Phase 0: Runner-loss retry

### Task 0: Exempt scheduled runs from the retry staleness guard (P0-b)

**DONE — O1 approved, landed as `d5edca64`** ("ci: exempt scheduled runs from the lost-runner staleness guard"), executed by crew agent `gardenio-task-0-exempt-7ps` on 2026-09-22. The steps below are kept as the record of what was changed and why; they are not work to redo.

**Line numbers below are the revision this task was written against (`197f5c8f`, 226 lines). The file is now 382 lines and four commits further on. Current anchors: allowlist `:176`, staleness guard `:274-310` (predicate `:294-296`), safety rails header `:102` with rail 3 at `:114-124`, attempt cap `:211`.** Do not edit by the old numbers.

**GATED ON OPERATOR DECISION O1** (resolved `approved`, 2026-09-22, operator delegated to laptop-claude). This edits decision logic in `.github/workflows/retry-lost-runners.yml`, which belongs to fimba-91's lane and landed hours before this plan.

**Files:**
- Modify: `.github/workflows/retry-lost-runners.yml:173-193` (the staleness guard) and `:54-61` (the safety-rail comment describing it)
- Test: no automated test exists for this workflow; validation is a `workflow_dispatch` dry run against a real scheduled run id (see Step 4)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: nothing other tasks import. T9 relies on the *behaviour* (a reclaimed nightly retries) but not on any symbol.

- [ ] **Step 1: Read the guard and its rationale comment**

Read `.github/workflows/retry-lost-runners.yml` lines 44-62 and 167-195. The two pieces that change are the comment at `:54-61` (safety rail 3, inside the `── SAFETY RAILS ──` header block) and the `if (run.path !== ...)` test at `:180`.

- [ ] **Step 2: Widen the exemption**

Replace, at `.github/workflows/retry-lost-runners.yml:173-180`:

```javascript
            // Staleness guard. Exempt `images` only: its per-commit path
            // filtering means an older sha's build is never redone by a newer
            // run, so it must be retried even when the branch has moved on.
            // Everything else re-tests the same code and IS superseded.
            // Keyed on `path`, not `name`: a future `run-name:` in a workflow
            // would change `name` and silently break the exemption.
            if (run.path !== '.github/workflows/images.yml') {
```

with:

```javascript
            // Staleness guard, with two exemptions.
            //
            // `images`: its per-commit path filtering means an older sha's
            // build is never redone by a newer run, so it must be retried
            // even when the branch has moved on. Keyed on `path`, not `name`:
            // a future `run-name:` in a workflow would change `name` and
            // silently break the exemption.
            //
            // `schedule`: a nightly's head_branch is `main`, and main moves
            // constantly, so this guard would SKIP every reclaimed nightly
            // as "superseded" -- the same failure mode the guard already
            // hit on a push run (35669237327 classified 35667563888 this
            // way at 23:48Z on 2026-09-21). But nothing supersedes a nightly:
            // there is no newer run until tomorrow, so a reclaimed one is
            // lost for a day. That is the same argument as `images`. The
            // nightly is the only thing keeping the e2e suite from rotting
            // again (see docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md D3),
            // so losing it silently defeats the point.
            //
            // Everything else re-tests the same code and IS superseded.
            const exemptFromStaleness =
              run.path === '.github/workflows/images.yml' ||
              run.event === 'schedule';
            if (!exemptFromStaleness) {
```

- [ ] **Step 3: Update safety rail 3 in the header comment**

Replace, at `.github/workflows/retry-lost-runners.yml:54-59`:

```
#  3. Staleness guard, every workflow except images. If the run's branch has
#     moved past its head sha, a newer run already covers the same tests and
#     retrying this one is pure queue load -- which is worst precisely during
#     the churn storms that cause the reclaims. `images` is exempt: it filters
#     paths per-commit, so an older sha's image build is NOT superseded by a
#     newer one and is lost forever if it isn't retried (gd-R1kL4sUECV).
```

with:

```
#  3. Staleness guard, with two exemptions. If the run's branch has moved past
#     its head sha, a newer run already covers the same tests and retrying this
#     one is pure queue load -- which is worst precisely during the churn
#     storms that cause the reclaims. `images` is exempt: it filters paths
#     per-commit, so an older sha's image build is NOT superseded by a newer
#     one and is lost forever if it isn't retried (gd-R1kL4sUECV). `schedule`
#     runs are exempt for the same reason: nothing supersedes a nightly --
#     there is no newer run until tomorrow, so a dropped one costs a day of
#     coverage.
```

Leave lines 60-61 (`This applies to push runs too…`) untouched — they still hold.

**Correction, 2026-09-22.** As first landed, both this comment and the inline
rationale argued the exemption from "main moves constantly, so *every*
reclaimed nightly would be dropped". That is not true and the plan should not
be read as asserting it. Measured: simulating a 09:17Z start over the 7 days
to 2026-09-22, main had moved by retry-check time on **2 of 7** nights for a
30-minute run and **3 of 7** for a 60-minute one — lower bounds, because the
guard reads the branch tip when the retry check runs, not at run end, so the
exposure window is the run plus reclaim-detection latency. The 03:00-08:00Z
band it was argued from averages **3.7 commits/hour** with a median of 2 and
43% of band-hours empty, and that mean slides to 2.1 and 1.5 on adjacent
7-day windows — it is bursty and zero-inflated, not a rate. (Note the failure
direction: at the 5-9/hr figure originally asserted, a Poisson model puts the
trip probability at 92-99% over 30 minutes, i.e. the wrong number argued for a
*stronger* claim than the one drawn, while the real data argues for a weaker
one.) The exemption is justified by **correlation, not frequency**: the nights
main moves are the churn-heavy nights the nightly most needs to cover, and
those are exactly the ones the guard would silently drop. The live comments
were rewritten to say this.

- [ ] **Step 4: Prove the new branch with a dry run**

The workflow already offers a classify-only dispatch. Find a run that previously SKIPped on staleness and re-classify it:

```bash
gh run list --workflow=retry-lost-runners.yml --limit 20 \
  --json databaseId,conclusion,createdAt
gh run view 35669237327 --log | grep -i 'SKIP\|RETRY'
```

Then, once this change is on main (`workflow_run` workflows only ever execute the default-branch copy), classify a *scheduled* run that failed:

```bash
gh workflow run retry-lost-runners.yml -f run_id=<a failed scheduled run id>
gh run list --workflow=retry-lost-runners.yml --limit 1
gh run view <the new run id>
```

Expected: the job summary no longer says `branch main has moved to …`.

**There is no scheduled run to test against, and there never has been.**
Verified 2026-09-22: `actions/runs?event=schedule` returns `total_count: 0`
against 1806 runs total (push 837, pull_request 553, workflow_run 395,
workflow_dispatch 23), and `git log -S "cron:" --all -- .github/workflows/`
finds the string in exactly two commits, both the commented e2e skeleton. So
the positive path is **unverified by construction** — record that on the bead
and hand T9 the job of confirming it on the first nightly.

Two consequences worth stating plainly. First, the exemption's blast radius is
exactly the e2e nightly: `run.event === 'schedule'` cannot match anything else
today, and the `workflow_run` allowlist (`:176`, `[ci, checks, images, e2e]`)
bounds it further. Second, the code is dead until T9 lands, so its first
execution will be in production on a real reclaimed nightly.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/retry-lost-runners.yml
git commit -m "$(cat <<'EOF'
ci: exempt scheduled runs from the lost-runner staleness guard

A nightly's head_branch is main and main moves constantly, so the guard
SKIPped every scheduled run as "superseded". Nothing supersedes a nightly:
there is no newer run until tomorrow. Same argument as the existing images
exemption.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
```

---

## Phase 1: Foundation and Playwright

### Task 1: Spike — postgres + pgvector on ARC dind

A spike. Its deliverable is a **decided, written-down recipe**, not a lasting workflow. The spike workflow is deleted before the PR is opened; only the doc survives.

**Files:**
- Create (throwaway, deleted in Step 6): `.github/workflows/e2e-spike-postgres.yml`
- Create: `docs/ci-e2e-postgres-recipe.md`

**Interfaces:**
- Consumes: nothing.
- Produces: `docs/ci-e2e-postgres-recipe.md`, containing one fenced ```yaml block named `RECIPE-SERVICES` **or** one fenced ```bash block named `RECIPE-APT` — exactly one is marked `DECIDED`. T3 and T8 paste the decided block. The recipe must yield a database reachable at `postgres://beads:beads@localhost:5432/beads?sslmode=disable` with the `vector` extension created, because that DSN is hard-coded into T3's `kd serve` env and T8's `BEADS_TEST_DATABASE_URL`.

- [ ] **Step 1: Write the spike workflow, services path**

Create `.github/workflows/e2e-spike-postgres.yml`:

```yaml
# THROWAWAY. Spike for gd-<T1 bead id>: does ARC + containerMode dind support
# Actions service containers? Deleted in the same task that answers the
# question; the answer lives in docs/ci-e2e-postgres-recipe.md.
name: e2e-spike-postgres

on:
  push:
    branches: ['agent/e2e-ci/**']

jobs:
  services-path:
    name: postgres via services container
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    timeout-minutes: 15
    services:
      postgres:
        image: pgvector/pgvector:pg16
        env:
          POSTGRES_USER: beads
          POSTGRES_PASSWORD: beads
          POSTGRES_DB: beads
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U beads"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 20
    steps:
      - name: psql client
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends postgresql-client
      - name: create extension and prove it
        env:
          PGPASSWORD: beads
        run: |
          set -euo pipefail
          psql -h localhost -U beads -d beads -v ON_ERROR_STOP=1 \
            -c "CREATE EXTENSION IF NOT EXISTS vector;"
          psql -h localhost -U beads -d beads -tAc \
            "SELECT default_version FROM pg_available_extensions WHERE name='vector';"
          psql -h localhost -U beads -d beads -tAc \
            "SELECT extversion FROM pg_extension WHERE extname='vector';"
```

- [ ] **Step 2: Run it**

```bash
git checkout -b agent/e2e-ci/<T1 bead id>
git add .github/workflows/e2e-spike-postgres.yml
git commit -m "spike: postgres services container on ARC dind"
git push -u origin agent/e2e-ci/<T1 bead id>
gh run list --workflow=e2e-spike-postgres.yml --limit 1
gh run view <run id> --log
```

Expected on success: both `SELECT` queries print `0.8.1` (or whatever the image ships). Expected on failure: either the job never starts (service container unsupported under this ARC mode) or `psql: could not connect to server` (the port mapping does not reach the runner container).

- [ ] **Step 3: If services failed, run the apt path**

Replace the `services:` block and the two steps above with the archive's proven recipe (`docs/archive/rwx/root/ci.yml:2799-2817`), then push again:

```yaml
    steps:
      - name: postgres 16 + pgvector via apt
        run: |
          set -euo pipefail
          sudo apt-get update -qq
          sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
            postgresql-16 postgresql-16-pgvector >/dev/null
          sudo pg_ctlcluster 16 main start
          for i in $(seq 1 30); do
            if sudo -u postgres pg_isready -q; then break; fi
            sleep 1
          done
          sudo -u postgres psql -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
          CREATE USER beads WITH SUPERUSER PASSWORD 'beads';
          CREATE DATABASE beads OWNER beads;
          \connect beads
          CREATE EXTENSION IF NOT EXISTS vector;
          SQL
      - name: prove it
        env:
          PGPASSWORD: beads
        run: |
          set -euo pipefail
          psql -h localhost -U beads -d beads -tAc \
            "SELECT extversion FROM pg_extension WHERE extname='vector';"
```

Note: `postgresql-16` is only in Ubuntu 24.04's archive for some mirrors; if `apt-get install` cannot find it, add the PGDG apt source first — the repo already pins PGDG client debs by sha (`PGDG_PG17_CLIENT_DEB_SHA256_AMD64` in `checksums.env`), so record that as a third option in the doc rather than improvising in the job.

- [ ] **Step 4: Time both attempts**

From `gh run view <id> --json jobs`, record the wall-clock of the postgres setup step for whichever paths ran. The doc must state the cost, because `viewer-e2e` budgets 8–12 minutes total.

- [ ] **Step 5: Write the recipe doc**

Create `docs/ci-e2e-postgres-recipe.md`. Structure, with the real measured numbers substituted:

```markdown
# Postgres + pgvector for the e2e CI jobs

`viewer-e2e` and `go-integration` both need a PostgreSQL 16 with the `vector`
extension available. `kbeads/internal/store/postgres/migrate_preflight.go:44-69`
queries `pg_available_extensions` and hard-fails the migration without it;
migration `005_bead_embeddings` is the consumer.

Two mechanisms were tried on `gardenio-k3s` (ARC, `containerMode: dind`) on
2026-09-__ under gd-<T1 bead id>.

## DECIDED: RECIPE-SERVICES

<the exact services: block and the CREATE EXTENSION step, as run>

Setup cost: __s.

## REJECTED: RECIPE-APT

<the apt block>

Setup cost: __s. Rejected because ____.

## The DSN both jobs use

    postgres://beads:beads@localhost:5432/beads?sslmode=disable

`viewer-e2e` passes it as `BEADS_DATABASE_URL`; `go-integration` passes it as
`BEADS_TEST_DATABASE_URL`. Changing it means changing both jobs.
```

Exactly one heading is `## DECIDED:`; the other is `## REJECTED:`. If the services path worked, it is DECIDED — an image pull beats apt on every run. If it did not, apt is DECIDED and the services findings are the rejection rationale.

- [ ] **Step 6: Delete the spike workflow and commit only the doc**

```bash
git rm .github/workflows/e2e-spike-postgres.yml
git add docs/ci-e2e-postgres-recipe.md
git commit -m "$(cat <<'EOF'
docs: decide the postgres+pgvector recipe for the e2e CI jobs

Spike under gd-<T1 bead id>. The throwaway spike workflow is deleted; the
decision and the rejected alternative are recorded so the fallback is
documented rather than rediscovered.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
git push
```

- [ ] **Step 7: Open the PR and link the spike runs from the bead**

The PR contains one new doc and nothing else. Comment on the bead with the run URLs for both attempts (or the one, if services worked first try).

---

### Task 2: `e2e.yml` skeleton — triggers, `select`, wiring proof, and P0-a

Everything else in this plan is a job added to the file this task creates.

**PRECONDITION: the `e2e` repo label must exist** (operator decision O2). Verify with `gh label list | grep '^e2e'`. If it is absent, comment on the bead and on the O2 decision bead and stop — the label path cannot be demonstrated without it. The `push`-trigger path still works, so the crew may proceed with Steps 1-6 and leave Step 7 for after the label lands.

**Files:**
- Create: `.github/workflows/e2e.yml`
- Modify: `.github/workflows/retry-lost-runners.yml:88`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `needs.select.outputs.suites` — a JSON array string. Every later suite job gates on `if: contains(fromJSON(needs.select.outputs.suites), '<name>')`.
  - The suite-name vocabulary, fixed here: `wiring`, `viewer`, `coop`, `shell-hermetic`, `go-e2e`, `go-integration`. A later task adding a job adds its name to the `ALL` array in the same edit, or the job never runs.
  - The `on:` block, the `concurrency:` block and the `select` job's `if:` guard. Later tasks add jobs and touch nothing above `jobs:` except T9's `schedule:`.

- [ ] **Step 1: Write the workflow**

Create `.github/workflows/e2e.yml`:

```yaml
# Optional e2e jobs. Never a required check.
#
# ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
# gardenio has ~225 e2e shell modules, 68 Playwright viewer specs, 8 coop
# specs and 7 `//go:build e2e` Go files, and NONE of them has run in GitHub
# Actions since 8a3eb29e (2026-09-16) deleted the RWX pipelines that drove
# them. The scripts survived; the orchestration did not. Green CI has meant
# "unit and build level correctness" ever since, which is not what people read
# it as. Spec: docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md
#
# ── THREE DOORS ─────────────────────────────────────────────────────────────
#  1. An `e2e` label on a PR. `types: [labeled, synchronize]` fires for ANY
#     label, so the `select` job's `if:` re-checks that ours is present.
#  2. workflow_dispatch, with a suite filter. Same jq-narrowing shape as
#     images.yml's `changes` job, so a typo yields an empty selection and a
#     visible no-op rather than a surprise full run.
#  3. A nightly cron on main -- the anti-rot mechanism. A dispatch-only job is
#     green the day it lands and untested three weeks later, which is exactly
#     how the suite rotted the first time.
#
# Plus `push: agent/e2e-ci/**`, which is load-bearing rather than convenient:
# a new workflow's dispatch and schedule triggers do not exist until the file
# is on the default branch, and the crew PAT is Contents-only so a crew cannot
# label its own PR. The push trigger is how each job is proven green on its own
# branch. It is self-limiting and costs nothing when no such branch exists.
name: e2e

on:
  pull_request:
    types: [labeled, synchronize]
  push:
    branches: ['agent/e2e-ci/**']
  workflow_dispatch:
    inputs:
      suites:
        description: >-
          Space- or comma-separated suite names to run (e.g. "viewer" or
          "viewer coop"). Leave EMPTY to run all. Unknown names are dropped,
          so a typo selects nothing instead of running everything.
        required: false
        default: ""
  # The nightly is added by the T9 task of the e2e-ci plan, once the operator
  # has chosen the hour (decision O3). It is deliberately absent until then:
  # landing an un-timed cron against a shared 6-slot spot pool is how you
  # starve `ci`.
  # schedule:
  #   - cron: '<chosen hour> * * *'

# github.event_name MUST be in the group key. GitHub evicts the *pending* run
# whenever a new run joins a group, regardless of cancel-in-progress -- learned
# on images.yml (gd-R1kL4sUECV). Without the event split, a push to main at
# 07:05 silently kills the nightly, and the nightly is the whole point.
concurrency:
  group: e2e-${{ github.ref }}-${{ github.event_name }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

env:
  # go.work pins 1.25.12; keep CI on the same minor as the Dockerfiles.
  GO_VERSION: "1.25"

jobs:
  select:
    name: select suites
    # `types: [labeled]` fires for every label on the PR, not just ours, so
    # the guard is what makes the label an opt-in rather than a tripwire.
    if: >-
      github.event_name != 'pull_request' ||
      contains(github.event.pull_request.labels.*.name, 'e2e')
    runs-on: gardenio-k3s-light # light ARC scale set (gardenio/helm/ci) -- adversarially verified safe 2026-09-21, see arc-runners-light-values.yaml
    timeout-minutes: 5
    outputs:
      suites: ${{ steps.pick.outputs.suites }}
    # No checkout: this job reads the event context and the dispatch inputs
    # and nothing else.
    steps:
      - name: pick suites
        id: pick
        env:
          EVENT: ${{ github.event_name }}
          WANT: ${{ inputs.suites }}
        run: |
          set -euo pipefail
          ALL='["wiring","viewer","coop","shell-hermetic","go-e2e","go-integration"]'
          if [ "$EVENT" = "workflow_dispatch" ] && [ -n "${WANT:-}" ]; then
            want_json=$(printf '%s' "$WANT" | tr ',' ' ' | tr -s ' ' '\n' | sed '/^$/d' | jq -R . | jq -sc .)
            sel=$(jq -cn --argjson all "$ALL" --argjson want "$want_json" \
                  '[$all[] | select(. as $s | $want | index($s))]')
            echo "narrowed dispatch -> $(echo "$want_json" | jq -r 'join(" ")')"
          else
            sel="$ALL"
          fi
          echo "suites=$sel" >> "$GITHUB_OUTPUT"
          echo "selected: $(echo "$sel" | jq -r 'join(" ")')"

  wiring:
    name: wiring (select self-check)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'wiring')
    runs-on: gardenio-k3s-light # light ARC scale set (gardenio/helm/ci) -- adversarially verified safe 2026-09-21, see arc-runners-light-values.yaml
    timeout-minutes: 5
    # Permanent, and ~10 seconds. It is the cheapest possible proof that the
    # triggers, the guard, the output plumbing and the `contains(fromJSON(...))`
    # gate all still work -- which matters most on the dispatch and schedule
    # paths, where nobody is watching.
    steps:
      - name: assert the selection is a non-empty JSON array
        env:
          SUITES: ${{ needs.select.outputs.suites }}
        run: |
          set -euo pipefail
          echo "$SUITES" | jq -e 'type == "array" and length > 0' >/dev/null
          echo "e2e wiring OK on ${{ github.event_name }}: $(echo "$SUITES" | jq -r 'join(" ")')"
```

- [ ] **Step 2: P0-a — add `e2e` to the lost-runner allowlist**

The file's own comment invites this. Replace `.github/workflows/retry-lost-runners.yml:88`:

```yaml
    workflows: [ci, checks, images]
```

with:

```yaml
    workflows: [ci, checks, images, e2e]
```

Takes effect only once on main: `workflow_run` workflows always execute the default-branch copy of the file.

- [ ] **Step 3: Lint the YAML locally**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/e2e.yml')); print('e2e.yml parses')"
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/retry-lost-runners.yml')); print('retry-lost-runners.yml parses')"
```

Expected: both print. Note that `on:` parses as the boolean `True` key in PyYAML — that is a YAML 1.1 quirk, not an error; GitHub reads it correctly.

- [ ] **Step 4: Push and prove the `push` door**

```bash
git checkout -b agent/e2e-ci/<T2 bead id>
git add .github/workflows/e2e.yml .github/workflows/retry-lost-runners.yml
git commit -m "ci: optional e2e workflow skeleton -- select + wiring check"
git push -u origin agent/e2e-ci/<T2 bead id>
gh run list --workflow=e2e.yml --limit 1
gh run view <run id> --log
```

Expected: `select` prints `selected: wiring viewer coop shell-hermetic go-e2e go-integration`; `wiring` prints `e2e wiring OK on push: …`; the four not-yet-existing suite jobs simply do not exist yet, so nothing else runs.

- [ ] **Step 5: Prove the `pull_request` guard is closed by default**

Open the PR, then check that `e2e` did **not** run for it:

```bash
gh pr create --fill
gh run list --workflow=e2e.yml --branch agent/e2e-ci/<T2 bead id> --limit 5
```

Expected: only the push-triggered run. A `pull_request` run either does not appear or appears with `select` skipped. This is the assertion that an unlabelled PR costs nothing.

- [ ] **Step 6: Record both runs on the bead**

Comment the two run URLs and the two expectations above. This is the definition-of-done evidence.

- [ ] **Step 7: After merge — prove the label and dispatch doors (operator or crew, once `e2e` exists)**

```bash
gh label list | grep '^e2e'          # precondition O2
gh pr edit <some open PR> --add-label e2e
gh run list --workflow=e2e.yml --limit 1     # a pull_request run should now exist

gh workflow run e2e.yml -f suites=wiring
gh run list --workflow=e2e.yml --limit 1
gh run view <id> --log | grep 'narrowed dispatch'

gh workflow run e2e.yml -f suites=nosuchsuite
gh run view <id> --log | grep 'selected: $'   # empty selection, visible no-op
```

If the label does not exist yet, record on the bead that Steps 7's label half is deferred to O2 and link the O2 decision bead.

---

### Task 3: `viewer-e2e`

68 Playwright spec files, ~320 tests — the flagship, and the one `checks.yml` already names as unfinished debt, in the `deferred` block's D rows ("beads-viewer e2e … needs node/playwright + a live viewer; port after gardenio is deployed"). It needs no deployed gardenio.

**Files:**
- Modify: `.github/workflows/e2e.yml` (append one job after `wiring`)
- Test: the suite itself — `gardenio/tests/beads-viewer-e2e/` (68 specs, unchanged by this task)

**Interfaces:**
- Consumes: `needs.select.outputs.suites` from T2; the DECIDED recipe block from `docs/ci-e2e-postgres-recipe.md` (T1).
- Produces: the pinned-Node install snippet, reused verbatim by T4; the artifact name `viewer-e2e-report`.

Contract facts this job must satisfy, each verified in the tree:

| Requirement | Source |
|---|---|
| `CREATE EXTENSION IF NOT EXISTS vector` | `kbeads/internal/store/postgres/migrate_preflight.go:44-69` |
| daemon health path is `/v1/health/live`, **not** `/healthz` | `docs/archive/rwx/root/ci.yml:2879-2881` |
| viewer health path **is** `/healthz` | `gardenio/tests/beads-viewer-e2e/playwright.config.js` `webServer.url` |
| `BEADS_VIEWER_BIN` must be set or the config falls back to `go run` inside the test timeout | same file, `webServer.command` |
| `kd` must be on `$PATH` — `seed.mjs` calls `execFileSync('kd', …)` with no override | `gardenio/tests/beads-viewer-e2e/seed/seed.mjs:41-55` |
| `BEADS_OPERATED_BY` required or the first `kd create` returns 400 | `docs/archive/rwx/root/ci.yml:2929-2930` |
| chromium **and** firefox — `popover_constellation.spec.js` is the sole firefox project | `playwright.config.js` `projects` |
| `PLAYWRIGHT_BROWSERS_PATH=0` must match between install and run | `docs/archive/rwx/root/ci.yml:2733`, `:2918` |

- [ ] **Step 1: Append the job**

Add to `.github/workflows/e2e.yml`, after the `wiring` job. Substitute the DECIDED postgres block from `docs/ci-e2e-postgres-recipe.md` where marked; the version below assumes `RECIPE-SERVICES` was decided.

```yaml
  viewer:
    name: viewer-e2e (playwright)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'viewer')
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    timeout-minutes: 25
    # Recipe ported from the deleted RWX pipeline, preserved byte-identical at
    # docs/archive/rwx/root/ci.yml:2757-2947 (`test-beads-viewer-e2e`).
    # Postgres mechanism decided by the T1 spike: docs/ci-e2e-postgres-recipe.md
    services:
      postgres:
        image: pgvector/pgvector:pg16
        env:
          POSTGRES_USER: beads
          POSTGRES_PASSWORD: beads
          POSTGRES_DB: beads
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U beads"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 20
    steps:
      - uses: actions/checkout@v4

      # cache: false -- an Actions cache upload once took 18 minutes over the
      # home uplink and timed out an otherwise green job; see the same comment
      # in ci.yml's `go` job.
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: false

      - name: apt deps
        # xz-utils: the runner image has no xz and the Node tarball is .tar.xz.
        # postgresql-client: for the CREATE EXTENSION below.
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends build-essential xz-utils postgresql-client

      - name: node (pinned tarball, sha256-verified)
        # Same pins the agent image uses (images/agent/install/node.sh), sourced
        # from the same two files, so CI and the image cannot drift apart.
        run: |
          set -euo pipefail
          . gardenio/images/agent/install/versions.env
          . gardenio/images/agent/install/checksums.env
          curl -fsSL "https://nodejs.org/dist/v${NODE_FULL_VERSION}/node-v${NODE_FULL_VERSION}-linux-x64.tar.xz" -o /tmp/node.tar.xz
          echo "${NODE_SHA256_X64}  /tmp/node.tar.xz" | sha256sum -c --status -
          sudo tar -xJ --strip-components=1 -C /usr/local -f /tmp/node.tar.xz
          node --version
          npm --version

      - name: create the vector extension
        # migrate_preflight.go:44-69 queries pg_available_extensions and refuses
        # to migrate without it; 005_bead_embeddings is the consumer.
        env:
          PGPASSWORD: beads
        run: |
          set -euo pipefail
          psql -h localhost -U beads -d beads -v ON_ERROR_STOP=1 \
            -c "CREATE EXTENSION IF NOT EXISTS vector;"
          psql -h localhost -U beads -d beads -tAc \
            "SELECT extversion FROM pg_extension WHERE extname='vector';"

      - name: build kd and put it on PATH
        # seed/seed.mjs shells out to a bare `kd` (seed.mjs:41-55) -- there is
        # no KD_BIN override -- so it must be installed, not just built.
        run: |
          set -euo pipefail
          (cd kbeads && GOWORK=off CGO_ENABLED=0 go build -o /tmp/kd ./cmd/kd/)
          sudo install /tmp/kd /usr/local/bin/kd
          kd --version || true

      - name: build beads-viewer
        # Without BEADS_VIEWER_BIN, playwright.config.js's webServer.command
        # falls back to `go run ./gardenio/controller/cmd/beads-viewer` and
        # compiles inside the 120s webServer timeout.
        run: |
          set -euo pipefail
          GOWORK="$(pwd)/go.work" CGO_ENABLED=0 \
            go build -o /tmp/beads-viewer ./gardenio/controller/cmd/beads-viewer
          test -x /tmp/beads-viewer

      - name: playwright install (chromium + firefox)
        # PLAYWRIGHT_BROWSERS_PATH=0 puts browsers inside node_modules; it must
        # match between install and run or every spec dies at browserType.launch.
        # firefox is not optional: popover_constellation.spec.js is the sole
        # firefox project in playwright.config.js.
        working-directory: gardenio/tests/beads-viewer-e2e
        env:
          PLAYWRIGHT_BROWSERS_PATH: "0"
        run: |
          set -euo pipefail
          npm ci --no-audit --no-fund
          DEBIAN_FRONTEND=noninteractive node_modules/.bin/playwright install-deps chromium firefox
          node_modules/.bin/playwright install chromium firefox
          shell="$(find node_modules/playwright-core/.local-browsers \
            -name chrome-headless-shell -type f 2>/dev/null | head -1 || true)"
          if [ -z "$shell" ] || [ ! -x "$shell" ]; then
            echo "FATAL: playwright install did not place an executable chrome-headless-shell" >&2
            ls -la node_modules/playwright-core/.local-browsers >&2 || true
            exit 1
          fi
          echo "OK: chrome-headless-shell at $shell"

      - name: start kd serve
        # BEADS_HTTP_ADDR is a LISTEN SPEC here (":8080") and a URL later for
        # the seed -- same variable, two meanings. BEADS_NATS_URL must be unset:
        # kd serve fails closed against an unreachable NATS (cmd/kd/serve.go).
        run: |
          set -euo pipefail
          export BEADS_AUTH_TOKEN="e2e-primary-static-token"
          export BEADS_DATABASE_URL="postgres://beads:beads@localhost:5432/beads?sslmode=disable"
          export BEADS_HTTP_ADDR=":8080"
          export BEADS_GRPC_ADDR=":9090"
          export BEADS_METRICS_ADDR=":9091"
          export BEADS_RATE_LIMIT_ENABLED=false
          unset BEADS_NATS_URL || true
          nohup kd serve > /tmp/kd-serve.log 2>&1 &
          echo $! > /tmp/kd.pid
          # The daemon serves /v1/health/live. It has NO /healthz -- the old
          # RWX poll silently ran all 60 iterations against one
          # (docs/archive/rwx/root/ci.yml:2873-2878).
          for i in $(seq 1 60); do
            if curl -sf http://localhost:8080/v1/health/live >/dev/null 2>&1; then
              echo "kd serve live after ${i}s"
              exit 0
            fi
            if ! kill -0 "$(cat /tmp/kd.pid)" 2>/dev/null; then
              echo "kd serve exited early; log:" >&2
              cat /tmp/kd-serve.log >&2
              exit 1
            fi
            sleep 1
          done
          echo "kd serve never became live; log:" >&2
          cat /tmp/kd-serve.log >&2
          exit 1

      - name: playwright test
        # CI=1 makes playwright.config.js set forbidOnly, retries:1, workers:2.
        # BEADS_OPERATED_BY is required: without it seed.mjs's first kd create
        # returns HTTP 400 "operator could not be resolved".
        working-directory: gardenio/tests/beads-viewer-e2e
        env:
          BEADS_HTTP_ADDR: http://localhost:8080
          BEADS_AUTH_TOKEN: e2e-primary-static-token
          BEADS_DAEMON_TOKEN: e2e-primary-static-token
          BEADS_OPERATED_BY: oidc:e2e-fixture@example.com
          BEADS_VIEWER_BIN: /tmp/beads-viewer
          PLAYWRIGHT_BROWSERS_PATH: "0"
          CI: "1"
        run: npm test

      - name: kd serve log
        if: always()
        run: |
          mkdir -p gardenio/tests/beads-viewer-e2e/test-results
          cp -f /tmp/kd-serve.log gardenio/tests/beads-viewer-e2e/test-results/kd-serve.log 2>/dev/null || true
          echo "=== kd-serve.log ==="
          cat /tmp/kd-serve.log 2>/dev/null || true

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: viewer-e2e-report
          path: gardenio/tests/beads-viewer-e2e/test-results/
          retention-days: 7
          if-no-files-found: warn
```

- [ ] **Step 2: Add `viewer` to the `select` vocabulary**

Already present in T2's `ALL` array — verify:

```bash
grep -n "ALL='" .github/workflows/e2e.yml
```

Expected: the array contains `"viewer"`. If a previous task changed it, restore it.

- [ ] **Step 3: Push and run**

```bash
git checkout -b agent/e2e-ci/<T3 bead id>
git add .github/workflows/e2e.yml
git commit -m "ci: viewer-e2e playwright job"
git push -u origin agent/e2e-ci/<T3 bead id>
gh run list --workflow=e2e.yml --limit 1
gh run watch <run id>
```

- [ ] **Step 4: Triage, expecting the seed step to be the surprise**

The likely failures, in descending order of probability:

1. `seed.mjs` fails — check `kd` is on PATH (`which kd` in the failing step's log) and `BEADS_OPERATED_BY` is set.
2. `webServer` timeout — `BEADS_VIEWER_BIN` unset or the binary not executable.
3. postgres unreachable — re-read `docs/ci-e2e-postgres-recipe.md`; if the services path is failing here after passing in the spike, say so on the bead rather than switching mechanisms silently.
4. firefox launch failure — `playwright install-deps` did not install the firefox libs; add `firefox` to the `install-deps` argument list (it is already there above).

Fix, push, re-run. **A `failure` with no failed step and a "lost communication with the server" annotation is a spot reclaim** — `gh run rerun <id> --failed`, never a code change.

- [ ] **Step 5: Green run, then record**

```bash
gh run view <green run id> --json jobs --jq '.jobs[] | {name, conclusion, startedAt, completedAt}'
```

Comment on the bead with the run URL, the job's wall-clock, and the test count from the log (`N passed`). If the wall-clock exceeds the 25-minute timeout, raise `timeout-minutes` in the same PR and say why.

- [ ] **Step 6: Commit and open the PR**

```bash
git commit --amend --no-edit   # or a fixup, as the crew prefers
git push --force-with-lease
gh pr create --title "ci: viewer-e2e playwright job" --body "$(cat <<'EOF'
Ports the beads-viewer Playwright suite (68 specs, ~320 tests) into the
optional e2e workflow, from the RWX recipe preserved at
docs/archive/rwx/root/ci.yml:2757-2947.

Green run: <url>
Closes the `beads-viewer e2e` D-row in checks.yml's comment block.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
```

---

### Task 4: `coop-e2e`

Seven MockCoop `mux-*.spec.ts` plus one pixel-diff `preview-fidelity.spec.ts`. The long pole and the most spot-reclaim-exposed job in the repo.

**Files:**
- Modify: `.github/workflows/e2e.yml` (append one job)

**Interfaces:**
- Consumes: `needs.select.outputs.suites` (T2); the pinned-Node install snippet from T3, pasted verbatim.
- Produces: the artifact name `coop-e2e-report`.

Contract facts, each verified in the tree:

| Requirement | Source |
|---|---|
| `MUX_BIN` defaults to `coop/target/debug/coopmux` — **debug, not release** | `coop/tests/e2e/playwright.config.ts:11-14` |
| **bun is hard-required**, not a preference: the fidelity server shells `bun build` | `coop/tests/e2e/lib/fidelity-server.ts:141-148`, called unconditionally from `start()` at `:52-57` |
| the fidelity server reads `coop/crates/web/node_modules/@xterm/xterm` at request time | `coop/tests/e2e/lib/fidelity-server.ts:35-40`, `:88-104` |
| `coop/crates/web` has only `bun.lock` — install there is `bun install`, not `npm ci` | `coop/crates/web/` listing |
| `run.sh` omits `--with-deps`, so browser system libs are not installed by it | `coop/tests/e2e/run.sh:12`, `:16` |
| `workers: 1` — the specs share one mux instance | `coop/tests/e2e/playwright.config.ts:21` |

- [ ] **Step 1: Append the job**

Add to `.github/workflows/e2e.yml`, after `viewer`:

```yaml
  coop:
    name: coop-e2e (mux + fidelity)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'coop')
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    # ~20 min expected: a cold cargo build with no cache, plus ~8 min of specs
    # (the RWX history). 45 leaves room for a slow node without masking a hang.
    # This is the most reclaim-exposed job in the repo; see retry-lost-runners.yml.
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4

      - name: apt deps
        # Same list as ci.yml's `rust` job -- protoc is for tonic-build in
        # crates/cli/build.rs -- plus unzip (bun ships a .zip) and xz-utils
        # (the Node tarball is .tar.xz).
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends build-essential protobuf-compiler libprotobuf-dev pkg-config libssl-dev tmux unzip xz-utils

      - name: rustup
        # Ephemeral runner pod: nothing is preinstalled. rustup reads
        # coop/rust-toolchain.toml (channel + components) on first use -- which
        # only happens if this step runs INSIDE coop/. ci.yml's `rust` job gets
        # that from a job-level `defaults.run.working-directory: coop`.
        # THIS job cannot use a job-level default, because
        # the apt/node/bun steps source gardenio/images/agent/install/*.env
        # relative to the repo ROOT -- hence the per-step directory here.
        #
        # Do not drop this line. There is no rust-toolchain.toml and no
        # Cargo.toml at the repo root, so with `--default-toolchain none` BOTH
        # halves of the last line fail there: `rustup show active-toolchain`
        # exits 1 ("no active toolchain") and the `rustup toolchain install`
        # fallback exits 1 ("could not choose a version of rustup to run ...
        # no default is configured"), killing the job before it builds
        # anything. Reproduced against rustup 1.29.1.
        working-directory: coop
        run: |
          curl -sSf https://sh.rustup.rs | sh -s -- -y --no-modify-path --profile minimal --default-toolchain none
          echo "$HOME/.cargo/bin" >> "$GITHUB_PATH"
          "$HOME/.cargo/bin/rustup" show active-toolchain || "$HOME/.cargo/bin/rustup" toolchain install

      - name: node (pinned tarball, sha256-verified)
        run: |
          set -euo pipefail
          . gardenio/images/agent/install/versions.env
          . gardenio/images/agent/install/checksums.env
          curl -fsSL "https://nodejs.org/dist/v${NODE_FULL_VERSION}/node-v${NODE_FULL_VERSION}-linux-x64.tar.xz" -o /tmp/node.tar.xz
          echo "${NODE_SHA256_X64}  /tmp/node.tar.xz" | sha256sum -c --status -
          sudo tar -xJ --strip-components=1 -C /usr/local -f /tmp/node.tar.xz
          node --version

      - name: bun (pinned zip, sha256-verified)
        # Hard requirement, not a preference: lib/fidelity-server.ts:141-148
        # calls execSync("bun build ...") from start(), unconditionally.
        # run.sh's "bun if available, else npm" fallback covers the RUNNER only
        # and obscures this. Same pins as images/agent/install/clis.sh.
        run: |
          set -euo pipefail
          . gardenio/images/agent/install/versions.env
          . gardenio/images/agent/install/checksums.env
          curl -fsSL "https://github.com/oven-sh/bun/releases/download/bun-v${BUN_VERSION}/bun-linux-x64.zip" -o /tmp/bun.zip
          echo "${BUN_SHA256_X86_64}  /tmp/bun.zip" | sha256sum -c --status -
          sudo unzip -q -j /tmp/bun.zip -d /usr/local/bin
          sudo chmod +x /usr/local/bin/bun
          sudo ln -sf /usr/local/bin/bun /usr/local/bin/bunx
          bun --version

      - name: build coopmux (debug)
        # DEBUG, not release: playwright.config.ts:11-14 defaults MUX_BIN to
        # ../../target/debug/coopmux and Playwright's webServer launches it.
        working-directory: coop
        run: cargo build -p coopmux

      - name: web deps for the fidelity server
        # fidelity-server.ts serves @xterm/xterm out of crates/web/node_modules
        # at request time (:35-40, :88-104). crates/web has bun.lock only.
        working-directory: coop/crates/web
        run: bun install --frozen-lockfile

      - name: playwright install (chromium)
        # run.sh omits --with-deps, so the system libs must come from here.
        working-directory: coop/tests/e2e
        run: |
          set -euo pipefail
          npm ci --no-audit --no-fund
          DEBIAN_FRONTEND=noninteractive node_modules/.bin/playwright install-deps chromium
          node_modules/.bin/playwright install chromium

      - name: playwright test
        # CI=1 -> retries:1 and the json reporter (playwright.config.ts:20-25).
        working-directory: coop/tests/e2e
        env:
          CI: "1"
        run: node_modules/.bin/playwright test

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: coop-e2e-report
          path: coop/tests/e2e/results/
          retention-days: 7
          if-no-files-found: warn
```

- [ ] **Step 2: Push and run**

```bash
git checkout -b agent/e2e-ci/<T4 bead id>
git add .github/workflows/e2e.yml
git commit -m "ci: coop-e2e mux + fidelity job"
git push -u origin agent/e2e-ci/<T4 bead id>
gh run list --workflow=e2e.yml --limit 1
gh run watch <run id>
```

- [ ] **Step 3: If `preview-fidelity.spec.ts` flakes, quarantine it — do not delete it**

Risk R3 in the spec: the pixel-diff spec compares screenshots at `DEVICE_SCALE_FACTOR: 2` against a 1% budget (`coop/tests/e2e/specs/preview-fidelity.spec.ts:47-50`), and font rendering differs between a laptop and a runner pod. If it fails on pixel diff **only** (not on a bundler or server error), run it three times to establish whether it is deterministic:

```bash
gh run rerun <id> --failed && gh run watch <id>
```

If it fails deterministically with a stable diff percentage, add to the job:

```yaml
        run: node_modules/.bin/playwright test --grep-invert '@fidelity'
```

and tag the spec, in `coop/tests/e2e/specs/preview-fidelity.spec.ts`, changing the describe title to carry `@fidelity`. **Then file a follow-up bead** ("coop fidelity pixel-diff is runner-environment-sensitive; rebaseline or drop the dpr=2 assertion") and link it from this task's bead. Quarantining without the follow-up is exactly the quiet-coverage-loss this plan exists to repair.

- [ ] **Step 4: Green run, then record**

Comment on the bead with the run URL, the wall-clock, whether the fidelity spec ran or was quarantined, and — if quarantined — the follow-up bead id.

- [ ] **Step 5: Commit and open the PR**

```bash
gh pr create --title "ci: coop-e2e mux + fidelity job" --body "$(cat <<'EOF'
Runs coop/tests/e2e (7 MockCoop mux specs + the pixel-diff fidelity spec)
in the optional e2e workflow. bun is a hard requirement of the fidelity
server, not a preference -- pinned and sha256-verified like every other tool.

Green run: <url>

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
```

---

## Phase 2: Shell and Go tiers

### Task 5: `HERMETIC_ONLY` in `run-shard.sh`, the pinned set, and its lint

Fully testable on a laptop with no cluster, so this is the one task in the plan where TDD applies in the normal way. **Write the lint first.**

**Files:**
- Create: `gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh`
- Modify: `gardenio/tests/e2e/scripts/run-shard.sh` — `:29-106` (exclusion block), `:117-129` (shard-var bypass), `:181-243` + `:468-617` (preflight, made conditional), `:764-800` (`run_module` preamble), `:1465-1518` (subset block), `:3306-3330` (failure log dump)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces:
  - `HERMETIC_ONLY=1` — an env var honoured by `run-shard.sh`, entry point for T6.
  - `HERMETIC_MODULES` — a comma-separated shell variable in `run-shard.sh`, the pinned set. The lint reads it by `grep`.
  - `gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh` — exit 0 clean, exit 1 with a per-module reason. T6 does not call it; `checks.yml`'s `script-selftests` job is where it belongs long-term, but wiring it there is out of scope (that file is fimba-91's lane and this plan does not touch it) — instead the lint is added to the `HERMETIC_MODULES` set itself is *not* possible (it is not a `run_module` entry), so **T6's job runs it as an explicit step**.

- [ ] **Step 1: Write the failing lint**

The precedent is `gardenio/tests/e2e/scripts/test-shard-isolation.sh`, which never executes `run-shard.sh` — it lifts pure functions out by `awk` and evaluates them. This lint needs no function lifting at all; it greps the variable and then greps each named module's script.

Create `gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh`:

```bash
#!/usr/bin/env bash
# test-hermetic-set-lint.sh — fail if any module in run-shard.sh's
# HERMETIC_MODULES has acquired a live-cluster dependency.
#
# The hermetic subset exists so `HERMETIC_ONLY=1 run-shard.sh` can run in a
# GitHub Actions runner with no cluster, no beads daemon and no port-forwards.
# That property is not enforced by anything at runtime: a module that starts
# shelling out to kubectl will simply fail, and the natural fix under time
# pressure is to drop it from the set -- which is how coverage disappears
# quietly. This lint makes the regression loud at the point it is introduced.
#
# Hermetic in this context means: no kubectl, no kd, no gb, no helm, no
# terraform, and no reference to the port-forward ports 19090/19091. A module
# MAY build and run Go, start httptest servers, bind 127.0.0.1 and use python3.
#
# Runs in <1s and needs nothing installed. Invoked by the `shell-hermetic` job
# in .github/workflows/e2e.yml before run-shard.sh itself.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN_SHARD="${SCRIPT_DIR}/run-shard.sh"

if [ ! -f "$RUN_SHARD" ]; then
  echo "FAIL: run-shard.sh not found at $RUN_SHARD" >&2
  exit 1
fi

# The forbidden tools, as word-boundary regexes. `go` is deliberately absent.
FORBIDDEN='(^|[^[:alnum:]_-])(kubectl|helm|terraform)([^[:alnum:]_-]|$)|\$\{?(KD_BIN|GB_BIN)\b|(^|[^[:alnum:]_.-])(kd|gb)[[:space:]]|localhost:1909[01]'

# `|| true` is load-bearing, on this and the run_module lookup below. This
# script sets `pipefail`, so a failing grep makes the whole pipeline exit 1,
# and `set -e` then aborts at the ASSIGNMENT -- before the guard that explains
# what went wrong. Without it the missing-variable path below is dead code and
# the lint exits 1 silently, which is the opposite of "make the regression
# loud".
HERMETIC_MODULES="$(
  grep -m1 '^HERMETIC_MODULES=' "$RUN_SHARD" \
    | sed 's/^HERMETIC_MODULES="//; s/"$//' || true
)"

if [ -z "$HERMETIC_MODULES" ]; then
  echo "FAIL: could not read HERMETIC_MODULES from $RUN_SHARD" >&2
  exit 1
fi

rc=0
count=0
IFS=',' read -ra MODULES <<< "$HERMETIC_MODULES"
for mod in "${MODULES[@]}"; do
  mod="$(echo "$mod" | xargs)"
  [ -n "$mod" ] || continue
  count=$((count + 1))

  # Resolve the module name to its script via the registry line in run-shard.sh.
  # `|| true` for the same reason as above: without it a typo'd or renamed
  # module aborts the lint here, so the "has no run_module entry" message
  # never prints AND every module after it in the list goes unchecked.
  script_rel="$(
    grep -m1 "run_module \"${mod}\" " "$RUN_SHARD" \
      | sed 's/.*run_module "[^"]*" "\([^"]*\)".*/\1/' || true
  )"
  if [ -z "$script_rel" ]; then
    echo "FAIL: ${mod} is in HERMETIC_MODULES but has no run_module entry" >&2
    rc=1
    continue
  fi

  script_abs="${SCRIPT_DIR}/../../../../${script_rel#./}"
  if [ ! -f "$script_abs" ]; then
    echo "FAIL: ${mod} -> ${script_rel} does not exist" >&2
    rc=1
    continue
  fi

  # Strip comments and blank lines before matching: a module may legitimately
  # *mention* kubectl in a rationale comment (several do).
  hits="$(sed 's/#.*//' "$script_abs" | grep -nE "$FORBIDDEN" || true)"
  if [ -n "$hits" ]; then
    echo "FAIL: ${mod} (${script_rel}) is no longer hermetic:" >&2
    echo "$hits" | head -5 | sed 's/^/    /' >&2
    rc=1
  fi
done

if [ "$count" -eq 0 ]; then
  echo "FAIL: HERMETIC_MODULES parsed to zero modules" >&2
  exit 1
fi

if [ "$rc" -eq 0 ]; then
  echo "PASS: all ${count} modules in HERMETIC_MODULES are hermetic"
else
  echo "" >&2
  echo "A module in HERMETIC_MODULES acquired a cluster dependency." >&2
  echo "Fix the module, or remove it from HERMETIC_MODULES *and* say so on a" >&2
  echo "bead -- silently shrinking the set is the failure this lint exists" >&2
  echo "to prevent." >&2
fi
exit "$rc"
```

```bash
chmod +x gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
```

- [ ] **Step 2: Run it and watch it fail for the right reason**

```bash
cd ~/ws/gardenio
./gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
```

Expected: `FAIL: could not read HERMETIC_MODULES from …` — the variable does not exist yet. That is the failing test.

- [ ] **Step 3: Derive the hermetic set empirically**

A static categorisation puts the set near 18 (see below). **It is a starting list, not the answer.** Run each candidate directly, with no cluster reachable, and keep only the ones that pass or cleanly skip:

```bash
cd ~/ws/gardenio
unset KUBECONFIG
CANDIDATES="edge-auth-box-auth claudemore-replay-fail-on-miss claudemore-replay-volatile-hash \
idp-audit-fieldcontract grafana-directive pvc-gc-ttl gvisor-ns-sweep gitlab-dark-provenance \
gitlab-webhook-deadman jira-webhook-deadman slackbridge-respawn-phase-metrics slackmock-loop \
grafana-bridge jira-conversational jira-syncback-denybydefault outbox-rules-json \
escalation-auto-reaper session-spend-attribution"
for m in $CANDIDATES; do
  s=$(grep -m1 "run_module \"${m}\" " gardenio/tests/e2e/scripts/run-shard.sh \
      | sed 's/.*run_module "[^"]*" "\([^"]*\)".*/\1/')
  printf '%-40s %s ... ' "$m" "$s"
  if E2E_SKIP_SENTINEL=/tmp/sent-$m bash "$s" >/tmp/out-$m 2>&1; then
    echo "PASS"
  else
    echo "FAIL (see /tmp/out-$m)"
  fi
done
```

Drop anything that fails. Note `idp-audit-fieldcontract` will **loud-skip** without `VICTORIA_LOGS_BIN` (`test-idp-audit-fieldcontract.sh:65-70`) — a clean skip counts as passing; it writes the sentinel and exits 0. Record the pass/skip/fail disposition of every candidate on the bead.

- [ ] **Step 4: Add the subset to `run-shard.sh`**

Insert after `gardenio/tests/e2e/scripts/run-shard.sh:1482` (immediately after `MIGRATION_ACCEPTANCE_MODULES=…`), substituting the empirically-derived list from Step 3:

```bash
# Hermetic subset (gd-<T5 bead id>). Every module here runs to completion with
# NO cluster, NO beads daemon and NO port-forwards: pure `go test` over fake
# clientsets and httptest servers, local probe binaries, and static linters.
# This is what .github/workflows/e2e.yml's `shell-hermetic` job runs, and it is
# the first thing to execute any of these 209 modules in GitHub Actions since
# 8a3eb29e deleted the RWX pipelines on 2026-09-16.
#
# Guarded by test-hermetic-set-lint.sh, which reds if any listed module
# acquires a kubectl/kd/gb/helm dependency. Do not shrink this list to make a
# red go away without saying so on a bead -- quiet shrinkage is precisely the
# failure mode this whole lane exists to fix.
HERMETIC_MODULES="<the derived list, comma-separated, no spaces>"
```

- [ ] **Step 5: Wire the subset selection**

Replace `run-shard.sh:1511-1518` (the `MIGRATION_ACCEPTANCE` block) so it is followed by the new one:

```bash
if [ "$MIGRATION_ACCEPTANCE" = "1" ]; then
  SMOKE_ONLY=1   # reuse the shard-gating bypass in should_run
  apply_subset "MIGRATION_ACCEPTANCE" "$MIGRATION_ACCEPTANCE_MODULES"
fi

# When HERMETIC_ONLY=1, narrow ONLY to the hermetic subset. Like
# MIGRATION_ACCEPTANCE it borrows SMOKE_ONLY's shard-gating bypass rather than
# adding a third one to should_run.
if [ "$HERMETIC_ONLY" = "1" ]; then
  SMOKE_ONLY=1   # reuse the shard-gating bypass in should_run
  apply_subset "HERMETIC_ONLY" "$HERMETIC_MODULES"
fi
```

and add the default alongside the others at `run-shard.sh:1465-1469`:

```bash
ONLY="${ONLY:-}"
SKIP="${SKIP:-}"
SMOKE_ONLY="${SMOKE_ONLY:-}"
MIGRATION_ACCEPTANCE="${MIGRATION_ACCEPTANCE:-}"
HERMETIC_ONLY="${HERMETIC_ONLY:-}"
```

- [ ] **Step 6: Add `HERMETIC_ONLY` to the early shard-var bypass**

This must happen at `:123`, before the `${E2E_SHARD:?…}` guard aborts. Replace `run-shard.sh:117-129`:

```bash
# In SMOKE_ONLY (and MIGRATION_ACCEPTANCE, and HERMETIC_ONLY) mode the runner
# is single-shard by design (one task runs the full named subset), so
# E2E_SHARD/TOTAL_SHARDS default to 0/1 and shard gating is disabled below. In
# normal sharded e2e runs both must be set. Each subset mode must be honored
# HERE (not only in the module-selection block far below) or the
# `${E2E_SHARD:?...}` guard aborts the whole gate before the subset logic runs
# (ka-0000003481 review finding).
if [ "${SMOKE_ONLY:-}" = "1" ] || [ "${MIGRATION_ACCEPTANCE:-}" = "1" ] || [ "${HERMETIC_ONLY:-}" = "1" ]; then
  E2E_SHARD="${E2E_SHARD:-0}"
  TOTAL_SHARDS="${TOTAL_SHARDS:-1}"
else
  E2E_SHARD="${E2E_SHARD:?E2E_SHARD must be set}"
  TOTAL_SHARDS="${TOTAL_SHARDS:-4}"
fi
```

- [ ] **Step 7: Make the cluster preflight conditional**

Five regions abort or hang without a cluster. Wrap each in `if [ "${HERMETIC_ONLY:-}" != "1" ]; then … fi`, keeping every variable the rest of the script reads under `set -u`:

| Region | What it does | Under `HERMETIC_ONLY` |
|---|---|---|
| `:181-208` | kubectl/git context banner (already subshelled with `\|\| true`) | skip — it is pure noise without a cluster and costs seconds of timeouts |
| `:210-243` | `kubectl wait --for=condition=Available` on the beads and agents deployments | skip |
| `:468-513` | beads port-forward spawn + 30×1s `/v1/health` poll | skip; still set `PF_SUPERVISED=0` and `PF_PID=""` |
| `:515-559` | slack-bridge port-forward | skip; still set `BRIDGE_AVAILABLE=0`, `BRIDGE_PF_PID=""` and `export BRIDGE_HTTP_URL=""` |
| `:561-569` | `BEADS_TOKEN` load and its `:?` hard guard | skip; set `export BEADS_TOKEN=""` so the `${#BEADS_TOKEN}` read at `:585` does not trip `set -u` |
| `:584-617` | `kd`/`gb` banner and the 5× `kd list` connectivity gate ending in `exit 1` | skip |
| `:3306-3330` | on-failure `kubectl logs` dump of staging pods | skip |

The variables at `:619-632` (`GB_BIN`, `KD_BIN`, `KD_ACTOR`, `NAMESPACE`, `BEADS_SVC`, `JUNIT_DIR`, `RESULTS_JSONL`, the three counters) are **kept unconditionally** — the JUnit and JSONL reporting is the point of using the driver at all.

Print one banner where the preflight would have been, so a hermetic run is self-describing:

```bash
if [ "${HERMETIC_ONLY:-}" = "1" ]; then
  echo ""
  echo "HERMETIC_ONLY=1: skipping cluster preflight (deployment waits, port-forwards,"
  echo "  token load, kd connectivity gate). Modules run against local processes only."
  PF_SUPERVISED=0
  PF_PID=""
  BRIDGE_PF_PID=""
  BRIDGE_AVAILABLE=0
  export BRIDGE_HTTP_URL=""
  export BEADS_TOKEN=""
fi
```

- [ ] **Step 8: Make `run_module`'s per-module preamble a no-op**

This is the half that a reader of the module scripts alone would miss: **the harness is not hermetic even when the module is.** `run_module` calls `ensure_port_forward` and `ensure_bridge_port_forward` and curls `localhost:19090` and `:19091` before *every* module (`run-shard.sh:764-800`), plus a post-failure connectivity probe at `:885-894` that calls `kd list`.

Only two edits are needed. The bridge half at `:786-801` is already inert, because it is wrapped in `if [ "$BRIDGE_AVAILABLE" = 1 ]` and Step 7 sets `BRIDGE_AVAILABLE=0` — `_bpf_ok` stays at its declared `1`.

First, replace `run-shard.sh:764-783` — the beads port-forward acquisition and its curl loop:

```bash
run_module() {
  local name="$1"
  shift
  ensure_port_forward
  ensure_bridge_port_forward
  local _pf_ok=0
  for _pf_i in 1 2 3; do
    if curl -sf --max-time 2 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
      _pf_ok=1
      break
    fi
    echo "Tunnel unreachable before $name (attempt $_pf_i/3), restarting..."
    ensure_port_forward
  done
  if [ "$_pf_ok" = 0 ]; then
    echo "WARNING: tunnel still unreachable after 3 attempts, running $name anyway"
  fi
```

with:

```bash
run_module() {
  local name="$1"
  shift
  # The harness is not hermetic even when the module is: without this guard a
  # hermetic module still pays two port-forward acquisitions and a 3x2s curl
  # of a tunnel that will never exist. _pf_ok=1 also disables the
  # environmental-retry gate below, which is correct -- there is no
  # environment to flap.
  local _pf_ok=1
  if [ "${HERMETIC_ONLY:-}" != "1" ]; then
    ensure_port_forward
    ensure_bridge_port_forward
    _pf_ok=0
    for _pf_i in 1 2 3; do
      if curl -sf --max-time 2 -o /dev/null "http://localhost:19090/v1/health" 2>/dev/null; then
        _pf_ok=1
        break
      fi
      echo "Tunnel unreachable before $name (attempt $_pf_i/3), restarting..."
      ensure_port_forward
    done
    if [ "$_pf_ok" = 0 ]; then
      echo "WARNING: tunnel still unreachable after 3 attempts, running $name anyway"
    fi
  fi
```

Second, guard the post-mortem connectivity probe. Replace `run-shard.sh:885-886`:

```bash
  local _post_conn_ok=1
  if [ "$rc" -ne 0 ]; then
```

with:

```bash
  local _post_conn_ok=1
  if [ "${HERMETIC_ONLY:-}" != "1" ] && [ "$rc" -ne 0 ]; then
```

Without this, a failing hermetic module spends 3s curling a dead tunnel and then runs `kd list` against nothing — and, worse, `_post_conn_ok=0` would classify a genuine assertion failure as environmental and retry it.

With `_pf_ok=1`, `_bpf_ok=1` and `_post_conn_ok=1`, both retry gates (`:908`, `:916`) evaluate false and the whole environmental-retry apparatus is inert.

- [ ] **Step 9: Add the lint to the exclusion block**

`run-shard.sh:29-106` carries a machine-parsed list between `[shard-isolation-lint: exclusion-list BEGIN]` and `END`. `test-shard-isolation.sh`'s FORWARD/REVERSE completeness check reds if a `test-*.sh` in that directory is neither wired into a shard nor listed. Add, in the exact bullet form the block uses:

```
#   - test-hermetic-set-lint.sh  (static lint over HERMETIC_MODULES; runs
#     standalone from the shell-hermetic job, never inside a shard)
```

- [ ] **Step 10: Run the lint — it should now pass**

```bash
./gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
```

Expected: `PASS: all N modules in HERMETIC_MODULES are hermetic`.

- [ ] **Step 11: Run the driver end to end with no cluster**

```bash
cd ~/ws/gardenio
unset KUBECONFIG
HERMETIC_ONLY=1 ./gardenio/tests/e2e/scripts/run-shard.sh; echo "exit=$?"
cat junit-results-0/e2e-results.jsonl
python3 -c "import xml.dom.minidom,sys; print(xml.dom.minidom.parse('junit-results-0/e2e-summary.xml').toprettyxml()[:800])"
```

Expected: `exit=0`; one JSONL row per module with `"status":"pass"` or `"skip"`; a well-formed `<testsuites>` with `tests`, `failures` and `skipped` counts matching. **If any row says `fail`, the module is not hermetic — remove it from `HERMETIC_MODULES` and record why on the bead.**

- [ ] **Step 12: Prove the other two subsets still work**

The `SMOKE_ONLY` and `MIGRATION_ACCEPTANCE` paths share the machinery this task edited. They cannot run without a cluster, but their *selection* logic can be checked:

```bash
SMOKE_ONLY=1 ONLY=nosuchmodule bash -n gardenio/tests/e2e/scripts/run-shard.sh && echo "syntax OK"
shellcheck -x gardenio/tests/e2e/scripts/run-shard.sh | head -20
shellcheck -x gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
bash gardenio/tests/e2e/scripts/test-shard-isolation.sh
bash gardenio/tests/e2e/scripts/test-skip-visibility-lint.sh
bash gardenio/tests/e2e/scripts/test-shard-budget-guard.sh
```

Expected: `syntax OK`, no *new* shellcheck findings relative to `git stash`-ing the change, and all three existing driver lints pass. `test-shard-isolation.sh` in particular proves Step 9's exclusion-list edit is well-formed.

- [ ] **Step 13: Commit**

```bash
git add gardenio/tests/e2e/scripts/run-shard.sh gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh
git commit -m "$(cat <<'EOF'
e2e: HERMETIC_ONLY subset in run-shard.sh, plus a lint that keeps it hermetic

A third curated subset next to SMOKE_ONLY and MIGRATION_ACCEPTANCE, so the
modules that need no cluster can run in a GitHub Actions runner. Two halves:
the subset itself, and making the cluster preflight -- including run_module's
per-module port-forward/health preamble -- conditional.

test-hermetic-set-lint.sh reds if a listed module acquires a kubectl/kd/gb
dependency, because silently shrinking the set is how this coverage
disappeared the first time.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
```

---

### Task 6: `shell-hermetic` job

**Files:**
- Modify: `.github/workflows/e2e.yml` (append one job)

**Interfaces:**
- Consumes: `HERMETIC_ONLY=1` and `gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh` (T5); `needs.select.outputs.suites` (T2).
- Produces: the artifact name `shell-hermetic-junit`.

- [ ] **Step 1: Append the job**

Add to `.github/workflows/e2e.yml`, after `coop`:

```yaml
  shell-hermetic:
    name: shell-hermetic (run-shard.sh subset)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'shell-hermetic')
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    timeout-minutes: 20
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: false

      - name: apt deps
        # The modules build and run Go probes; several shell out to python3.
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends build-essential python3

      - name: lint the hermetic set
        # Runs first and takes <1s. If a module in HERMETIC_MODULES has
        # acquired a cluster dependency, fail here with the module named rather
        # than 15 minutes later with an opaque module failure.
        run: ./gardenio/tests/e2e/scripts/test-hermetic-set-lint.sh

      - name: build gb and kd
        # No module in the hermetic set calls them (the lint enforces that), but
        # the driver's own banner and several modules' preludes resolve them,
        # and a missing binary turns a clean run into a confusing one.
        run: |
          set -euo pipefail
          (cd gardenio/controller && CGO_ENABLED=0 go build -o /tmp/gb ./cmd/gb/)
          (cd kbeads && GOWORK=off CGO_ENABLED=0 go build -o /tmp/kd ./cmd/kd/)
          sudo install /tmp/gb /usr/local/bin/gb
          sudo install /tmp/kd /usr/local/bin/kd

      - name: run the hermetic shard
        # cwd MUST be the repo root: run-shard.sh's registry entries are
        # repo-relative (./gardenio/tests/e2e/scripts/test-*.sh).
        env:
          HERMETIC_ONLY: "1"
        run: ./gardenio/tests/e2e/scripts/run-shard.sh

      - name: summarise
        if: always()
        run: |
          set -euo pipefail
          if [ -f junit-results-0/e2e-results.jsonl ]; then
            echo "### shell-hermetic results" >> "$GITHUB_STEP_SUMMARY"
            echo '```' >> "$GITHUB_STEP_SUMMARY"
            jq -r '"\(.status)\t\(.duration_s)s\t\(.name)\(if .skip_reason then "  -- " + .skip_reason else "" end)"' \
              junit-results-0/e2e-results.jsonl >> "$GITHUB_STEP_SUMMARY"
            echo '```' >> "$GITHUB_STEP_SUMMARY"
          fi

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: shell-hermetic-junit
          path: junit-results-0/
          retention-days: 7
          if-no-files-found: warn
```

- [ ] **Step 2: Push and run**

```bash
git checkout -b agent/e2e-ci/<T6 bead id>
git add .github/workflows/e2e.yml
git commit -m "ci: shell-hermetic job running run-shard.sh's HERMETIC_ONLY subset"
git push -u origin agent/e2e-ci/<T6 bead id>
gh run list --workflow=e2e.yml --limit 1
gh run watch <run id>
```

- [ ] **Step 3: Check the skip count against the local run**

The runner and the laptop should agree on which modules skip. If a module skipped in CI but passed locally, something on the runner is missing (most likely a Go build dependency); say so on the bead rather than accepting a quieter run.

```bash
gh run view <id> --log | grep 'MODULE SKIPPED\|MODULE PASSED\|MODULE FAILED'
```

Expected: identical dispositions to Step 11 of T5, except `idp-audit-fieldcontract`, which skips in both places.

- [ ] **Step 4: Green run, record, open the PR**

Comment on the bead with the run URL, the pass/skip counts, and the job wall-clock.

---

### Task 7: `go-e2e`

**Files:**
- Modify: `.github/workflows/e2e.yml` (append one job)
- Test: the 7 `//go:build e2e` files themselves — **no Go source changes**

**Interfaces:**
- Consumes: `needs.select.outputs.suites` (T2).
- Produces: nothing other tasks import.

The seven tagged files and their disposition with no cluster, no daemon and
`gb` on PATH. **Measured on 2026-09-21 by running the tier**, not inferred from
reading the source:

| File | Gate | Behaviour |
|---|---|---|
| `cmd/controller/stranded_recovered_livecluster_e2e_test.go` | live cluster | **skips cleanly** (2 tests) |
| `internal/auditxform/auditxform_e2e_test.go` | `VICTORIA_LOGS_BIN` or a binary on PATH | **skips cleanly** when absent (2 tests); `t.Fatalf` only if the var is set but unstat-able |
| `internal/bridge/gardener_start_argv_emit_e2e_test.go` | `GARDENER_EMIT_BEAD_ID` | **skips cleanly** (1 test) |
| `internal/bridge/subagentstop_e2e_test.go` | `gb` on PATH + embedded NATS | **runs and passes** once `gb` is installed; NATS is embedded and costs nothing |
| `internal/claudemore/hooks_natsrelay_e2e_test.go` | `gb` on PATH + embedded NATS | **runs and passes** |
| `internal/bridge/thread_task_autoclose_livedaemon_e2e_test.go` | live daemon | **skips cleanly** (`liveE2EClient`) |
| `internal/bridge/trigger_changed_to_seam_livedaemon_e2e_test.go` | live daemon | **skips cleanly** — `liveE2EClient` runs first and ends the test |

Whole tier, scoped as in Step 1: **8154 pass, 11 skip, 0 fail, exit 0, ~82s**
(`internal/bridge` is 79s of it). No source edit required to get there.

**An earlier draft of this task claimed the last file HARD-FAILS with no
daemon, and spent two steps adding a skip guard to `liveBeadSourceOverSSE`.
That was wrong.** The test body is `client := liveE2EClient(t)` at `:314`
followed immediately by `liveBeadSourceOverSSE(t, client)` at `:315`, and
`t.Skip` calls `runtime.Goexit` — so with the daemon env unset the second
helper is never reached and its unguarded 60s `t.Fatal` at `:178` is
unreachable. Verified by running exactly the command the old step gave:
`--- SKIP ... (0.00s)`. The guard was unnecessary and its TDD red step could
never go red. **This task edits no Go source.** If you believe you have found
a hard-fail, run it before writing a guard.

So no file needs a guard — and if one ever does, the fix is a guard, **never**
excluding the package (spec §4.4, R6). Scoping the `go test` invocation to the
packages that hold tagged files, as Step 1 does, is not "excluding a package":
it drops only packages with zero tagged files, and the guard in the job
asserts that set has not changed.

- [ ] **Step 1: Run the whole tagged tier locally**

All seven tagged files live in four controller packages, and **`kbeads` has
none** — verify that before you trust it:

```bash
cd ~/ws/gardenio
grep -rln '^//go:build.*\be2e\b' --include='*.go' \
  --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.worktrees --exclude-dir=archive . \
  | xargs -r -n1 dirname | sed 's|^\./||' | sort -u
```

Expected, exactly:

```
gardenio/controller/cmd/controller
gardenio/controller/internal/auditxform
gardenio/controller/internal/bridge
gardenio/controller/internal/claudemore
```

kbeads' twelve `*_e2e_test.go` files carry **no build constraint** — they are
in-process `httptest` tests that already run in `ci.yml`'s `go` job on every
push. `-tags e2e` neither adds nor removes anything there, which is why this
task's job has no kbeads leg. (§4.4 of the spec says "across controller and
kbeads"; that half of the sentence is wrong about the tree, and this is the
correction.)

Now run the tier — **the package list, not `./...`**:

```bash
cd ~/ws/gardenio/gardenio/controller && go build -o /tmp/gb ./cmd/gb/ && sudo install /tmp/gb /usr/local/bin/gb
cd ~/ws/gardenio/gardenio/controller && go test -tags e2e -count=1 \
  ./cmd/controller/... ./internal/auditxform/... ./internal/bridge/... ./internal/claudemore/... 2>&1 | tail -40
```

Expected, measured 2026-09-21: `ok` for all four packages, **8154 pass / 11
skip / 0 fail**, exit 0, ~82s (`internal/bridge` is 79s of that). Anything that
does fail gets a skip guard of its own in this same task — add it here, do not
carry it forward, and **never** drop the package from the list.

**Do not "simplify" that to `./...`.** A Go build tag is ADDITIVE: `-tags e2e`
admits the constrained files, it does not narrow what `./...` expands to. So
`-tags e2e ./...` runs all 1697 controller test files — a duplicate of
`ci.yml`'s `go` job on the same capacity-capped heavy pool — and it picks up
`cmd/gb/real_catalog_budget_test.go` (plain `package main`, no build
constraint), whose `TestRealCatalogSnapshotFreshness` hard-fails once the
pinned advice-catalog snapshot passes `hardSnapshotAge = 14d`. That fixture is
dated 2026-09-01, so it is red today; the `test` step of `ci.yml`'s `go` job carries
`-skip 'TestRealCatalogSnapshotFreshness'` for exactly this reason and
`.github/known-failures.md` records it as pre-existing. Scoping avoids it
outright. It is also what keeps the skip census in Step 2 meaningful: a
module-wide run buries the seven tagged files' skips under every unrelated
skip in the module. The same fix was applied to Task 8's `go-integration` in
commit `52ed7a63`.

- [ ] **Step 2: Append the job**

Add to `.github/workflows/e2e.yml`, after `shell-hermetic`:

```yaml
  go-e2e:
    name: go-e2e (-tags e2e, controller)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'go-e2e')
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    timeout-minutes: 20
    # No matrix: all seven //go:build e2e files are in gardenio/controller.
    # kbeads has none -- its *_e2e_test.go files are untagged httptest tests
    # that ci.yml already runs -- so a kbeads leg here would re-run 506 test
    # files, with cgo and libicu, to assert nothing. The `tagged set` guard
    # below is what catches the day that stops being true.
    defaults:
      run:
        working-directory: gardenio/controller
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: false

      - name: apt deps
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends build-essential

      - name: tagged set has not drifted
        # The test step runs an explicit PACKAGE LIST, not ./..., because
        # `-tags e2e` is ADDITIVE -- it admits the constrained files, it does
        # not narrow what ./... expands to (see Step 1). A hand-written list
        # can go stale silently, and silent coverage loss is the exact failure
        # this whole plan is repairing, so assert it instead.
        working-directory: .
        run: |
          set -euo pipefail
          expected='gardenio/controller/cmd/controller
          gardenio/controller/internal/auditxform
          gardenio/controller/internal/bridge
          gardenio/controller/internal/claudemore'
          actual="$(grep -rln '^//go:build.*\be2e\b' --include='*.go' \
            --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.worktrees --exclude-dir=archive . \
            | xargs -r -n1 dirname | sed 's|^\./||' | sort -u)"
          if [ "$(echo "$expected" | sed 's/^ *//')" != "$actual" ]; then
            echo "::error::the set of packages holding //go:build e2e files has changed" >&2
            diff <(echo "$expected" | sed 's/^ *//') <(echo "$actual") >&2 || true
            echo "Update BOTH this guard and the package list in the test step below." >&2
            exit 1
          fi

      - name: build gb and put it on PATH
        # subagentstop_e2e_test.go and hooks_natsrelay_e2e_test.go LookPath
        # $GB_BIN (default "gb") and skip without it. They embed their own NATS
        # server, so once gb exists they run for free -- and a silent skip here
        # would be two of the five runnable files quietly doing nothing.
        run: |
          set -euo pipefail
          CGO_ENABLED=0 go build -o /tmp/gb ./cmd/gb/
          sudo install /tmp/gb /usr/local/bin/gb

      - name: go test -tags e2e
        # The other tagged files gate on a live cluster, a live daemon,
        # VICTORIA_LOGS_BIN or GARDENER_EMIT_BEAD_ID and skip LOUDLY without
        # them. -v so those skips are visible: a silent skip is how this
        # coverage disappeared in the first place.
        #
        # The package list is the four packages that hold the seven tagged
        # files, asserted by the guard above. NOT ./... -- see Step 1.
        # Tee the log so the census below reads it instead of running the
        # whole tier a second time.
        run: |
          set -o pipefail
          go test -tags e2e -v -count=1 \
            ./cmd/controller/... ./internal/auditxform/... ./internal/bridge/... ./internal/claudemore/... \
            2>&1 | tee /tmp/go-e2e-controller.log

      - name: skip census
        if: always()
        run: |
          set -euo pipefail
          echo "### go-e2e skips (controller)" >> "$GITHUB_STEP_SUMMARY"
          echo '```' >> "$GITHUB_STEP_SUMMARY"
          grep -E '^\s*---\s+SKIP' /tmp/go-e2e-controller.log >> "$GITHUB_STEP_SUMMARY" || echo "(none)" >> "$GITHUB_STEP_SUMMARY"
          echo '```' >> "$GITHUB_STEP_SUMMARY"
```

- [ ] **Step 3: Verify the `gb` build works from a bare checkout**

```bash
cd ~/ws/gardenio/gardenio/controller && CGO_ENABLED=0 go build -o /tmp/gb ./cmd/gb/ && echo OK
```

Expected: `OK`. (The `-ldflags` in `gardenio/Makefile:22` inject version metadata; the tests do not read it, so the plain build is correct here.)

- [ ] **Step 4: Push, run, record**

```bash
git checkout -b agent/e2e-ci/<T7 bead id>
git add .github/workflows/e2e.yml
git commit -m "ci: go-e2e job (-tags e2e) over the four packages that hold tagged files"
git push -u origin agent/e2e-ci/<T7 bead id>
gh run watch <run id>
```

On the bead, record the skip census from the job summary — **which of the seven ran and which skipped, by name.** That census is the honest statement of what this job proves, and T11 quotes it in the user guide.

---

### Task 8: `go-integration`

25 files that have never executed in any CI. **Expect red.** The job lands `continue-on-error: true` with an entry in `.github/known-failures.md`; T10 triages it and flips it to blocking or records what remains.

**Files:**
- Modify: `.github/workflows/e2e.yml` (append one job)
- Modify: `.github/known-failures.md`

**Interfaces:**
- Consumes: `needs.select.outputs.suites` (T2); the DECIDED recipe from `docs/ci-e2e-postgres-recipe.md` (T1), reaching the same DSN.
- Produces: the artifact name `go-integration-log`; a `known-failures.md` section T10 edits.

The tier, as read in the tree: 25 files carry `//go:build integration` on line 1. Twenty-four are `kbeads/internal/store/postgres` and gate on `BEADS_TEST_DATABASE_URL` with the identical four-line skip (there is no shared helper; the same block is repeated 29 times). The odd one is `gardenio/controller/cmd/edge-auth/pat_integration_test.go:62-77`, which gates on `GITLAB_TEST_URL` / `GITLAB_TEST_GROUP` and will skip — correctly, and permanently: it needs a real GitLab.

- [ ] **Step 1: Run it locally first, to know what "expect red" actually means**

```bash
docker run -d --name pgv -e POSTGRES_USER=beads -e POSTGRES_PASSWORD=beads \
  -e POSTGRES_DB=beads -p 5432:5432 pgvector/pgvector:pg16
sleep 10
PGPASSWORD=beads psql -h localhost -U beads -d beads -c 'CREATE EXTENSION IF NOT EXISTS vector;'
cd ~/ws/gardenio/kbeads
BEADS_TEST_DATABASE_URL='postgres://beads:beads@localhost:5432/beads?sslmode=disable' \
  go test -tags integration ./... -count=1 2>&1 | tee /tmp/go-integration-local.log | tail -60
docker rm -f pgv
```

If rootless docker is not running on this machine, skip to Step 2 and let the first CI run be the measurement — but say on the bead which route was taken. Record the pass/fail counts either way.

- [ ] **Step 2: Append the job**

Add to `.github/workflows/e2e.yml`, after `go-e2e`. Substitute the DECIDED postgres block from `docs/ci-e2e-postgres-recipe.md`.

```yaml
  go-integration:
    name: go-integration (-tags integration)
    needs: select
    if: contains(fromJSON(needs.select.outputs.suites), 'go-integration')
    runs-on: gardenio-k3s # ARC scale set on the k3s CI pool (gardenio/helm/ci)
    timeout-minutes: 30
    # continue-on-error: these 25 files have never executed in ANY CI -- not
    # GitHub Actions, not the deleted RWX pipeline. Landing red-tolerant
    # captures the coverage immediately instead of letting unknown breakage
    # stall the phase. Triaged and flipped to blocking by the T10 task of the
    # e2e-ci plan; the disposition lives in .github/known-failures.md.
    continue-on-error: true
    services:
      postgres:
        image: pgvector/pgvector:pg16
        env:
          POSTGRES_USER: beads
          POSTGRES_PASSWORD: beads
          POSTGRES_DB: beads
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U beads"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 20
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}
          cache: false

      - name: apt deps
        # libicu-dev + cgo: the kbeads *test* suite needs it for unicode
        # normalization (ci.yml's matrix says the same).
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends build-essential libicu-dev postgresql-client

      - name: create the vector extension
        env:
          PGPASSWORD: beads
        run: |
          set -euo pipefail
          psql -h localhost -U beads -d beads -v ON_ERROR_STOP=1 \
            -c "CREATE EXTENSION IF NOT EXISTS vector;"

      - name: go test -tags integration (kbeads)
        # Scoped to ./internal/store/postgres/... on purpose. `-tags integration`
        # is ADDITIVE -- it lets the constrained files into the build, it does
        # NOT narrow what `./...` expands to. `-tags integration ./...` would run
        # all 21 kbeads test packages (506 _test.go files), not the 24 tagged
        # ones this job exists for. Do not "simplify" this back to ./...
        working-directory: kbeads
        env:
          BEADS_TEST_DATABASE_URL: postgres://beads:beads@localhost:5432/beads?sslmode=disable
        run: |
          set -o pipefail
          go test -tags integration -v ./internal/store/postgres/... -count=1 2>&1 | tee /tmp/go-integration-kbeads.log

      - name: go test -tags integration (controller)
        # One file: cmd/edge-auth/pat_integration_test.go, gated on
        # GITLAB_TEST_URL/GITLAB_TEST_GROUP. It skips, and skipping is correct
        # -- it needs a real GitLab instance. Run it anyway so the skip is
        # visible rather than assumed.
        #
        # Scoped to ./cmd/edge-auth/... for the same additive-tag reason as the
        # kbeads step above, and for a second one: a module-wide run pulls in
        # cmd/gb/real_catalog_budget_test.go (plain `package main`, no build
        # constraint), whose TestRealCatalogSnapshotFreshness HARD-fails once the
        # pinned advice-catalog snapshot passes hardSnapshotAge = 14d. It is
        # already past that, which is why the `test` step of ci.yml's `go` job
        # runs `-skip 'TestRealCatalogSnapshotFreshness'`. Scoping avoids it;
        # if you ever do widen this step, carry that -skip across.
        working-directory: gardenio/controller
        run: |
          set -o pipefail
          go test -tags integration -v ./cmd/edge-auth/... -count=1 2>&1 | tee /tmp/go-integration-controller.log

      - name: failure census
        if: always()
        run: |
          set -euo pipefail
          echo "### go-integration" >> "$GITHUB_STEP_SUMMARY"
          echo '```' >> "$GITHUB_STEP_SUMMARY"
          grep -hE '^\s*---\s+(FAIL|SKIP)' /tmp/go-integration-*.log >> "$GITHUB_STEP_SUMMARY" || echo "(no failures or skips)" >> "$GITHUB_STEP_SUMMARY"
          echo '```' >> "$GITHUB_STEP_SUMMARY"

      - uses: actions/upload-artifact@v4
        if: always()
        with:
          name: go-integration-log
          path: /tmp/go-integration-*.log
          retention-days: 14
          if-no-files-found: warn
```

- [ ] **Step 3: Add the known-failures entry**

`.github/known-failures.md` is the repo's home for this kind of debt. Append a new `##` section after the existing `## Tests skipped in \`ci.yml\`` section, matching its heading style (`###` per entry, a prose cause paragraph, a bold `**Fix:**` line):

```markdown
## `continue-on-error` in `e2e.yml`

### `go-integration`
The 25 `//go:build integration` files have never executed in any CI — not in
GitHub Actions, and not in the deleted RWX pipeline either. Twenty-four are
`kbeads/internal/store/postgres` real-Postgres tests gated on
`BEADS_TEST_DATABASE_URL`; one is `gardenio/controller/cmd/edge-auth/pat_integration_test.go`,
which gates on `GITLAB_TEST_URL`/`GITLAB_TEST_GROUP` and skips permanently (it
needs a live GitLab and always will).

The job is `continue-on-error: true` because landing it red-tolerant captures
the coverage now instead of letting unknown breakage stall the phase that
delivers it. That is a deliberate, time-boxed state, not a resting one.

**Fix:** triage the red set and either fix it or record each remaining failure
with a reason, then drop `continue-on-error`. Owned by the T10 task of the
e2e-ci plan (`plan:e2e-ci`).

Failure census as of the first green-ish run: <N passed, M failed, K skipped —
fill in from the run linked on the bead>.
```

- [ ] **Step 4: Push, run, fill in the census**

```bash
git checkout -b agent/e2e-ci/<T8 bead id>
git add .github/workflows/e2e.yml .github/known-failures.md
git commit -m "ci: go-integration job (red-tolerant) + known-failures entry"
git push -u origin agent/e2e-ci/<T8 bead id>
gh run watch <run id>
gh run view <run id> --log | grep -E '^\s*--- (FAIL|SKIP|PASS)' | sort | uniq -c
```

Substitute the real counts into the `known-failures.md` census line and amend.

- [ ] **Step 5: Record and open the PR**

Definition of done here is **a completed run**, not a green one: the job is `continue-on-error`. Comment on the bead with the run URL, the exact `N passed / M failed / K skipped`, and the artifact link. T10 starts from that census.

---

## Phase 3: Make it stick

### Task 9: Enable the nightly and measure the cost

**UNBLOCKED as of 2026-09-22.** Both gates have cleared: `gd-IoVkE0k8ot` (the lost-runner retry demonstration) is closed, `gd-X3zmgmE3nD` (T0, the schedule exemption) landed as `d5edca64`, and O3 is ruled — **09:17 UTC**. The original reasoning still holds and is why those gates existed: Phases 1–2 ran on watched branch pushes where a human could rerun by hand; a nightly runs with nobody watching, so the retry path had to be real first.

One caveat inherited from T0, worth knowing before you watch the first night. The `schedule` exemption has **never executed** — no scheduled run has ever existed in this repo (`event=schedule` returns `total_count: 0` against 1806 runs), so T0's positive path was unverifiable by construction and this task is what verifies it. If the first reclaimed nightly is still dropped as "superseded", that is T0's branch failing, not this cron.

**Files:**
- Modify: `.github/workflows/e2e.yml` (`on:` block only — uncomment `schedule:`)

**Interfaces:**
- Consumes: the commented `schedule:` block left by T2.
- Produces: a measurement comment on the bead; possibly a `max-parallel` recommendation, but **only if starvation is observed**.

- [ ] **Step 1: Confirm the preconditions**

```bash
gh workflow view e2e.yml                      # exists on main
gh run list --workflow=e2e.yml --limit 10     # the five suite jobs have each been green at least once
```

Check `gd-IoVkE0k8ot`'s third acceptance criterion has a positive demonstration (a run that reached a `RETRY` decision), not only the negative half. If it does not, say so on this bead and on `gd-IoVkE0k8ot` rather than proceeding quietly.

- [ ] **Step 2: Replace the commented block with the chosen cron**

Replace, in `.github/workflows/e2e.yml`:

```yaml
  # The nightly is added by the T9 task of the e2e-ci plan, once the operator
  # has chosen the hour (decision O3). It is deliberately absent until then:
  # landing an un-timed cron against a shared 6-slot spot pool is how you
  # starve `ci`.
  # schedule:
  #   - cron: '<chosen hour> * * *'
```

with the literal text below. This is not an example: O3 is **ruled** (operator
delegated to laptop-claude, 2026-09-22) and the answer is **09:17 UTC**. Note
the placeholder above cannot simply be uncommented — `'<chosen hour> * * *'`
is not a valid cron expression, it is a five-field slot with the first two
fields missing.

```yaml
  # The anti-rot mechanism. A dispatch-only job is green the day it lands and
  # untested three weeks later, which is exactly how this suite rotted the
  # first time. Hour chosen by operator decision O3 (capacity-shaped: the pool
  # is shared with ci/checks/images and a full nightly can take 5 of 6 slots).
  # An odd minute, because on-the-hour crons queue behind every other repo's.
  schedule:
    - cron: '17 9 * * *'
```

- [ ] **Step 3: Land it and watch the first three nights**

After merge, on each of three consecutive mornings:

```bash
gh run list --workflow=e2e.yml --event schedule --limit 5 \
  --json databaseId,conclusion,attempt,createdAt,updatedAt
gh run view <id> --json jobs \
  --jq '.jobs[] | {name, conclusion, startedAt, completedAt}'

# Per-JOB attempt numbers are not in `gh run view --json jobs` -- that object
# carries only completedAt/conclusion/databaseId/name/startedAt/status/steps/url
# (verified 2026-09-22). Use the REST route, and `filter=all` or you get only
# the latest attempt's jobs and the retry becomes invisible:
gh api "repos/$(gh repo view --json nameWithOwner --jq .nameWithOwner)/actions/runs/<id>/jobs?filter=all&per_page=100" \
  --jq '.jobs[] | {name, conclusion, run_attempt}'
```

Record per night: total wall-clock, per-job wall-clock, each job's conclusion, **and each job's attempt number**.

**Four conclusions do not mean what they look like on this fleet, and reading any of them naively will put a wrong number in your cost report.**

- **`cancelled` on a `checks.yml` run on main is normal.** Since `a8ac1ba3` the `security` job carries its own job-level concurrency group, and when a newer push supersedes it GitHub marks that job `cancelled` and rolls the whole *run* up to `cancelled` even though every sibling succeeded or skipped. It means the supersession guard fired, not that a runner died. (Confirmed live on run `35682726884`.)
- **`skipped` on an `e2e.yml` suite job is the design working.** On a `pull_request` event without the `e2e` label the `select` job is gated off, every suite job's `contains(fromJSON(...))` reads an empty array, and they all skip. A skipped suite is not a missing measurement; exclude those runs from the cost arithmetic rather than counting them as free nights.
- **A `success` on a `ci.yml` run on main may mean nothing was built, and the job conclusion alone will not tell you.** `ci.yml` checks supersession in **two** places, and they leave different traces. `cdfde351` added the `supersession` gate job on the light tier: if a strictly-newer push run exists, the `go` and `rust` matrices evaluate their `if:` to false and the whole job conclusion is **`skipped`**. `c023d0fe` then added the same check, as the shared composite action `.github/actions/superseded`, as step 0 *inside* each heavy job, with every later step carrying `if: steps.superseded.outputs.superseded != 'true'` — so when that one fires the job runs, skips every real step, and concludes **`success`** having compiled nothing.

  **The in-job path is the one that fires, which is exactly why the job conclusion lies.** Do not argue this from a queue-time median — Step 4 explains why the median on this fleet is not a number you may cite. Measure the split directly instead, by classifying each run on when its successor push arrived relative to (a) the gate's answer, about six seconds after run creation on light, and (b) the first heavy job actually starting.

  Over the last 40 `ci.yml` push runs: 23 were never superseded, 15 were superseded while the heavy job sat in the queue, and 1 early enough for a creation-time gate to have caught it. **Of the runs that get superseded at all, the in-job check is what catches 15 of 16.**

  Two honesty notes on that figure, both load-bearing. First, it is **counterfactual for 34 of the 40 runs** — `cdfde351` landed at 03:33:45Z and `c023d0fe` at 03:44:50Z, so most of the window predates both call sites and the classification says where a supersession *would* have landed given real push timing and real queue waits, not what a guard did. What is *observed*, on the six runs where both call sites existed, is one supersession, caught in-job, with the gate catching nothing — consistent, but n=1 and far too small to carry the claim alone. Second, the counterfactual is robust exactly where the median is not: split those 40 runs in half and the older half reads 12 in-job to 1 at-creation, the newer half 3 to 0, while the median wait across the same halves moves by more than 3×. It is sensitive to one input only, the gate's own latency — 94% in-job at a 6 s or 15 s gate, but 42% at 120 s. If the light tier ever starts queueing, re-measure before trusting this paragraph.

  So "job conclusion `success` means the tree built" is wrong in the case that actually matters: every superseded run but one.

  Read the **step** conclusions instead. `b5ba4f06` added a dedicated step named `heavy work starting`, identically in the `go` and `rust` jobs, existing only to be read. Use it, not an incidental build step: a guarded step carries no `always()`/`failure()`, so it also skips when an *earlier* step failed, and a sentinel anywhere but first reports `skipped` for a job that built and then failed `test` — indistinguishable from supersession, in the direction that hides a red build. `heavy work starting` is first among the guarded steps, downstream of nothing but the guard itself.
```bash
gh run view <id> --json jobs --jq '
  .jobs[] | select(.name | startswith("build + test"))
  | {job: .name,
     conclusion: .conclusion,
     built: ([.steps[] | select(.name == "heavy work starting") | .conclusion] | first)}'
```
  Read the pair, never `built` alone — `null` has two meanings and only the job conclusion separates them:

  | job conclusion | `built` | meaning |
  |---|---|---|
  | `skipped` | `null` | creation-time gate fired; no heavy slot was ever claimed |
  | `success` | `"skipped"` | in-job check fired; **the node was held through the whole queue wait**, then given back unused |
  | `success` | `"success"` | it really built |
  | `success` | `null` | **PRE-SENTINEL** — a run older than `b5ba4f06`; unclassifiable on the build axis |

  That last row is the trap for this task specifically. Any sample window reaching back before `b5ba4f06` (2026-09-22 ~04:0xZ) contains runs with no sentinel step at all, and reading `built == null` as "the gate skipped it" would classify the entire pre-sentinel cohort as never-built — measured, 13 of the 14 most recent push runs at the time of writing. Emit `PRE-SENTINEL` and drop those from the build-time baseline rather than guessing.

  This matters twice over in Step 4 below. For the *build-time* baseline, exclude both superseded shapes and the pre-sentinel cohort — none of them prove a compile. For the *queue-time* baseline (the overlap measurement in Step 4), exclude only the `skipped`-job shape: an in-job-superseded run waited in the queue like any other heavy job, and so did every pre-sentinel run, so both are valid queue-time samples and dropping them understates contention. Conflating the two axes is how the nightly ends up blamed for pressure it did not cause, or excused from pressure it did.

  (Verified live on 2026-09-22: the in-job path on run `35684471028` — gate answered `superseded=false` at 03:48:09Z, the successor was pushed five seconds later, all three in-job checks said true at 03:49:00–03:49:53Z, and all three jobs concluded **`success`** with every build step skipped; and the sentinel reading `success` on run `35684942471`. **The gate-skipped row is still unobserved** — no `build + test` job in the last 14 push runs has conclusion `skipped`, so whether such a job reports an empty `steps` array is inferred, not measured. Confirm that row on the first one you see rather than trusting this line.)
- **A `success` that took more than one attempt is a failure you cannot see.** An OOM-killed runner and a reclaimed spot node produce the same "lost communication with the server" annotation, so `retry-lost-runners.yml` retries either one. Always pull the attempt number alongside `conclusion` — run-level as `attempt` from `gh run list`/`gh run view`, per-job as `run_attempt` from the REST route above; a green job on attempt 2 or 3 is the signature of a job that does not fit its tier, and it is the single most useful thing this task can surface.

- [ ] **Step 4: Measure contention, do not pre-empt it**

The question is whether the nightly starves `ci`. Measure it **at the job level, and only over jobs that actually got a runner**:

```bash
R=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
for id in $(gh run list --limit 60 --json databaseId,createdAt \
              --jq '.[] | select(.createdAt >= "<night start>" and .createdAt <= "<night end>")
                    | .databaseId'); do
  gh api "repos/$R/actions/runs/$id/jobs?filter=all&per_page=100" --jq '
    .jobs[]
    | select(.labels | index("gardenio-k3s"))        # heavy tier only; light/medium are separate pools
    | select((.runner_name // "") != "")             # drop jobs that never got a runner
    | {run: .run_id, name: .name,
       wait: ((.started_at | fromdate) - (.created_at | fromdate))}'
done
```

**Do not use run-level `startedAt` for this, which is what an earlier draft of this step did.** GitHub sets a run's `startedAt` equal to its `createdAt`, so run-level queue time is **identically zero** — measured 0s on all four runs spot-checked on 2026-09-22 (`35683806194`, `35684286705`, `35684471027`, `35683787431`) while the real job-level waits in those same runs were 4–42s. A step built on the run-level number reports "no queueing" in and out of the nightly window alike, so the decision rule below always returns *change nothing* regardless of what the nightly does. That is a silent always-pass, not a measurement.

**Both filters are load-bearing, and they fail in opposite directions:**

- `labels | index("gardenio-k3s")` — the light and medium pools have their own capacity and their waits are unrelated to the nightly. Mixing them in dilutes the heavy signal the nightly actually competes for.
- `runner_name != ""` — a job that was never scheduled reports `started_at == created_at`, i.e. **a wait of zero**. Skipped jobs do this, and so does a job displaced out of the pending slot by job-level concurrency (`steps: []`, empty `runner_name`, and the giveaway that each one's `completed_at` is the next one's `created_at`). Leaving them in stuffs the sample with artificial zeroes and drags the median down — manufacturing the conclusion that there is no contention, which is the same direction as the bug above.

The reciprocal error, for whatever cost arithmetic this step feeds: **never cost a cancelled job by `completed_at - started_at` without checking `runner_name` first.** For a displaced job that interval is time spent in the pending slot, not slot consumption; nine such `security` jobs summed to an apparent 18 heavy minutes on 2026-09-22 and the true figure was zero.

**Do not decide this by comparing a median inside the window against a median outside it**, which is what an earlier draft of this step asked for. The heavy-tier median is a function of who pushed last, not of capacity. Measured on 2026-09-22: the same metric, the same definition, the window slid forward by one day, `max` agreeing to the decimal so it is demonstrably the same population with newer runs appended —

| sample | n | median | p90 | max | >5m |
|---|---|---|---|---|---|
| older window | 40 | 10.0m | 45.8m | 50.8m | 22/40 |
| newer window | 40 | 2.8m | 41.0m | 50.8m | 15/40 |

The median moved 3.6× while p90 and max barely moved. Nothing got faster: a burst of guard-development pushes onto an already-drained pool flooded the sample with cheap records. **The tail is the stable statistic on this fleet; the middle is an artifact of push mix.** A window-vs-window median comparison is doubly exposed to this, because the nightly window has essentially no human pushes in it while the comparison window is mostly human push bursts — so the two samples differ in composition before capacity is considered at all, and the verdict can flip on how busy the operator happened to be that week.

Measure overlap instead, which does not depend on push mix. The nightly starves `ci` only if some *other* heavy job was actually waiting while the nightly held slots:

```python
# rows: (job_id, name, created, started, completed) for heavy jobs with a runner,
# from the query above, over a window spanning the nightly and the hours either side.
nightly = [r for r in rows if r.run_workflow == "e2e.yml"]
if not nightly:
    verdict = "nightly did not run; no measurement"
else:
    occupied = (min(r.started for r in nightly), max(r.completed for r in nightly))
    victims  = [r for r in rows if r not in nightly
                and r.started > occupied[0] and r.created < occupied[1]]   # pending during occupancy
```

Then judge on three nights together:

- **No `victims` across all three nights** → the nightly starved nothing that exists. Record "not measurable — nothing else was queued", **not** "no contention". Those are different findings and only the first is honest.
- **`victims` exist, and their waits sit in the ordinary body of the heavy-tier distribution** → change nothing.
- **`victims` exist and their waits land in the p90 tail** → starvation is real; propose a fix.

**If it is not real, change nothing.** Adding `max-parallel` pre-emptively costs nightly wall-clock and buys nothing.

If it is real, the minimal fix is a `strategy.max-parallel` on nothing (the jobs are independent, not a matrix) — so the actual lever is either moving the cron or splitting the nightly across two hours by giving `coop` its own `schedule` entry and a `github.event.schedule`-keyed `if:`. **Split across time, never into more jobs.** Wait on this pool is driven by concurrent slot-claims rather than total node-minutes, so carving the same work into a wider fan-out makes the queue worse even though the node-minutes are unchanged; moving half of it to a different hour is what actually reduces peak claims. Propose, do not apply: that is a capacity decision and belongs to the operator.

- [ ] **Step 5: Record the measurement on the bead**

Three nights of numbers, the contention verdict, and a recommendation. Close the task with the measurement even if the recommendation is "no change needed" — especially then, because that is the result the spec predicted and it deserves to be on the record.

---

### Task 10: Triage `go-integration`'s red set

**Files:**
- Modify: `.github/known-failures.md`
- Modify: `.github/workflows/e2e.yml` (drop `continue-on-error: true`, if the set is clean)
- Modify: whichever `kbeads/internal/store/postgres/*_integration_test.go` files are genuinely broken

**Interfaces:**
- Consumes: the failure census on T8's bead and the `go-integration-log` artifact.
- Produces: either a blocking `go-integration` job, or a `known-failures.md` section naming every remaining failure with a cause.

- [ ] **Step 1: Pull the census**

```bash
gh run download <T8's run id> -n go-integration-log -D /tmp/gi
grep -E '^\s*--- FAIL' /tmp/gi/*.log | sed 's/.*--- FAIL: //' | sort | uniq
```

- [ ] **Step 2: Reproduce each failure locally, one at a time**

```bash
docker run -d --name pgv -e POSTGRES_USER=beads -e POSTGRES_PASSWORD=beads \
  -e POSTGRES_DB=beads -p 5432:5432 pgvector/pgvector:pg16
sleep 10
PGPASSWORD=beads psql -h localhost -U beads -d beads -c 'CREATE EXTENSION IF NOT EXISTS vector;'
cd ~/ws/gardenio/kbeads
BEADS_TEST_DATABASE_URL='postgres://beads:beads@localhost:5432/beads?sslmode=disable' \
  go test -tags integration -v ./internal/store/postgres/ -run '<TestName>' -count=1
```

- [ ] **Step 3: Sort each failure into one of three buckets, and act**

1. **The test is wrong** (stale expectation, drifted schema) — fix the test. Commit each fix separately with the test name in the subject.
2. **The code is wrong** — this is a real bug the tier just found, which is the whole argument for running it. **File a bead**, link it from this task, and leave the test failing with an entry in `known-failures.md` naming the bead.
3. **The test needs something CI cannot give it** — record it with the reason; if it should self-skip, add the skip guard (the same shape the other 24 files use).

- [ ] **Step 4: If nothing is left failing, drop `continue-on-error`**

In `.github/workflows/e2e.yml`, delete from the `go-integration` job:

```yaml
    # continue-on-error: these 25 files have never executed in ANY CI -- not
    # GitHub Actions, not the deleted RWX pipeline. Landing red-tolerant
    # captures the coverage immediately instead of letting unknown breakage
    # stall the phase. Triaged and flipped to blocking by the T10 task of the
    # e2e-ci plan; the disposition lives in .github/known-failures.md.
    continue-on-error: true
```

and replace the `## \`continue-on-error\` in \`e2e.yml\`` section of `.github/known-failures.md` with a `### go-integration (was: continue-on-error)` entry under the file's existing `## Resolved` heading, which is where a fixed entry is moved and retitled.

- [ ] **Step 5: If something is left failing, say exactly what**

Update the `known-failures.md` census with the final numbers and one `###` entry per remaining failure: the test name, the bucket, the cause, and the bead id if it is bucket 2. `continue-on-error: true` stays, and its comment gains a line pointing at this task's bead as the record of why.

- [ ] **Step 6: Run it and commit**

```bash
git checkout -b agent/e2e-ci/<T10 bead id>
git add -A
git commit -m "ci: triage the go-integration tier"
git push -u origin agent/e2e-ci/<T10 bead id>
gh workflow run e2e.yml -f suites=go-integration --ref agent/e2e-ci/<T10 bead id> || \
  gh run list --workflow=e2e.yml --branch agent/e2e-ci/<T10 bead id> --limit 1
gh run watch <run id>
```

(The push trigger fires automatically on `agent/e2e-ci/**`; the dispatch line is there only if a targeted re-run is wanted.)

---

### Task 11: Rewrite the e2e user guide

`gardenio/docs/user-guide/11-e2e-testing.md` still documents the RWX/GitLab flow deleted on 2026-09-16. It was last edited 2026-09-20 without that being noticed, so anyone following it today is being actively misled.

**Files:**
- Modify: `gardenio/docs/user-guide/11-e2e-testing.md`

**Interfaces:**
- Consumes: everything above — the job names, the suite vocabulary, `HERMETIC_ONLY`, the three doors, and T7's skip census.
- Produces: nothing other tasks import.

- [ ] **Step 1: Read the current file and mark what is now false**

```bash
cd ~/ws/gardenio
git log -3 --oneline -- gardenio/docs/user-guide/11-e2e-testing.md
grep -n -i 'rwx\|gitlab\|\.rwx/\|dispatch' gardenio/docs/user-guide/11-e2e-testing.md
```

Every RWX and GitLab-CI reference is false. The `run-shard.sh` invocation details are still true. Preserve the file's existing heading structure and voice; this is a rewrite of content, not of format.

- [ ] **Step 2: Rewrite, covering exactly these things**

1. **What exists.** Counted 2026-09-22: **226** `test-*.sh` module files, of which **209** are wired as distinct `run_module` calls in `run-shard.sh` and **18** are in `HERMETIC_MODULES`; 68 viewer Playwright specs, 8 coop specs, 7 `//go:build e2e` files, 25 `//go:build integration` files, and `gardenio/tests/slack-e2e/`, which is permanently manual. Re-count before publishing rather than copying these — the suite is moving (225 → 226 on 2026-09-22 when `test-hermetic-set-lint.sh` landed).
2. **What runs in CI, and what does not.** The five `e2e.yml` jobs and the hermetic boundary. State plainly that **the ~191 wired modules outside `HERMETIC_MODULES` need a live cluster and do not run in CI** — and that as of 2026-09-22 this is **not in CI by decision, not merely "not yet"**. Write it as decided:

   > A Phase 4 running the in-cluster suite is **approved in principle** (operator decision O4, 2026-09-22): it would run on a **dedicated in-cluster `needs_k8s_api` role**, not on the CI runners' credentials. Two things gate it, deliberately. First, **a census spike comes before any credential** — roughly $1 of agent time to run the wired in-cluster set against a k3s-deployed gardenio and record, per module, pass / fail / needs-substrate-that-no-longer-exists. Nobody should mint a cluster credential for a suite whose pass rate is unknown. Second, the credential half is **gated on `gd-adirNxx3Qo`** (every pod can assume the node role): until that closes, a scoped ServiceAccount is documentation, not a boundary.

   Two corrections to the preconditions as previously written, both verified 2026-09-22:
   - **"No registry exists" is false for gardenio.** `ghcr.io/joyousaversion/gardenio/agent` is published with both `:latest` and `:<sha>` (`a9a4b568…`, updated 2026-09-22T06:43Z), alongside 23 other packages, with an ECR pull-through cache in front (`deploy-values/gardenio-k3s.yaml:50`). That blocker is a Gas City finding that does not transfer. But state the real constraint: `images.yml` **no longer runs on ordinary pushes to main** — `branches: [main]` was removed in `df984a49` on 2026-09-21 — so `:latest`/`:<sha>` arrive only on calver tag pushes (`tags: ['20*']`) and manual `workflow_dispatch`. Tags are computed at `images.yml:569-570`, pushed under the gate at `:599`. A Phase 4 cannot assume an image for an arbitrary main sha exists.
   - **"A k3s values overlay is missing" is stale; "a k3s *e2e* values overlay is missing" is still true.** `deploy-values/gardenio-k3s.yaml` exists, is tracked, is 891 lines as of `8d6d9f87`, and sets `global.storageClass: local-path`. It is the *production* overlay, and `git ls-files deploy-values/` returns that one file — there is no e2e-specific overlay anywhere in the repo, and nobody has shown the production one is fit for e2e use.
3. **The three doors**, with copy-pasteable commands:
   ```bash
   gh pr edit <n> --add-label e2e             # label a PR
   gh workflow run e2e.yml                    # everything
   gh workflow run e2e.yml -f suites=viewer   # one suite
   ```
   and the suite names: `wiring`, `viewer`, `coop`, `shell-hermetic`, `go-e2e`, `go-integration`.
4. **Running locally.** `HERMETIC_ONLY=1 ./gardenio/tests/e2e/scripts/run-shard.sh` from the repo root; `SMOKE_ONLY=1` and `MIGRATION_ACCEPTANCE=1` and what they need that the hermetic set does not; `ONLY=`/`SKIP=`; where JUnit and JSONL land.
5. **The hermetic contract**, and `test-hermetic-set-lint.sh`. Say what "hermetic" means here and that the list is linted, so nobody quietly shrinks it.
6. **What a skip means.** Quote T7's census: name which `-tags e2e` files skip in CI and why, so "the job is green" is never read as "all seven ran".
7. **Spot reclaims.** A failure with no failed step and a "lost communication with the server" annotation is a reclaim; `retry-lost-runners.yml` handles it; never change code in response to one.
8. **A history note**, brief: the RWX pipeline was deleted in 8a3eb29e on 2026-09-16 and the archive is preserved at `docs/archive/rwx/root/e2e.yml` and `ci.yml`. Link the spec.

- [ ] **Step 3: Check every link and command resolves**

```bash
grep -oE '\]\([^)]+\)' gardenio/docs/user-guide/11-e2e-testing.md \
  | sed 's/](\(.*\))/\1/' | while read -r p; do
      case "$p" in http*) continue;; esac
      [ -e "$p" ] || [ -e "gardenio/docs/user-guide/$p" ] || echo "BROKEN: $p"
    done
```

Expected: no output. Then run the quench gate, which lints docs links repo-wide:

```bash
. gardenio/images/agent/install/versions.env
. gardenio/images/agent/install/checksums.env
curl -fsSL "https://github.com/alfredjeanlab/quench/releases/download/v${QUENCH_VERSION}/quench-linux-x86_64.tar.gz" -o /tmp/quench.tar.gz
echo "${QUENCH_SHA256_X86_64}  /tmp/quench.tar.gz" | sha256sum -c --status -
mkdir -p "$HOME/.local/bin" && tar -xz -C "$HOME/.local/bin" -f /tmp/quench.tar.gz quench
"$HOME/.local/bin/quench" check --ci
```

Expected: no new findings. The `quench` job in `checks.yml` is no longer `continue-on-error`, so a broken link here reds CI.

- [ ] **Step 4: Commit**

```bash
git checkout -b agent/e2e-ci/<T11 bead id>
git add gardenio/docs/user-guide/11-e2e-testing.md
git commit -m "$(cat <<'EOF'
docs: rewrite the e2e user guide for the GitHub Actions flow

The file still described the RWX/GitLab dispatch that 8a3eb29e deleted on
2026-09-16, and was edited on 2026-09-20 without that being caught. It now
describes the five optional e2e.yml jobs, the three doors, the hermetic
boundary, and -- explicitly -- which modules still do not run anywhere, and
that their absence is a decision (O4) rather than an oversight.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01867LYYhYsvm7HB3MvQ2JSj
EOF
)"
git push -u origin agent/e2e-ci/<T11 bead id>
```

---

### Task 12: Right-size each e2e job's runner tier from measured usage

**Files:**
- Modify: `.github/workflows/e2e.yml` (the `runs-on:` line of the `viewer`, `coop`, `shell-hermetic`, `go-e2e` and `go-integration` jobs, plus temporary probe steps on the two candidate jobs)
- Use, do NOT modify: `.github/actions/peak-memory` (an existing composite action)
- Read only, do NOT modify: `gardenio/helm/ci/arc-runners-values.yaml`, `gardenio/helm/ci/arc-runners-medium-values.yaml`, `gardenio/helm/ci/arc-runners-light-values.yaml`

**Interfaces:**
- Consumes: all five suite jobs, as landed by Tasks 3, 4, 6, 7 and 8. This task runs LAST of the job-adding tasks; it does not add or remove a job, it only changes `runs-on:` values.
- Produces: nothing other tasks consume.

**Why this task exists.** Every suite job was written `runs-on: gardenio-k3s`, the heavy tier, because that is the tier the plan named and the tier that unquestionably works. A fully-labelled PR therefore fires five heavy jobs at once, one of them holding a runner for up to 45 minutes, into the pool that is the fleet's binding constraint — the ci pool is hard-capped at `ci_max_nodes` and one heavy runner needs a whole node (3 CPU / 12Gi request on 4-vCPU nodes), so the heavy tier cannot grow. Over a measured 4h40m window it ran pinned at its cap 65% of the time while `gardenio-k3s-medium` averaged 1.53 of 6 and `gardenio-k3s-light` 0.64 of 10. Moving jobs DOWN a tier is the only lever left.

**Be honest about the size of the prize.** This is a real but second-order win, and the task should not oversell itself. A census at 03:05Z found 34 jobs queued, all heavy, of which **26 belonged to nine pushes to main whose shas were already ancestors of the main tip** — `checks.yml` and `ci.yml` key concurrency per-SHA on push with `cancel-in-progress: false`, so a push to main supersedes nothing and every commit gets a full non-cancellable matrix. Three quarters of the heavy queue was work nobody was waiting on. That is being fixed separately in the CI lane (`a8ac1ba3` did `security`), and it is a much bigger lever than this task. Note also that `e2e.yml` does **not** have that pathology: its concurrency group already carries `github.ref` with `cancel-in-progress` on `pull_request`. So the case for this task is "do not add five permanent heavy tenants when two of them may not need to be heavy", not "the pool is starving because of us".

The standing objection to demotion — that a medium runner would trade a queue wait for a cold Go build — **has been retired**. Commit `b689d995` gave the medium tier the same node-local cache mounts the heavy tier has (`GOCACHE`, `GOMODCACHE`, `GOTOOLS_BIN`, `GOLANGCI_LINT_CACHE`) plus `NODE_NAME`. Medium deliberately does not get the cargo mounts; light deliberately gets no cache at all.

**The hard constraint that must not be optimised away.** `gardenio-k3s` is the only scale set with `containerMode: dind`. GitHub Actions **service containers require it**. Any job with a `services:` block therefore MUST stay on `gardenio-k3s` no matter what its memory numbers say. As of this plan that is `viewer` and `go-integration`, both of which use the postgres service container decided in `docs/ci-e2e-postgres-recipe.md`. `coop` needs the cargo/rustup cache mounts, which only the heavy tier's values file defines. Treat all three as fixed.

That leaves `shell-hermetic` and `go-e2e` as the only genuine candidates, and only if the measurements support it.

**Tier envelopes, for reference** (from the values files named above; read them, do not trust this summary if it disagrees):

| scale set | dind | cpu request | memory request | memory LIMIT | maxRunners | Go cache |
|---|---|---|---|---|---|---|
| `gardenio-k3s` | yes | `3` | 12Gi | 14Gi | 10 | yes, + cargo |
| `gardenio-k3s-medium` | no | `750m` | 1Gi | **4Gi** | 6 | yes, no cargo |
| `gardenio-k3s-light` | no | `250m` | 512Mi | **1536Mi** | 10 | none |

CPU is a request, not a limit, so a medium runner bursts above `750m` when the node has room. The memory **limit** is what decides this task.

**`peak` is not the decision number. `anonmax` is.** This is the single most important thing in this task, and getting it backwards has already cost real work twice, once in each direction. cgroup v2's `memory.peak` is the high-water mark of the whole pod *including page cache*, and page cache is reclaimable — a cgroup that touches 8Gi of files does not OOM at a 4Gi limit, the kernel drops cache first. The `security` job measured **peak 7967Mi**, which reads as a flat refusal and is not one. The number a memory limit actually kills on is unreclaimable anonymous memory.

But do **not** read the probe's plain `anon` either. `anon` in `memory.stat` is *instantaneous* and the kernel exposes no peak-anon counter, so a probe that runs after a step reports what that step has already freed. Measured on `security` (job `106601326347`): `peak` climbed 1179Mi → 8214Mi across the govulncheck step while `anon` read 88Mi → 91Mi. Those 91Mi are not govulncheck's footprint. Believing them would be the same mistake as believing the 7967Mi, only in the cheap direction — which is the worse one, because it ships an undersized job.

Use **`anonmax`**, which `.github/actions/peak-memory` reports since `a68c0c89`: a background sampler, one per job, polling `memory.stat` every 0.5s and keeping the maximum. It is monotonic like `peak`, so the delta between two probes attributes anonymous memory to the steps between them. A job at peak=8000Mi / anonmax=900Mi is a fine 4Gi tenant; one at anonmax=6000Mi is not.

`peak` remains useful as a **one-sided test**: `anonmax ≤ peak` always, so a job whose attributable `peak` already clears the target limit is *proven* to fit and needs no further measurement. Only a job that fails the `peak` test is ambiguous — not rejected.

**Read this before you interpret any failure in this task.** On this fleet an OOM kill and a spot reclaim are **indistinguishable from the outside**. `arc-runners-medium-values.yaml` documents the coupling: a runner killed for exceeding its limit produces the same "lost communication with the server" annotation a reclaimed spot node produces, so `retry-lost-runners.yml` cannot tell them apart and retries it. An undersized job does not fail loudly — it silently burns three attempts and only then goes red. Do not look for exit 137; it will not be there. The signal is the **retry**.

- [ ] **Step 1: Bracket each candidate job's suite step with the existing probe**

Do not write a probe. `.github/actions/peak-memory` already exists and is already collecting on every push to main; read its header before using it. Add two uses to `shell-hermetic` and to `go-e2e` only — not to `viewer`, `coop` or `go-integration`.

Immediately BEFORE the job's suite step:

```yaml
      - name: peak before the suite (temporary, removed in step 5)
        uses: ./.github/actions/peak-memory
        with:
          label: before-suite
```

Immediately AFTER it:

```yaml
      - name: peak after the suite (temporary, removed in step 5)
        if: always()
        uses: ./.github/actions/peak-memory
        with:
          label: after-suite
```

**Do not pass `reset: true`.** The kernel on these CI nodes rejects writes to `memory.peak`, so it returns `reset=unsupported` and does nothing. Attribution comes from the **delta between the two probes** instead: both `peak` and `anonmax` are monotonic, so `after − before` is what the suite step added. That is why the first probe is needed at all even though it resets nothing — it establishes the baseline, and it also starts the background sampler that makes `anonmax` meaningful.

`if: always()` on the second probe matters: a job that fails still tells you how much memory it wanted, which is the most useful case of all.

- [ ] **Step 2: Run both jobs on a branch and read the numbers**

```bash
git checkout -b agent/e2e-ci/<this bead id>
git add .github/workflows/e2e.yml
git commit -m "ci(e2e): temporary peak-memory probes on shell-hermetic and go-e2e"
git push -u origin HEAD
```

The `push: branches: ['agent/e2e-ci/**']` trigger fires the whole workflow on that branch. `gh api` cannot fetch job logs — the endpoint redirects to blob storage, and the download is UTF-8-BOM + CRLF, so plain `grep` finds nothing:

```bash
export GH_CONFIG_DIR=$HOME/.config/gh-ja
curl -sL -H "Authorization: Bearer $(gh auth token)" \
  "https://api.github.com/repos/mattbakerconsulting/gardenio/actions/runs/<run id>/logs" \
  -o /tmp/logs.zip
unzip -p /tmp/logs.zip | tr -d '\r\0' | grep -a PEAKMEM
```

Record, per job, **both** probe lines in full — you need the pair, not just the second one — and from them compute `Δpeak` and `Δanonmax`. Also record `node` and the job's wall-clock duration in a comment on this bead. Record the node because every cache in this pool is node-local, so a duration without a node beside it is not a measurement.

Sanity-check as you go: if `anonmax` is identical on both lines while `peak` moved a lot, the sampler did not start — say so rather than reporting the zero as a result.

- [ ] **Step 2b: Only if a job's `Δpeak` is at or above 2800Mi**

If `Δpeak` came in under 2800Mi, that job is **proven** to fit and you are done measuring it — `anonmax ≤ peak`, so no further evidence can change the answer. Skip to Step 3 for it.

Only a job that fails the `peak` test is ambiguous, and for it there is still nothing to build: `Δanonmax` from the probes you already added is the number. Do **not** hand-roll a sampler; `.github/actions/peak-memory` has one, it is started automatically by the first probe, and a second sampler would just contend with it. If `Δanonmax` is implausibly small for a job whose `Δpeak` is large, re-run once before believing it, and report both runs.

- [ ] **Step 3: Decide, with an explicit margin**

Move a job to `gardenio-k3s-medium` only if the number you have is **below 2800Mi** — the 4Gi limit with roughly 30% headroom for run-to-run variance and for the suite growing. The number you have is `Δpeak` if that already cleared the bar, otherwise `Δanonmax`. Anything at or above 2800Mi stays on `gardenio-k3s`; say so in the bead comment and move on. Do not move anything to `gardenio-k3s-light`; its 1536Mi limit is not a realistic ceiling for a Go test binary, and it has no cache at all.

Decide the two jobs independently. Do not average them and do not reason about them together.

- [ ] **Step 4: Apply the decision**

For each job that qualifies, change exactly one line:

```yaml
    runs-on: gardenio-k3s-medium # no dind: this job has no `services:` block. Measured <peak|anon> <N>Mi against a 4Gi limit (gardenio/helm/ci/arc-runners-medium-values.yaml).
```

If neither qualifies, change nothing and skip to step 5. A task that measures and correctly changes nothing has succeeded.

- [ ] **Step 5: Remove every temporary step**

Delete both probe steps, and the sampler pair if Step 2b was used, from both jobs. Verify with `grep -c 'temporary, removed in step 5' .github/workflows/e2e.yml`, which must print `0`. Leave `.github/actions/peak-memory` itself alone — it is shared, and its own header says who deletes it and when.

- [ ] **Step 6: Re-run the branch and confirm the moved jobs are genuinely healthy**

Push again and check two things for every job that changed tier, not one:

1. It is green.
2. It passed on its **first attempt**. Per the note above, an OOM on medium is retried as if it were a spot reclaim, so a green job that took two or three attempts is the exact signature of a job that does not fit. Check with `gh run view <run id> --json jobs -q '.jobs[] | "\(.name) attempt=\(.runAttempt) \(.conclusion)"'`, or read the run's annotations for a lost-communication message.

If a moved job fails, or needed a retry, revert it to `gardenio-k3s` in the same branch rather than debugging it. The heavy tier is the known-good answer and this task is an optimisation, not a rescue.

- [ ] **Step 7: Commit and open the PR**

```bash
git add .github/workflows/e2e.yml
git commit -m "ci(e2e): right-size runner tiers from measured memory"
git push
gh pr create --fill --label e2e
```

The `--label e2e` matters: it opens the label door so the full suite runs on the PR that changes where the suite runs.

**Out of scope.** Do not edit any file under `gardenio/helm/ci/`, do not change `maxRunners` on any scale set, do not touch `gardenio/terraform/`, and do not change concurrency groups in `ci.yml` or `checks.yml`. All of those are owned by another session and a change there has fleet-wide effects. If the conclusion of this task is that the pools themselves are mis-sized, say that in a comment on this bead and stop — it is a separate decision for the operator.

---

## Operator decisions this plan waits on

Filed as decision beads, related to the plan epic. None blocks the plan as a whole. **All five are resolved as of 2026-09-22** (O1/O3/O4 operator-delegated to laptop-claude).

| # | Decision | Blocks | Ruling |
|---|---|---|---|
| O1 | P0-b — exempt `schedule` runs from the retry staleness guard. Changes decision logic in fimba-91's file. | T0 only | **APPROVED.** Landed `d5edca64`. See the correction in T0 Step 3: the outcome stands, the commit-rate argument originally given for it does not. |
| O2 | Create the `e2e` repo label. It does not exist; `gh label list` shows only GitHub defaults. | T2 Step 7's label half only | Resolved. |
| O3 | The nightly cron hour. Capacity-shaped: the pool is shared with `ci`/`checks`/`images`. | T9 | **`cron: '17 9 * * *'`** — 09:17 UTC. Substituted into T9 Step 2. |
| O4 | Whether Phase 4 (the in-cluster modules) proceeds at all, and on which credential model. Touches RBAC and the chart/namespace lane. | nothing in this plan | **PROCEED**, on a dedicated in-cluster `needs_k8s_api` role — but **census spike first, no CI credential**, and the credential half gated on `gd-adirNxx3Qo`. Recorded in T11 Step 2. |
| O5 | Whether the crew/gardener fine-grained PAT gains `Actions: Read and write`, so crews and gardeners can call `workflow_dispatch` and `rerun-failed-jobs` themselves. Filed by the plan gardener after both bit it on PR #73. | nothing — the dispatch proof it blocked was completed from an operator session on 2026-09-22 | Closed. |

**The rulings were independently verified on 2026-09-22 and several of their stated reasons did not survive.** 36 factual claims checked, 20 not confirmed. The three that changed text in this plan:

1. **O1's "the plan cites a scheduled run that was never scheduled" is itself wrong.** The plan (`:103`) and the spec (`:205`) both say "push run", correctly — `35669237327` was `event=workflow_run` and classified `35667563888`, a push run. Nothing was misattributed and nothing needed correcting. The spec's argument was always a-fortiori.
2. **O1's "5-9 commits/hour" is wrong and argued the wrong way.** See T0 Step 3.
3. **O4's two cited blockers were a wrong-repo finding and a stale one.** See T11 Step 2.

The pattern is worth naming, because it will recur: **a ruling is a decision plus a rationale, and delegated authority covers the decision only.** All three outcomes held under verification; three of their supporting facts did not. Re-check a ruling's numbers before quoting them into code comments or user-facing docs, where they outlive the decision.

---

## Self-review

**Spec coverage.** Every section of `docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md` maps to a task:

| Spec | Task |
|---|---|
| §3 architecture — `on:`, `select`, concurrency, the label guard | T2 |
| §4.1 `viewer-e2e`, all 7 contract facts | T3 |
| §4.2 `coop-e2e`, debug build + hard bun requirement | T4 |
| §4.3 `shell-hermetic`, `HERMETIC_ONLY` + pinned list + lint | T5, T6 |
| §4.4 `go-e2e`, skip guards not exclusions | T7 |
| §4.5 `go-integration`, red-tolerant + known-failures | T8 |
| §4.6 cross-cutting — sha256 pins, `cache: false`, artifacts | Global Constraints, applied in T3/T4/T6/T7/T8 |
| §5 postgres decided by spike | T1 |
| §6 Phase 0 — P0-a, P0-b | T2 Step 2, T0 |
| §7 validation — push trigger, `gh run list`, green-run DoD | Global Constraints; every job task's "record" step |
| §8 phases | the phase table |
| §9 risks R1–R8 | R1→T1; R2→T9 Step 4; R3→T4 Step 3; R4→accepted, stated in T4's timeout comment; R5→T5's lint; R6→T7 Step 1; R7→Global Constraints; R8→T0's gate and T2 Step 2 |
| §10 deferred in-cluster tier | out of scope by decision; surfaced in T11 Step 2 item 2 and O4 |
| §11 operator decisions O1–O4 | the operator-decisions table |

One deliberate refinement of the spec: §3 wrote the suite gate as `contains(needs.select.outputs.suites, '<name>')` (substring). This plan uses `contains(fromJSON(needs.select.outputs.suites), '<name>')` (exact array membership), because substring matching would make a future suite named `go-e2e-cluster` silently enable `go-e2e`. Same architecture, tighter gate.

One spec correction was made while planning, in the spec file itself: §4.3 asserted the driver skips `wait-for-stack-ready.sh`, the `kd-beads-token` load and `seed-types-and-users.sh` under `HERMETIC_ONLY`. None of the three is in `run-shard.sh` — they were upstream RWX tasks. The real structural change is `run_module`'s per-module port-forward preamble, which §2.1 now names and T5 Step 8 implements.

**Placeholder scan.** The only `<…>` placeholders are bead ids (unknown until minting), run ids and urls (unknown until running), the derived `HERMETIC_MODULES` list (deriving it *is* T5 Step 3), the measured numbers in T1's doc template and T8's census, and the cron hour (operator decision O3). Each is a value the task produces, not work left undescribed. Every code step carries real code.

**Type consistency.** `needs.select.outputs.suites` is a JSON array string everywhere, produced once in T2 and read with `fromJSON` in T3/T4/T6/T7/T8. Suite names are the same six strings in the `ALL` array and in every `if:`. The postgres DSN `postgres://beads:beads@localhost:5432/beads?sslmode=disable` is identical in T1's doc, T3's `BEADS_DATABASE_URL` and T8's `BEADS_TEST_DATABASE_URL`. `HERMETIC_ONLY` and `HERMETIC_MODULES` are spelled the same in `run-shard.sh`, the lint and the workflow. The pinned Node install block in T3 and T4 is byte-identical.
