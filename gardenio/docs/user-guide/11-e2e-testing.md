# E2E Testing

End-to-end tests for the gardenio/kbeads/coop stack. The suite is large (~226
shell modules, 65 viewer Playwright specs, 8 coop specs, 7 `//go:build e2e`
Go files, 27 `//go:build integration` Go files) and **most of it does not run
in CI today**. That is a decided, documented state, not an oversight — see
"What runs in CI, and what does not" below.

## Architecture Overview

CI orchestration is [`.github/workflows/e2e.yml`](../../../.github/workflows/e2e.yml).
It replaced the RWX/GitLab dispatch pipeline that `8a3eb29e` deleted on
2026-09-16 (see "History" below) — there is no RWX CLI and no GitLab CI
dispatch job anymore.

`e2e.yml` jobs are **never a required check**. They start from four triggers
(`on:` in the workflow):

1. **A PR label.** `pull_request: types: [labeled, synchronize]` fires for any
   label, so the `select` job's `if:` re-checks that the PR carries `e2e`;
   without it nothing runs. Adding `e2e` runs all six suites against the PR,
   and later pushes to the PR re-run them.
2. **`workflow_dispatch`**, with an optional `suites` filter to run a subset.
3. **A nightly cron on `main`** — `cron: '17 9 * * *'` (09:17 UTC, an odd
   minute so it does not queue behind every other repo's on-the-hour crons).
   It is the anti-rot mechanism, so the suite can't silently bitrot behind a
   dispatch-only trigger the way it did before.
4. **`push` to `agent/e2e-ci/**` branches.** This exists so a job can be
   proven green on its branch before the workflow reaches `main`; it costs
   nothing when no such branch exists.

The workflow's concurrency group is `e2e-${{ github.ref }}-${{ github.event_name }}`;
only `pull_request` runs cancel an in-progress run in the same group.

```diagram
PR labeled `e2e` / workflow_dispatch / nightly cron / push agent/e2e-ci/**
        │
        ▼
  select (suite selection)
        │
        ├─ wiring            (select self-check)
        ├─ coop              (coopmux + fidelity Playwright)
        ├─ shell-hermetic    (run-shard.sh HERMETIC_ONLY subset)
        ├─ viewer            (beads-viewer Playwright, postgres service)
        ├─ go-integration    (-tags integration, postgres service)
        └─ go-e2e            (-tags e2e, controller packages)
```

`select` picks from the fixed list `wiring viewer coop shell-hermetic go-e2e
go-integration`. Every trigger selects all six, except a `workflow_dispatch`
with `suites` set, which keeps only the names it lists; unknown names are
dropped, so a typo selects nothing (a visible no-op) instead of running
everything.

### The `e2e.yml` jobs (six suites plus `select`)

Runner tiers are the ARC scale sets described in
[`gardenio/helm/ci/README.md`](../../helm/ci/README.md) ("Runner tiers for
the gardenio repo"): `gardenio-k3s` is the heavy tier (3 CPU / 12Gi requests,
with Docker-in-Docker, needed for `services:` containers),
`gardenio-k3s-medium` is 750m / 1Gi with no dind, and `gardenio-k3s-light` is
250m / 512Mi with no dind.

| Job | Runner | What it runs | Needs a live cluster? |
|---|---|---|---|
| `select` | light | Computes the suite list above | No |
| `wiring` | light | Asserts `select`'s suite output is a non-empty JSON array — a self-check that the triggers/guard/plumbing still work | No |
| `coop` | heavy | 8 Playwright specs against `coopmux` (mux + fidelity server), built from source | No |
| `shell-hermetic` | medium | The `HERMETIC_ONLY=1` subset of `run-shard.sh` (17 modules) | No |
| `viewer` | heavy | 65 beads-viewer Playwright specs, against a `kd serve` daemon backed by an ephemeral Postgres+pgvector service container | No (ephemeral service container, not a K8s namespace) |
| `go-integration` | heavy | `go test -tags integration` over `kbeads/internal/store/postgres/...` (26 files) against a Postgres+pgvector service container, then over `gardenio/controller/cmd/edge-auth/...` (1 file, gated on a real GitLab, so it skips by design) | No (service container) |
| `go-e2e` | medium | `go test -tags e2e` over the 4 controller packages that hold the 7 tagged files | Partially — 2 of 7 tagged tests run standalone; the other 5 skip loudly for want of a live cluster/daemon/Postgres (see "What A Skip Means" below) |

None of these jobs stand up a K8s namespace. The **live-namespace lane**
below does.

### The live-namespace lane (`live-setup` → `live-smoke` / `live-shard ×4` → `live-report` → `live-teardown`)

Added under gd-WBz2qt9ADZ (design:
[`docs/superpowers/specs/2026-10-13-e2e-live-namespace-lane-design.md`](../../../docs/superpowers/specs/2026-10-13-e2e-live-namespace-lane-design.md);
census of every module: bead gd-xad8rBcLmH). It is the first thing since
`8a3eb29e` to run `run-shard.sh` against a cluster.

| Job | Runner | What it does |
|---|---|---|
| `live-setup` | `gardenio-k3s-e2e` | Leases one of the pool namespaces `gardenio-e2e-0`/`-1` (a `Lease` object; stale ones from reclaimed runs are swept), wipes leftovers, mints a run-scoped beads token and coop key, `helm upgrade --install <ns> gardenio/helm/gardenio -f gardenio/tests/e2e/lane/values-e2e.yaml --set global.imageTag=<newest calver tag>`, runs `wait-for-stack-ready.sh` and `seed-types-and-users.sh` |
| `live-smoke` | `gardenio-k3s-e2e` | `SMOKE_ONLY=1 ONLY=stack-health,agent-crew-lifecycle,beads-crud,mail-ready,trigger-engine-smoke run-shard.sh` (SMOKE_MODULES minus `tls-certs`, which needs cert-manager) |
| `live-shard 0..3` | `gardenio-k3s-e2e` | `E2E_SHARD=n TOTAL_SHARDS=4 SKIP=<HERMETIC_MODULES> run-shard.sh` — the `shard_for()` partition, minus the modules the shell-hermetic job already ran. Dispatch-only (`suites=live-shards`) until the shards PR promotes it into the default set |
| `live-report` | light | Merges every shard's `e2e-results.jsonl` into one pass/fail/skip table in the step summary; red if any module failed. A known-failing module stays listed with its own bug bead, never hidden behind a skip |
| `live-teardown` | `gardenio-k3s-e2e` | `if: always()`: captures pod logs, `helm uninstall`, deletes everything the run created (including agent pods the controller spawned), releases the lease |

`gardenio-k3s-e2e` is a fourth ARC scale set with the medium tier's shape and
the `gardenio-e2e-runner` ServiceAccount, which holds **one namespaced Role per
pool namespace and nothing cluster-scoped** (operator decision O4). What it
needs from the operator, and why, is in
[`gardenio/tests/e2e/lane/README.md`](../../tests/e2e/lane/README.md).

What runs there and what does not, per the census: 138 modules need only the
namespace and run; 34 need an external system (Slack app, GitLab, Jira,
LiteLLM key, Grafana, Keycloak, cert-manager, VictoriaLogs) and self-skip
loudly (a few may fail and get bug beads after the first run); 18 are parked
behind `GVISOR_E2E`/`MICROVM_SUBSTRATE_E2E` because that substrate does not
exist on the k3s cluster; 1 is dead (`gitlab-jwt-e2e`). The deployed daemon is
the newest calver release while the test scripts and `kd`/`gb` are HEAD;
`run-shard.sh`'s context banner prints that skew on every shard.

```bash
gh workflow run e2e.yml -f suites=live-smoke                    # the lane on five modules
gh workflow run e2e.yml -f suites=live-shards                   # all four shards
gh workflow run e2e.yml -f suites=live-shards -f image_tag=2026.286.7
```

`go-integration` has no `continue-on-error` at job or step level (removed
under gd-Ai6ekNov28 once its red set was triaged), so a failing kbeads
integration test fails the job. The controller step uses `if: ${{ !cancelled() }}`
so it still runs, and its skip stays visible, after a kbeads failure. A
`failure census` step lists every `--- FAIL`/`--- SKIP` line in the job
summary, and the logs upload as the `go-integration-log` artifact.

### Cluster & Namespace (manual, in-cluster runs only)

None of the above needs one. The much larger wired-but-not-hermetic set
(below) does, when run manually against a real deployment:

- **Namespace**: `gardenio-e2e` by default (`run-shard.sh`'s `NAMESPACE`
  default), overridable via `NAMESPACE`. The CI lane sets it to the leased pool
  namespace (`gardenio-e2e-0`/`-1`)
- **Beads service**: `<namespace>-beads` (port 8080, forwarded to localhost:19090)
- **Auth**: Token loaded from K8s secret `kd-beads-token`

## What Exists

Counted 2026-10-11 against the tree (re-count before trusting these — the
suite moves):

- **226** `test-*.sh` module files in `gardenio/tests/e2e/scripts/` (plus
  `gardenio/tests/e2e/edge-auth/test-edge-auth.sh`, which is wired
  separately), of which:
  - **209** are referenced by a `run_module` call in `run-shard.sh`
  - **17** names are in `HERMETIC_MODULES` (all wired; one of them,
    `outbox-rules-json`, runs `verify-outbox-rules-json.sh`, which is not a
    `test-*.sh` file)
- **65** beads-viewer Playwright specs (`gardenio/tests/beads-viewer-e2e/tests`)
- **8** coop Playwright specs (`coop/tests/e2e/specs`)
- **7** `//go:build e2e` Go files, all in `gardenio/controller`
- **27** `//go:build integration` Go files (26 in `kbeads/internal/store/postgres`,
  1 in `gardenio/controller/cmd/edge-auth` — gated on a real GitLab, skips
  permanently)
- `gardenio/tests/slack-e2e/` — real-user Slack E2E, **permanently manual**
  (see `gardenio/CLAUDE.md`'s E2E section); not part of the `e2e.yml` suite
  vocabulary at all

## What Runs In CI, and What Does Not

The six `e2e.yml` jobs above are it. In particular: **the ~192 wired
modules outside `HERMETIC_MODULES` (209 − 17) need a live cluster and do not
run in CI** — and as of 2026-09-22 this is **not in CI by decision, not
merely "not yet."** (The decision record is
[`docs/superpowers/plans/2026-09-21-e2e-optional-ci-jobs.md`](../../../docs/superpowers/plans/2026-09-21-e2e-optional-ci-jobs.md),
operator decision O4.)

> A Phase 4 running the in-cluster suite is **approved in principle**
> (operator decision O4, 2026-09-22): it would run on a **dedicated
> in-cluster `needs_k8s_api` role**, not on the CI runners' credentials. Two
> things gate it, deliberately. First, **a census spike comes before any
> credential** — roughly $1 of agent time to run the wired in-cluster set
> against a k3s-deployed gardenio and record, per module, pass / fail /
> needs-substrate-that-no-longer-exists. Nobody should mint a cluster
> credential for a suite whose pass rate is unknown. Second, the credential
> half is **gated on `gd-adirNxx3Qo`** (every pod can assume the node role):
> until that is remediated, a scoped ServiceAccount is documentation, not a
> boundary. (That decision bead is now closed; check what it decided before
> treating the gate as lifted.)

Two corrections to the preconditions as previously written here, both
verified 2026-09-22:

- **"No registry exists" is false for gardenio.**
  `ghcr.io/joyousaversion/gardenio/agent` is published with both `:latest`
  and `:<sha>`, alongside the rest of the image matrix, with an ECR
  pull-through cache in front (`deploy-values/gardenio-k3s.yaml`). That
  blocker is a Gas City finding that does not transfer. The real constraint:
  [`images.yml`](../../../.github/workflows/images.yml) **no longer runs on
  ordinary pushes to `main`** — `branches: [main]` was removed in `df984a49`
  on 2026-09-21 — so `:latest`/`:<sha>` arrive only on calver tag pushes
  (`tags: ['20*']`) and manual `workflow_dispatch`. A Phase 4 cannot assume
  an image for an arbitrary `main` sha exists.
- **"A k3s values overlay is missing" is stale; "a k3s *e2e* values overlay
  is missing" is still true.** `deploy-values/gardenio-k3s.yaml` exists, is
  tracked, and sets `global.storageClass: local-path`. It is the
  *production* overlay — `git ls-files deploy-values/` returns exactly that
  one file. There is no e2e-specific overlay anywhere in the repo, and
  nobody has shown the production one is fit for e2e use.

## The Three Doors

```bash
gh pr edit <n> --add-label e2e             # label a PR
gh workflow run e2e.yml                    # everything
gh workflow run e2e.yml -f suites=viewer   # one suite
```

Suite names: `wiring`, `viewer`, `coop`, `shell-hermetic`, `go-e2e`,
`go-integration`, `landgate`, `live-smoke`, and (dispatch-only for now) `live-shards`. The
nightly cron runs the default set on `main`.

## Running Tests Locally

### Hermetic subset (no cluster, no daemon)

```bash
# From the repo root
HERMETIC_ONLY=1 ./gardenio/tests/e2e/scripts/run-shard.sh
```

This is exactly what the `shell-hermetic` CI job runs: no `kubectl`, no
`kd`/`gb` daemon calls, no port-forwards — pure `go test` over fake
clientsets and `httptest` servers, local probe binaries, and static linters.
`gb`/`kd` still need to be built and on `PATH` for the driver's own banner
and several module preludes, even though the lint below guarantees no
hermetic module actually calls them.

### Other named subsets (need a live cluster/namespace)

- `SMOKE_ONLY=1` — the 6-module smoke gating subset (`tls-certs`,
  `stack-health`, `agent-crew-lifecycle`, `beads-crud`, `mail-ready`,
  `trigger-engine-smoke`). Needs a reachable namespace, beads daemon and
  port-forwards — unlike the hermetic set, `run-shard.sh` does **not** skip
  cluster preflight for this mode.
- `MIGRATION_ACCEPTANCE=1` — the cutover gate (`stack-health`, `tls-certs`,
  `migration-postauth-viewer`, `migration-coop-reachable`,
  `migration-agent-opens-mr`). Same cluster/daemon requirement as
  `SMOKE_ONLY`, run single-shard, intended against a real (often
  production-track) deployment.

### Filtering

- `ONLY=mod1,mod2` — run only the named modules (intersected with any active
  subset mode above)
- `SKIP=mod1,mod2` — skip the named modules

### Output

Each shard writes to `junit-results-<E2E_SHARD>/` under the working
directory: `e2e-results.jsonl` (one line per module: name, status, duration,
skip/fail reason) and a generated `e2e-summary.xml` (JUnit). The
`shell-hermetic` CI job uploads that directory as the `shell-hermetic-junit`
artifact and also echoes a plain-text table into the job's step summary.

## The Hermetic Contract

"Hermetic" here means a module runs to completion with **no kubectl, no
`kd`, no `gb`, no `helm`, no `terraform`, and no reference to the
port-forward ports 19090/19091**. It may build and run Go, start `httptest`
servers, bind `127.0.0.1`, and use `python3`.

The list lives in `run-shard.sh`'s `HERMETIC_MODULES` and is linted by
[`test-hermetic-set-lint.sh`](../../tests/e2e/scripts/test-hermetic-set-lint.sh),
which the `shell-hermetic` job runs as its first step (`<1s`) — before the
subset itself — specifically so a module that has acquired a cluster
dependency fails loudly, by name, instead of shrinking the set quietly to
make a red go away.

## What A Skip Means

A green `go-e2e` job does not mean all seven `-tags e2e` files ran. These
are the 5 (of 7) tagged files that skip in CI, by name and reason (each
`t.Skip` is in the named file):

| File | Skip reason |
|---|---|
| `stranded_recovered_livecluster_e2e_test.go` | No cluster (RBAC forbids namespace creation on the ARC runner SA) |
| `gardener_start_argv_emit_e2e_test.go` | `GARDENER_EMIT_BEAD_ID` unset |
| `thread_task_autoclose_livedaemon_e2e_test.go` | No live daemon |
| `trigger_changed_to_seam_livedaemon_e2e_test.go` | No live daemon (short-circuits first) |
| `twotenant_e2e_test.go` (cmd/landgate) | No Postgres (the `landgate` job runs it) |

Only `subagentstop_e2e_test.go` and `hooks_natsrelay_e2e_test.go` actually
run in CI today (the job builds `gb` first, because both skip without it).
Other tests in the four packages can also skip. The job's `if: always()`
"skip census" step lists every `--- SKIP` line from the `go test -v` log in
the job summary on every run — read that, not the badge, before assuming
"the job is green" means "all seven ran."

## Spot Reclaims

The CI runner pool is a spot node pool. A job with **no failed step**, whose
annotations say something like "lost communication with the server," is a
reclaim, not a real failure —
[`retry-lost-runners.yml`](../../../.github/workflows/retry-lost-runners.yml)
detects both the abrupt (dead heartbeat) and graceful (SIGTERM'd runner pod)
signatures and re-runs the job automatically, up to an attempt cap. **Never
change code in response to one** — there is nothing to fix; the annotation
is the whole diagnosis.

## History

The RWX pipeline (`.rwx/e2e.yml`, driven by a GitLab CI `e2e-dispatch` job)
was deleted in `8a3eb29e` on 2026-09-16: the RWX account, token, and target
cluster no longer exist. The pipeline specs are preserved as a design record
at [`docs/archive/rwx/root/e2e.yml`](../../../docs/archive/rwx/root/e2e.yml)
and [`docs/archive/rwx/root/ci.yml`](../../../docs/archive/rwx/root/ci.yml).
The replacement design is
[`docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md`](../../../docs/superpowers/specs/2026-09-21-e2e-optional-ci-jobs-design.md).

## Adding New Test Modules

### 1. Create the script

Create `tests/e2e/scripts/test-<name>.sh` following the standard pattern:

```bash
#!/usr/bin/env bash
# test-<name>.sh — <description>
#
# Scenarios tested:
#   1. <scenario 1>
#   2. <scenario 2>

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────
NAMESPACE="${NAMESPACE:-gardenio-e2e}"
BEADS_SVC="${BEADS_SVC:-${NAMESPACE}-beads}"
KD_BIN="${KD_BIN:-kd}"
GB_BIN="${GB_BIN:-gb}"
PASS=0
FAIL=0
ERRORS=()
CREATED_BEADS=()

export KD_ACTOR="${KD_ACTOR:-e2e-test}"

# ── Helpers ───────────────────────────────────────────────────────
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
blue()  { printf '\033[0;34m%s\033[0m\n' "$*"; }
dim()   { printf '\033[2m%s\033[0m\n' "$*"; }

pass() {
  local name="$1"
  green "  ✓ $name"
  PASS=$((PASS + 1))
}

fail() {
  local name="$1"
  local reason="${2:-}"
  red "  ✗ $name"
  [ -n "$reason" ] && dim "    $reason"
  FAIL=$((FAIL + 1))
  ERRORS+=("$name: $reason")
}

# ── Cleanup ───────────────────────────────────────────────────────
cleanup() {
  for id in "${CREATED_BEADS[@]}"; do
    "$KD_BIN" close "$id" 2>/dev/null || true
  done
}
trap cleanup EXIT

# ── Tests ─────────────────────────────────────────────────────────
blue "=== <name> E2E Tests ==="

# ... your test assertions ...

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "Results: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  red "Failed:"
  for e in "${ERRORS[@]}"; do red "  - $e"; done
  exit 1
fi
```

### 2. Wire it in (optional)

A new script is not automatically part of any run. Two independent choices:

- **Add it to `run-shard.sh`** so `ONLY=`/`SKIP=`/sharded runs pick it up:
  add a `run_module "<name>" "./gardenio/tests/e2e/scripts/test-<name>.sh"`
  call in the appropriate section.
- **If it needs no cluster, no daemon, and no `kubectl`/`kd`/`gb` call**,
  also add its name to `HERMETIC_MODULES` so it runs in CI via the
  `shell-hermetic` job. `test-hermetic-set-lint.sh` will fail the build if a
  listed module later grows a cluster dependency it didn't have when added.

Neither step is required — a script can exist unwired (17 `test-*.sh` files
currently are, e.g. the `gvisor-*` and `lib-*` scripts, each dispatched or
invoked out-of-band by another harness or by a lint in `checks.yml` — see the
exclusion list at the top of `run-shard.sh`), but an unwired script does not
run in `e2e.yml`.

### 3. Clean up after yourself

Every test should clean up beads it creates (use `CREATED_BEADS` array +
cleanup trap). Tests sharing a namespace can flake each other on leftover
state.

## Agent Verification Checklist

After deploying a new gardenio release, verify agent functionality manually:

- [ ] Pod starts and reaches `Running` state
- [ ] `coop` session initializes (check logs for session registration)
- [ ] Claude Code agent connects (look for "session started" in coop logs)
- [ ] `gb prime` outputs valid context (role, project, advice)
- [ ] `gb ready` returns beads from the correct project
- [ ] `kd claim <id>` successfully claims a bead
- [ ] Agent can read/write files in the workspace
- [ ] Git operations work (clone, commit, push)
- [ ] Stop gate fires on session end (`gb bus emit --hook=Stop`)
- [ ] Decision checkpoints post to Slack and block via `gb yield`
- [ ] Session resume works after pod restart (`coop --resume`)

Quick smoke test commands:

```bash
# Check pod status
kubectl get pods -n gardenio -l app=agent

# Stream agent logs
kubectl logs -f -n gardenio <agent-pod>

# Check controller logs for reconciler
kubectl logs -f -n gardenio -l app=controller | grep reconcile

# Verify coop session
kubectl exec -n gardenio <agent-pod> -- coop status
```

## Debugging Failed Agent Spawns

When an agent pod fails to start or dies early:

### 1. Check pod events

```bash
kubectl describe pod <pod-name> -n gardenio
```

Look for: `ImagePullBackOff` (wrong image tag), `CrashLoopBackOff` (entrypoint
crash), `OOMKilled` (memory limit), `CreateContainerConfigError` (missing
secrets or config maps).

### 2. Check init container logs

Agent pods have init containers for git clone and credential setup:

```bash
kubectl logs <pod-name> -n gardenio -c init-git
kubectl logs <pod-name> -n gardenio -c init-creds
```

### 3. Check controller logs

The controller reconciler creates pods. Look for reconciliation errors:

```bash
kubectl logs -f -n gardenio -l app=controller | grep -i error
```

### 4. Check beads daemon connectivity

```bash
# Port-forward
kubectl -n gardenio port-forward svc/<namespace>-beads 19090:8080

# Health check
curl http://localhost:19090/healthz

# List agent beads
BEADS_HTTP_URL=http://localhost:19090 kd list --type=agent --status=open
```

### 5. Check coop/coopmux logs

Session registration and credential broker issues:

```bash
kubectl logs -f -n gardenio -l app=coopmux
```

## Common Failure Modes

| Symptom | Likely cause | Fix |
|---|---|---|
| `ImagePullBackOff` | Image tag doesn't exist in the registry | Images publish to `ghcr.io/mattbakerconsulting/gardenio/<component>` (via an ECR pull-through cache in prod) only on a calver tag push or manual `images.yml` dispatch — an ordinary `main` merge does not push `:latest`/`:<sha>`. Verify a release tag exists for the version in question. |
| `CrashLoopBackOff` on init | Git clone failed (bad token or URL) | Check project bead `git_url` and per-project secrets |
| Agent starts then dies immediately | Missing `BEADS_HTTP_ADDR` or NATS | Verify controller and NATS are running in namespace |
| `gb ready` returns empty | Wrong project label or no open beads | Check `BOAT_PROJECT` env var matches bead labels |
| Stop gate blocks indefinitely | Open decision bead not resolved | `gb decision list` to find open decisions; resolve via Slack |
| Test module times out | Port-forward dropped | Re-establish: `kubectl -n <ns> port-forward svc/<ns>-beads 19090:8080` |
| `kd` auth failure | Missing or expired token | Re-load: `kubectl -n <ns> get secret kd-beads-token -o jsonpath='{.data.token}' \| base64 -d` |
| Config beads not resolving | Type config not seeded | Seed via `curl -X PUT /v1/configs/type:<type>` |
| Claudeless test flakes | `claudeless` binary not in PATH | Verify agent image includes claudeless; check `which claudeless` |
| CI job shows "lost communication with the server" with no failed step | Spot-instance reclaim, not a real failure | Nothing to fix — `retry-lost-runners.yml` auto-retries it. See "Spot Reclaims" above. |
