# Optional e2e jobs in GitHub Actions: revive the orphaned suite, hermetic tier first

**Status:** design spec, approved in conversation 2026-09-21, awaiting written review.
**Repo:** gardenio. New file `.github/workflows/e2e.yml`; changes to `gardenio/tests/e2e/scripts/run-shard.sh` and `.github/workflows/retry-lost-runners.yml`.
**Prior art this builds on:** the surviving e2e harness (`gardenio/tests/e2e/scripts/run-shard.sh`, 3,350 lines, 209 wired modules), the deleted RWX pipeline preserved byte-identical at `docs/archive/rwx/root/e2e.yml` and `docs/archive/rwx/root/ci.yml`, the optional-job pattern in `.github/workflows/images.yml`, the pinned-tarball install pattern from the quench gate (PR #26), and `.github/workflows/retry-lost-runners.yml` (landed 2026-09-21 16:43–16:50Z).

---

## 0. Decisions already taken

Settled in the brainstorming conversation. These are not open.

| # | Decision | Choice |
|---|---|---|
| D1 | What the jobs run against | **Hermetic first, cluster later.** Phase 1–3 ship only suites needing no live cluster. The ~150 in-cluster modules are deferred to a later, separately-approved phase. |
| D2 | What "optional" means | **Three doors, never a required check:** an `e2e` label on a PR, `workflow_dispatch` with suite/shard/only/skip inputs, and a nightly cron on main. |
| D3 | Why the nightly is mandatory | The suite rotted precisely because nothing ran it. A dispatch-only job is green the day it lands and untested three weeks later. The nightly is the anti-rot mechanism, not a nice-to-have. |
| D4 | Workflow file layout | **One new `e2e.yml`** with a `select` job resolving which suites run, and per-suite jobs gated on its output. Not jobs added to `ci.yml`/`checks.yml`; not `workflow_call`. |
| D5 | The hermetic shell tier | **A third curated subset inside `run-shard.sh`** (`HERMETIC_ONLY`), alongside the existing `SMOKE_ONLY` and `MIGRATION_ACCEPTANCE`. Not a second driver script, not a loop in YAML. |
| D6 | The `//go:build integration` tier | **In scope for this plan, landing `continue-on-error: true`.** 25 files that have never executed anywhere; they need exactly the postgres+pgvector service `viewer-e2e` already stands up. It sits in Phase 2 of §8's finer-grained phasing only because it reuses T1's postgres recipe and follows `viewer-e2e`; it is not deferred. |
| D7 | `slack-e2e` | **Permanently out of scope.** It requires a human to hand-capture MFA-gated session cookies that expire in weeks. It cannot be CI. Its wrapper already SKIP-passes unless `SLACK_UI_REQUIRE=true`; that stays. |
| D8 | Execution model | **Plans-as-beads.** Kickoff crew mints phases and tasks from this spec's plan; `gb plan gardener`; crews open `agent/e2e-ci/*` PRs. Operator reviews verdicts and merges under the gardenio delegation. |
| D9 | Runner-loss retry | **Phase 0, ahead of everything.** Not built from scratch — `retry-lost-runners.yml` already exists; Phase 0 is two precise fixes to it. |
| D10 | Cluster credentials | Whatever the deferred in-cluster phase chooses, it is **never** the IMDS→SSM admin kubeconfig. That path is the open P1 `gd-adirNxx3Qo`. |

## 1. Goals and non-goals

**Goals**

- Every e2e suite that can run without a live cluster runs in GitHub Actions, on demand and nightly.
- A green nightly is a truthful claim that those suites pass. A red one names which suite and hands back a JUnit artifact.
- The mechanism is provably alive: each job is demonstrated green on a real run before its task is done, and the nightly keeps proving it.
- The 25 never-executed `//go:build integration` files get a home and a triage path.
- Nothing in this plan increases the cluster's attack surface or requires new cluster credentials.

**Non-goals**

- Not making e2e a required check. It is optional by design (D2).
- Not porting the ~150 in-cluster modules. That is §10, deferred.
- Not `slack-e2e` (D7), not the 18 gVisor modules (already `GVISOR_E2E=1`-gated and needing real gVisor nodes).
- Not fixing `checks.yml`'s inline `HELM_VER`/`YQ_VER` drift away from `versions.env`. Real debt, different plan.
- Not resolving the remaining `if: false` `deferred` D-rows in `checks.yml` beyond the beads-viewer one this delivers.

## 2. Background: what exists, and what broke

gardenio has substantial e2e coverage and **none of it has run in GitHub Actions since 2026-09-16**.

- `gardenio/tests/e2e/scripts/` — 225 `test-*.sh` modules on disk, **209 wired** into `run-shard.sh`'s registry. 16 are unwired by design (12 hermetic, 4 gVisor), listed in the driver's header exclusion block.
- `gardenio/tests/beads-viewer-e2e/` — 68 Playwright spec files, ~320 tests.
- `coop/tests/e2e/` — 7 MockCoop `mux-*.spec.ts` plus one pixel-diff `preview-fidelity.spec.ts`.
- `gardenio/tests/slack-e2e/` — 6 tests, 2 specs, real Slack workspace required.
- 7 Go files behind `//go:build e2e`, plus 39 untagged `*_e2e_test.go` that `ci.yml` already runs, plus 25 behind `//go:build integration` that nothing runs.

The historical runner was GitLab CI dispatching an RWX pipeline against a live EKS namespace (`gardenio-rwx`). Commit **8a3eb29e** (2026-09-16, "Delete the RWX pipelines and everything that shelled out to them", 84 files, 32,992 deletions) removed `.gitlab-ci.yml` and every `.rwx/` spec as part of the move to GitHub Actions. **The scripts survived; the orchestration did not, and nothing replaced it.** `deploy-values/gardenio-rwx-*.yaml` went with it.

This is a quiet failure, which is what makes it worth fixing carefully: the suite still works, `make e2e` still runs (three hermetic scripts), CI is green, and none of that touches the 209 modules. `gardenio/docs/user-guide/11-e2e-testing.md` still documents the RWX/GitLab flow and is actively misleading.

**This plan reverses an explicit non-goal.** Closed epic `gd-ljoVmxrW8O` listed "do not port … the E2E gating" among its non-goals. That was the right call for that epic's scope. Citing it here rather than quietly contradicting it.

### 2.1 What the harness actually requires

`run-shard.sh` reads `E2E_SHARD`, `TOTAL_SHARDS` (default 4; shard pins are only valid at ≥4), `NAMESPACE` (default `gardenio-rwx`), and `BEADS_TOKEN` (env or `/tmp/beads-token`). Optional: `ONLY`, `SKIP` (comma lists — there is no `EXCLUDE`), `SMOKE_ONLY`, `MIGRATION_ACCEPTANCE`, `KEEP`, `SHARD_BUDGET_SECONDS` (1500).

Registry entries are repo-relative (`./gardenio/tests/e2e/scripts/test-tls-certs.sh`), so **cwd must be the repo root**. Reporting goes to `$PWD/junit-results-${E2E_SHARD}/` as `e2e-results.jsonl` and `e2e-summary.xml`. Modules register a skip by writing a reason to `E2E_SKIP_SENTINEL` and exiting 0; the driver exits 0 when everything passed or skipped, 1 on any failure.

Tool usage across the 209 wired modules: `kubectl` 166, `kd` 135, `gb` 88, `go` 47, `claudeless` 25, `helm` 8, `terraform` 1, `node/npx` 1, **`docker` 0**. Roughly 18 are fully hermetic; ~150 need a live cluster; 14 mutate the deployed stack (`rollout restart`, `scale`, `delete pod`), which is why pointing this at the live `gardenio` namespace was rejected outright.

One subtlety that shapes §4.3: **the harness itself is not hermetic even when the module is.** `run_module` calls `ensure_port_forward` / `ensure_bridge_port_forward` and curls `localhost:19090` and `:19091` before every module (`run-shard.sh:764-800`), so a hermetic module driven through the registry still reaches for a cluster. Making that preamble conditional is the structural half of `HERMETIC_ONLY`.

Nothing in the scripts is EKS-specific. The EKS coupling lived in the deleted values overlays, not the harness.

## 3. Architecture: `e2e.yml`

One new file. `ci.yml` and `checks.yml` are untouched.

```yaml
on:
  pull_request:
    types: [labeled, synchronize]
  push:
    branches: ['agent/e2e-ci/**']
  workflow_dispatch:
    inputs: {suites: '', shards: '', only: '', skip: ''}
  schedule:
    - cron: '<off-peak>'
```

**`select` job** — `gardenio-k3s-light`, seconds, **no checkout at all**: it reads only the event context and the dispatch inputs. Resolves one `suites` JSON output:

- `schedule` → all suites
- `workflow_dispatch` → `inputs.suites` filtered with jq; empty means all
- `pull_request` / `push` → all hermetic suites

The job is guarded `if: github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'e2e')`, because `types: [labeled]` fires for **any** label, not just ours. Each suite job is then `if: contains(needs.select.outputs.suites, '<name>')`. This mirrors `images.yml`'s `changes` job, which already filters a `workflow_dispatch` input through jq — a pattern the repo maintains rather than a new one.

**The `push: agent/e2e-ci/**` trigger is load-bearing, not convenience.** See §7.

**Concurrency:**

```yaml
concurrency:
  group: e2e-${{ github.ref }}-${{ github.event_name }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

`github.event_name` **must** be in the group key. GitHub evicts the *pending* run whenever a new run joins a group, regardless of `cancel-in-progress` — learned on `images.yml` (`gd-R1kL4sUECV`). Without the event split, a push to main at 07:05 silently kills the nightly, and the nightly is the whole anti-rot mechanism (D3).

**The `e2e` label does not exist in the repo yet.** `gh label list` shows only the GitHub defaults. Creating it is an operator action, done once, before the label path can work.

## 4. The suite jobs

Five jobs, independent, `needs: select` only.

| job | tier | headline cost | gate |
|---|---|---|---|
| `viewer-e2e` | `gardenio-k3s` (dind) | ~8–12 min | postgres+pgvector |
| `coop-e2e` | `gardenio-k3s` | ~20 min | cargo + bun |
| `shell-hermetic` | `gardenio-k3s` | <10 min | `HERMETIC_ONLY=1` |
| `go-e2e` | `gardenio-k3s` | <10 min | `-tags e2e` |
| `go-integration` | `gardenio-k3s` (dind) | ~15 min | postgres+pgvector, **`continue-on-error: true`** |

All five want the heavy tier because all five build Go or Rust; the light tier is capped at 1536Mi. **A full nightly can therefore occupy 5 of the pool's 6 slots.** This is the plan's principal operational risk and is handled by measurement (§8, T9), not by pre-emptive throttling.

### 4.1 `viewer-e2e`

The flagship: 68 spec files, ~320 tests, and the one `checks.yml` already names as unfinished debt in its `if: false` `deferred` job ("beads-viewer e2e … needs node/playwright + a live viewer; port after gardenio is deployed"). It turns out not to need a deployed gardenio at all.

1. Postgres + pgvector. **`CREATE EXTENSION IF NOT EXISTS vector` is non-negotiable** — `kbeads/internal/store/postgres/migrate_preflight.go:66` hard-fails without it.
2. Build `kd`: `cd kbeads && CGO_ENABLED=0 go build -o /tmp/kd ./cmd/kd/`. The `libicu-dev` + CGO in `ci.yml`'s matrix is for the kbeads *test* suite, not this binary.
3. `kd serve &` with `BEADS_AUTH_TOKEN=e2e-primary-static-token`, `BEADS_DATABASE_URL=postgres://beads:beads@localhost:5432/beads?sslmode=disable`, `BEADS_RATE_LIMIT_ENABLED=false`. Poll `/v1/health/live`.
4. Build `beads-viewer` and set **`BEADS_VIEWER_BIN`**. Without it `playwright.config.js`'s `webServer.command` falls back to `go run ./gardenio/controller/cmd/beads-viewer` and compiles inside the test timeout.
5. `globalSetup.mjs` runs `seed/seed.mjs`, which shells out to `kd` for ~95 fixtures, idempotent by the `fixture:e2e:<key>` label. **`kd` must be on PATH.** This is the step most likely to surprise us.
6. Install chromium **and** firefox — `popover_constellation.spec.js` is the sole firefox project.
7. `CI=true` makes the config set `forbidOnly`, `retries: 1`, `workers: 2`.

The archive recipe (`docs/archive/rwx/root/ci.yml:2757-2947`, `timeout 10m`) does all of this with `apt install postgresql-16 postgresql-16-pgvector`, and `gardenio/plans/archive/beads-viewer-playwright-tests.md:187` targets "< 3 min from clean checkout" for the test phase. Budget 8–12 min with installs; `timeout-minutes: 25`.

### 4.2 `coop-e2e`

`cargo build -p coopmux` in **debug**, because `playwright.config.ts` defaults `MUX_BIN` to `../../target/debug/coopmux`.

**`bun` is hard-required, not a preference.** `coop/tests/e2e/lib/fidelity-server.ts:142-149` calls `execSync("bun build …")`. `run.sh` prefers bun and falls back to npm for the *runner*, which obscures this — the fidelity server needs bun regardless. Pin `BUN_VERSION=1.3.13` from `versions.env`.

Seven mux specs use MockCoop and never launch a real coop. `workers: 1`, chromium only. RWX history was ~8 min, plus a cold cargo build with no cache (§4.6): budget ~20 min, `timeout-minutes: 45`. **This is the long pole and the most reclaim-exposed job in the repo.**

### 4.3 `shell-hermetic`

`HERMETIC_ONLY=1 ./gardenio/tests/e2e/scripts/run-shard.sh`, run from the repo root, with `gb` and `kd` installed to `/usr/local/bin`.

The driver gains a third curated subset next to the two that already exist:

```sh
SMOKE_MODULES="tls-certs,stack-health,agent-crew-lifecycle,beads-crud,mail-ready,trigger-engine-smoke"
MIGRATION_ACCEPTANCE_MODULES="stack-health,tls-certs,migration-postauth-viewer,..."
HERMETIC_MODULES="<the pinned set, ~18>"
```

The one structural change: the cluster preflight becomes conditional. Under `HERMETIC_ONLY` the driver skips the `kubectl wait` deployment gates, the pf-supervisor port-forwards, the `BEADS_TOKEN` hard guard, the `kd list` connectivity gate, and the failure-path `kubectl logs` dump — and `run_module`'s per-module `ensure_port_forward` / health-curl preamble becomes a no-op. JUnit/JSONL emission, the module registry, and the `E2E_SKIP_SENTINEL` protocol are untouched.

Note for the implementer: `wait-for-stack-ready.sh`, the `kubectl … get secret kd-beads-token` token load and `seed-types-and-users.sh` are **not** in `run-shard.sh` — they were upstream tasks in the deleted RWX harness (`docs/archive/rwx/root/e2e.yml:1842-1850`, `:1893`, `:1976`). There is nothing to make conditional for those; the new workflow simply never invokes them.

**The module list must be pinned and then linted.** A companion check fails if any module in `HERMETIC_MODULES` acquires a `kubectl`/`kd`/`gb` dependency. Without it this set rots silently — which is the exact failure this whole plan exists to fix. `test-shard-isolation.sh` is the existing precedent for a hermetic lint of the driver, and a new lint script must also be added to that file's machine-parsed exclusion block (`run-shard.sh:29-106`) or `test-shard-isolation.sh` reds.

Deriving the set is part of the task, not an input to it: a static categorisation puts it near 18, and the authoritative list is whatever passes with no cluster reachable.

### 4.4 `go-e2e`

`go test -tags e2e ./...` across controller and kbeads, with `gb` built and on PATH (`hooks_natsrelay_e2e_test.go` and `subagentstop_e2e_test.go` need `GB_BIN`; both use an embedded NATS and cost nothing).

Five of the seven tagged files are cluster-free. `stranded_recovered_livecluster_e2e_test.go` is confirmed to self-skip. **The remaining behaviour is unverified:** the two `*_livedaemon_e2e_test.go` files need a live daemon SSE stream, `auditxform_e2e_test.go` needs `VICTORIA_LOGS_BIN`, and `gardener_start_argv_emit_e2e_test.go`'s wrapper needs a live daemon.

Where one hard-fails without its dependency, **add a skip guard; do not exclude the package.** Excluding is how coverage disappears quietly, which is the failure mode under repair.

### 4.5 `go-integration`

`continue-on-error: true`, reusing the same postgres+pgvector service. `BEADS_TEST_DATABASE_URL` set, `go test -tags integration ./...`. kbeads needs `libicu-dev` and CGO here, unlike the `kd` binary in §4.1.

25 files that have never executed in any CI. **Expect red.** The job lands red-tolerant with an entry in `.github/known-failures.md` — the existing home for `continue-on-error` debt, per the quench-gate precedent — and a follow-up task triages and flips it to blocking. Landing it red-tolerant is deliberate: it captures the coverage immediately without letting unknown breakage stall the phase.

### 4.6 Cross-cutting

- **Every tool install is a sha256-verified release tarball** with pins sourced from `gardenio/images/agent/install/{versions,checksums}.env` — the PR #26 quench-gate pattern. Never `npm i -g`, never `curl | sh`, never `cargo install` per run. Relevant pins: `NODE_FULL_VERSION=24.15.0`, `GO_FULL_VERSION=1.25.9`, `PLAYWRIGHT_VERSION=1.58.2`, `BUN_VERSION=1.3.13`, plus `PLAYWRIGHT_SYSTEM_DEPS`.
- **`cache: false` on every `setup-go`.** `ci.yml` sets it because an Actions cache upload once took 18 minutes and timed out a job. Cold builds are the accepted cost.
- Failures upload `junit-results-*` and the Playwright HTML report with `if: always()`.

## 5. Postgres + pgvector: decided by spike, not by assumption

Two candidate mechanisms:

- **Actions `services:` with `pgvector/pgvector:pg16`.** `gardenio-k3s` is `containerMode: dind`, so service containers should be available, and an image pull beats `apt` on every run.
- **The archive's `apt install postgresql-16 postgresql-16-pgvector`.** Proven in `docs/archive/rwx/root/ci.yml`, but slower.

The service-container path is preferred **and not confident** — ARC + dind + service containers is a finicky combination. This is Task 1, a spike: try services, fall back to apt, and **write down which one and why**, so the fallback is documented rather than rediscovered later. Two jobs (`viewer-e2e`, `go-integration`) depend on the answer.

## 6. Phase 0: runner-loss retry

The ci pool is 100% spot. A reclaim mid-job kills the runner, GitHub marks the job `failure`, and nothing retries it (`gd-IoVkE0k8ot`). A 45-minute `coop-e2e` is the most exposed job in the repo, and a nightly that dies at 07:00 with nobody watching reads as "e2e is broken" — which destroys the signal's credibility faster than having no signal at all.

**This is already built.** `.github/workflows/retry-lost-runners.yml` landed on main 2026-09-21 16:43–16:50Z. It keys on the Actions service's own `"lost communication with the server"` annotation, having measured and rejected the naive "failed job with no failed step" heuristic (a 10m45s job against `timeout-minutes: 10` presents identically). It re-runs only when *every* failed job lost its runner, caps at 3 attempts, and offers a dry-run `workflow_dispatch`. It is live and discriminating correctly.

Phase 0 is therefore two precise fixes, not a build:

**P0-a — add `e2e` to the retry allowlist.** `workflows: [ci, checks, images]` is explicit, and the file's own comment says new heavy workflows must be added or their lost runners go unretried. Rides with the PR that lands `e2e.yml`; takes effect only once on main, since `workflow_run` workflows always run from the default-branch copy.

**P0-b — exempt scheduled runs from the staleness guard.** The guard exempts only `images.yml`; for everything else a moved branch tip reads as "a newer run supersedes this one". A scheduled run has `head_branch = main`, and main moves constantly — run `35669237327` already SKIPped a push-to-main run for exactly this reason at 23:48Z. **But a nightly is superseded by nothing**: there is no newer run until tomorrow, so a reclaimed nightly is simply lost. That is precisely the `images` argument. Suggested change: exempt `run.event === 'schedule'` alongside the existing path check.

**P0-b is an operator decision, not a crew task.** It alters the decision logic of a file that belongs to fimba-91's lane and is hours old. P0-a is additive and explicitly invited by the file; P0-b is not.

`gd-IoVkE0k8ot` stays open regardless: its third acceptance criterion — demonstrate a lost-runner job is retried automatically — is half met. The negative half is proven (a genuine failure correctly SKIPped); no run has yet reached a `RETRY` decision.

## 7. Validation: how anything is proven

The requirement is that these jobs **work**, not that the YAML parses. Two constraints shape how that is demonstrated:

1. **A new workflow's `workflow_dispatch` and `schedule` triggers do not exist until the file is on the default branch.** Only `pull_request` and `push` run from the branch.
2. **The crew PAT is Contents-only**, so a crew cannot add a label to its own PR. The `e2e`-label path is unavailable as a self-test mechanism.

Hence the `push: branches: ['agent/e2e-ci/**']` trigger in §3. It is self-limiting to this plan's own branches, needs no label, and lets each crew verify its own job before requesting review. It is retained after Phase 1 — it costs nothing when no such branch exists, and it remains the way future e2e work is tested.

Crews read their results with **`gh run list` / `gh run view`**, never the check-runs API: that is still 403 for the crew PAT (`gd-XldfmHq3ez`; operator decision `gd-4j5m4jtW34` open). Keep the `gh run list` fallback until that clears.

**Definition of done for every job task: a green run of that job, on its own branch, linked from the bead.** Not "the workflow file is committed".

## 8. Implementation phases

**Phase 0 — runner-loss retry (§6).** P0-a, add `e2e` to the allowlist (rides with T2). P0-b, the schedule exemption — operator decision.

**Phase 1 — foundation and Playwright.**
- **T1 (spike):** postgres+pgvector on ARC dind — services vs apt. Output is a decided, written-down recipe. Blocks T3 and T8.
- **T2:** the `e2e.yml` skeleton — `select`, all four triggers, concurrency, and one trivial suite job proving the wiring end to end. Blocks everything. Includes P0-a. Requires the `e2e` label to exist.
- **T3:** `viewer-e2e`.
- **T4:** `coop-e2e`.

**Phase 2 — shell and Go tiers.**
- **T5:** `HERMETIC_ONLY` in `run-shard.sh` + the pinned module list + the isolation lint. Testable without CI; TDD applies.
- **T6:** `shell-hermetic`.
- **T7:** `go-e2e`, with skip guards for whichever dependency-gated files hard-fail.
- **T8:** `go-integration`, `continue-on-error`, `known-failures.md` entry.

**Phase 3 — make it stick.**
- **T9:** enable the nightly, then **measure** peak concurrent slot usage against `ci` queue depth over several nights. Throttle only if starvation is real and observed. No pre-emptive `max-parallel`.
- **T10:** triage `go-integration`'s red set; flip to blocking, or file what remains with reasons.
- **T11:** rewrite `gardenio/docs/user-guide/11-e2e-testing.md`. It still describes the RWX/GitLab flow deleted on 2026-09-16, so anyone following it today is being actively misled.

**Phase 4 — the in-cluster tier.** Minted as beads, not executed. See §10.

## 9. Risks

| # | Risk | Handling |
|---|---|---|
| R1 | ARC dind + service containers unproven | T1 spike; the archive's apt path is the proven fallback |
| R2 | A nightly can take 5 of 6 runner slots | Off-peak cron; T9 measures before anything is throttled |
| R3 | `coop-e2e` pixel-diff fidelity spec may flake on the runner | Run it; if it proves environment-sensitive, tag and skip it. Recorded now so it is not a surprise |
| R4 | `coop-e2e` at ~45 min × attempt cap 3 = 135 min worst case of slot time | Accepted; the alternative is no retry at all (§6) |
| R5 | The hermetic 21 silently acquire a cluster dependency | The lint in §4.3 — non-optional |
| R6 | `go-e2e`'s dependency-gated files may hard-fail rather than skip | Add skip guards, never exclude packages (§4.4) |
| R7 | Crews chase ghosts on spot-reclaimed runs | Plan-level constraint: a failure with no failed step and a "lost communication" annotation is runner loss — `gh run rerun --failed`, never a code change |
| R8 | `.github/workflows` is fimba-91's lane | Phase 1–3 only *add* `e2e.yml`; the single edit to their file (P0-a) is invited by its own comment. P0-b is escalated as a decision |

## 10. Deferred: the in-cluster tier (~150 modules)

Not in this plan. Recorded so the deferral is a decision rather than an omission. Preconditions, each of which is an operator decision:

- **A k3s e2e values overlay.** `deploy-values/` contains only `gardenio-k3s.yaml`. The chart defaults `global.storageClass: "gp2"` (`gardenio/helm/gardenio/values.yaml:2156`) and `values-rwx.yaml:56,65` pin `persistence.storageClass: gp2`. k3s has only `local-path`.
- **Release name must equal namespace.** The harness derives `BEADS_SVC=${NAMESPACE}-beads` and selects on `app.kubernetes.io/instance=${NAMESPACE}`.
- **Cluster credentials.** The runner SA is `gardenio-k3s-gha-rs-no-permission` and `kubectl auth can-i` confirms zero permissions in both `gardenio` and `arc-runners`. Candidates: a scoped SA + Role bound to a new scale-set tier; repo Actions secrets (the repo currently has none); a k8s Secret mounted via the ARS template. **Never** the IMDS→SSM admin kubeconfig (D10, `gd-adirNxx3Qo`).
- **Namespace strategy.** Ephemeral per-run vs a persistent reset-between-runs namespace. The live `gardenio` namespace is ruled out: 14 wired modules `rollout restart` / `scale` / `delete pod` against the deployed stack.
- **`test-tls-certs.sh` needs rethinking.** It is a SMOKE module requiring public HTTPS ingress, and gardenio k3s is ziti-only; it would yield zero assertions.
- **`seed-types-and-users.sh` hardcodes 4 shards** (line 96). Any other shard count needs it parameterised.
- 14 modules use `kubectl debug node` / hostPID and will need node-level permissions the scoped SA should probably not have.

## 11. Open decisions for the operator

| # | Decision | Why it is yours |
|---|---|---|
| O1 | P0-b — exempt `schedule` runs from the retry staleness guard | Changes decision logic in fimba-91's six-hour-old file (§6) |
| O2 | Create the `e2e` repo label | Nothing in the label path works without it; it does not exist today (§3) |
| O3 | The nightly cron hour | Capacity-shaped; the pool is shared with `ci`/`checks`/`images` |
| O4 | Whether Phase 4 proceeds at all, and on which credential model | §10; touches RBAC and the chart/namespace lane |

---

## Appendix A — plan-to-bead mapping

Plan epic: `type=epic`, labels `plan`, `plan:e2e-ci`, `project:gardenio`, fields `plan_state` and `phase_order`. Phase epics are `parent-child` children of the plan epic. Task beads are `parent-child` children of their **phase** epic, not the plan, and carry no `plan_id` — the plan is derived by walking task → phase → plan.

A related edge to CI epic `gd-irs5C9BEBi`, which owns CI work. A mention of `gd-ljoVmxrW8O`, whose non-goal this reverses (§2). `gd-IoVkE0k8ot` blocks T9 (the nightly enable), not the whole plan: Phases 1–2 are developed on watched branch runs where a human can rerun by hand.

One task = one branch `agent/e2e-ci/<task-bead-id>` = one PR.
