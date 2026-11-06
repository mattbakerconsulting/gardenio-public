# Ephemeral rwx smoketest instances — plan of record

**Status:** Draft (research/design — no implementation until operator approval)
**Umbrella epic:** TBD (filled in once umbrella bead is created)
**Gardener:** scheduled `gardener-ephemeral-rwx-smoke` (every 30m)
**Source:** Slack thread spawning `ka-0000006532` (Matt Baker, 2026-04-29) — "all MRs fighting over the same rwx smoketest instance"

## Problem

Today there is exactly **one** rwx smoketest instance, deployed in the EKS namespace
`gardenio-rwx`. It is shared by:

- **Main-push CI**: `rwx-deploy-rwx` → `rwx-smoke` (5-module subset). On every push to `main`.
- **Release tag CI**: `rwx-deploy-rwx` → `rwx-e2e` (full suite). On every release tag.
- **Ad-hoc dispatches**: humans / agents running `rwx dispatch gardenio-deploy-rwx`.
- **Pre-merge MR CI** (planned / partially live): smoke against `gardenio-rwx` for in-flight MRs.

The current concurrency-control mechanism (`gardenio/docs/release-pipeline.md` §
"gardenio-rwx Deploy Lock Runbook") is a **15-minute ConfigMap mutex**
(`gardenio-deploy-lock` in `gardenio-rwx`). Concurrent deploys queue behind one
another or steal stale locks; concurrent **smoke runs** are not gated at all and
race the deploy out from under each other.

Symptoms reported in the spawning thread (and seen in past incidents):

1. Two MRs land on `main` near-simultaneously → second MR's smoke runs against
   the first MR's image (or vice versa) — the test result is meaningless.
2. A long deploy holds the lock; a fresh push waits, then deploys mid-smoke,
   flipping the controller image while smoke is asserting against the old one.
3. `FORCE_DEPLOY=1` (P0 hotfix path) bypasses the lock entirely and is
   guaranteed to corrupt any in-flight smoke.
4. Bug-hunting agents who want to dispatch a one-off smoke against their branch
   either (a) take the lock and starve `main` CI, or (b) wait indefinitely.

**The fix is to give every smoke run its own instance**: deploy → test → tear
down, with no shared state. This eliminates the lock, FORCE_DEPLOY, and the
"who's the current image" question entirely.

## Goal

Every smoke / E2E / pre-merge dispatch deploys a **fresh ephemeral instance**
(its own namespace + Helm release + isolated dependencies) keyed by an
`instance-id` derived from the commit SHA / MR IID / dispatch ID. The instance
runs the test suite, then is torn down. Concurrent smoke runs have no
interaction.

Production deploy (the `gardenio` namespace, target=prod) remains a singleton
and is **out of scope**. This plan only touches staging-shaped instances.

## Non-goals

- Removing the production singleton (`gardenio` namespace) — it must stay shared.
- Ephemeralizing the **agent base image** build pipeline — already parallelized
  per task; no contention.
- Per-PR preview environments for human review — different problem,
  different cost profile. (Could be a follow-up once ephemerals are stable.)
- Migrating beads-viewer / beads3d / coopmux test surfaces beyond what they
  share with the gardenio smoke. Those do not currently use the `gardenio-rwx`
  namespace.

## Architecture sketch (pre-decision)

### Instance ID

Resolved by D6 (ka-0000005792, 2026-05-05): always `CI_COMMIT_SHORT_SHA` from
GitLab CI for pipeline-driven dispatches. One uniform code path across MR
pipelines, detached pipelines, main-push pipelines, and release-tag pipelines
— no MR-vs-non-MR branching in the dispatch chain. Ad-hoc human/agent
dispatches pass an explicit `instance-id` param.

| Dispatch source | `instance-id` |
|---|---|
| GitLab CI (any pipeline kind) | `$CI_COMMIT_SHORT_SHA` (8 hex chars) |
| Ad-hoc human / agent | explicit `--param instance-id=<short-uid>` |

Constraint: ≤ 30 chars total when prefixed (`gardenio-rwx-<id>`) so it fits a
K8s namespace label (max 63) with room for chart-suffixed resources.
`CI_COMMIT_SHORT_SHA` is 8 chars → `gardenio-rwx-<8>` = 20 chars, well under
the limit.

Trade-off accepted: when an MR is rebased/force-pushed, the new short-SHA
yields a fresh ephemeral; the prior one is reaped on its normal 90m timeout
(or by the cost-cap eviction in D4). MR IID is not encoded; cross-referencing
an MR ↔ ephemeral happens via the GitLab pipeline UI, not the namespace name.

### Helm release / namespace shape

- One Helm release per instance, name = `gardenio-rwx-<id>`, namespace = same.
- All in-cluster names (services, deployments, configmaps, secrets, pods)
  carry the release name as a prefix — the chart already does this where
  resource names use `{{ include "gardenio.fullname" . }}`. Anywhere a
  hard-coded `gardenio-rwx` literal exists today (deploy values, smoke scripts,
  E2E scripts) gets templated against the release name / namespace.
- Per-instance overlays: `deploy-values/gardenio-rwx-ephemeral.yaml.tmpl`
  rendered at dispatch time with `instance-id` substituted.

### External dependencies

These are the load-bearing decisions. Each will be its own design decision in
the gardener's first cycle:

| Dependency | Today | Options |
|---|---|---|
| Postgres (beads DB) | Shared RDS, per-namespace database with scoped role | **DECIDED (D2, ka-0000005090, 2026-05-05): (a) per-instance database on shared RDS** — cheapest, ~10s provision, scoped role per ephemeral. (b) in-cluster pod was rejected as slower; (c) shared-DB-with-suffix breaks pgvector. Implementation lives in E3 (ka-0000006535): provisioning script (CREATE DATABASE + scoped role + GRANT), pgvector extension creation per DB, teardown drops DB + role. |
| S3 (sessions, archives) | Shared bucket, per-namespace prefix via IRSA | Per-instance prefix `s3://gardenio-sessions/<id>/...` (cheap, fast) |
| Secrets (Slack, AWS, Mezmo, LiteLLM) | Shared `ExternalSecret` references | Reuse same secrets across ephemerals (read-only); isolate test-channel writes via a per-instance Slack channel ID derived from instance-id |
| Slack channel for E2E posts | Shared `#gardenio-rwx-tests` | Per-instance threaded posts under one parent message, OR per-instance child channel auto-created via Slack API |
| DNS / ingress | `*.gardenio-rwx.e2e.example.com` | `<id>.gardenio-rwx.e2e.example.com`. Wildcard cert already covers. |
| LiteLLM (model proxy) | Shared, with per-instance virtual-key pool | Reuse — virtual keys are already per-account, not per-instance |

Default position: **share the infra, isolate the data path**. Ephemerals need
to be cheap; spinning a new RDS per smoke run is a non-starter.

### Dispatch / lifecycle

```text
GitLab CI / human / agent
  └── rwx dispatch gardenio-deploy-rwx --param instance-id=<id> --param commit-sha=<sha>
      └── deploy-rwx.yml (per-instance lock, NOT the global one)
            ├── helm upgrade --install gardenio-rwx-<id> -n gardenio-rwx-<id>
            ├── wait for rollout
            └── emit instance-id back to GitLab job artifact
  └── rwx dispatch gardenio-smoke-rwx --param instance-id=<id> --param commit-sha=<sha>
      └── smoke-rwx.yml
            ├── target NAMESPACE=gardenio-rwx-<id>
            ├── run smoke (5 modules) or e2e (full) — same scripts, parameterized
            └── on success or fail: dispatch teardown
  └── rwx dispatch gardenio-teardown-rwx --param instance-id=<id>
        └── helm uninstall + namespace delete + Slack channel cleanup
```

### Reaper

A scheduled trigger runs every 30m and tears down any `gardenio-rwx-*`
namespace whose `creationTimestamp` is > 90 minutes old (longer than the
longest E2E run + safety margin) and is not labeled `lifecycle=keep`. Catches
crashed-mid-flight instances.

The reaper also enforces the **concurrency cost cap** (max=20, configurable
via Helm value; resolved by D4 / ka-0000004069). When the count of live
`gardenio-rwx-*` namespaces exceeds the cap, the reaper evicts the oldest
unlabeled instances (regardless of age) until the count is back at the cap.
`lifecycle=keep` namespaces are exempt and do not count against the cap.

## Workstream decomposition (proposed child epics)

Each becomes its own `kd-` epic under the umbrella. Order matters — the early
ones unblock the later ones.

| # | Epic | Drives |
|---|---|---|
| **E1** | Helm chart parameterization for ephemeral release names | Audit and template every `gardenio-rwx`-literal in `helm/gardenio/`, deploy-values/, scripts/. Verify `helm install gardenio-rwx-foo -n gardenio-rwx-foo` works against a scratch cluster. |
| **E2** | RWX deploy/smoke/e2e dispatch chain with `instance-id` | Add `instance-id` param to `deploy-rwx.yml`, `smoke-rwx.yml`, `e2e.yml`. Pass through from GitLab CI. Replace the global ConfigMap lock with per-instance no-op (each instance is its own lock domain). |
| **E3** | External-dependency isolation (DB, S3, Slack, ingress) | Decision per dependency. Implement per-instance DB names (likely a script + SQL grants), S3 prefix templating, Slack thread/child-channel strategy, ingress wildcard host-name templating. |
| **E4** | Teardown + reaper | `teardown-rwx.yml` workflow + `gardenio-rwx-reaper` cron schedule (every 30m, deletes `gardenio-rwx-*` namespaces older than 90m without `keep` label). |
| **E5** | E2E / smoke script parameterization | `tests/e2e/scripts/run-shard.sh` and friends already accept `NAMESPACE=` env — audit that every script honors it; remove any remaining `gardenio-rwx` hardcoding. Add a smoke run that proves two ephemerals can run concurrently without interfering. |
| **E6** | Cutover + doc rewrite | Run new path in shadow mode (deploy ephemeral, also deploy shared, compare results). Once green for N days, cut over `main`-push CI, then release-tag CI. Delete `gardenio-deploy-lock` runbook. Delete `FORCE_DEPLOY` (no longer needed once ephemerals are routine). Update `release-pipeline.md` end-to-end. |

The gardener creates these as it gets operator approval — see "Approvals" below.

### E5 acceptance — script parameterization (ka-0000002597)

Implemented 2026-05-05.

- **Audit findings.** Every test script under `gardenio/tests/e2e/scripts/`
  declares `NAMESPACE="${NAMESPACE:-<dev-default>}"` and uses `kubectl -n
  "$NAMESPACE"` and `${NAMESPACE}-{beads,slack-bridge,…}` for service
  references. The only residual `gardenio-rwx` hardcodings are (a)
  back-compat fallback defaults — left in place per plan note — and (b)
  human-readable comment strings. No script's actual logic targets the
  literal namespace.
- **Dispatch helper fix.** `tests/e2e/scripts/e2e-dispatch-and-check.sh`
  used to default `NAMESPACE=gardenio-rwx` and pass it as a dispatch param,
  bypassing the post-E2 instance-id derivation. Now accepts `--instance-id
  <id>` (forwards `instance-id=<id>` to the rwx dispatch chain), keeps
  `--namespace` as a legacy override, and rejects passing both at once.
  Empty defaults let `.rwx/e2e.yml`'s own fallback (short-SHA derivation)
  take effect, matching what GitLab CI does.
- **Hermetic concurrency lint.** `tests/e2e/scripts/test-concurrency-isolation.sh`
  audits all sibling scripts for namespace-hardcoding regressions: no
  `kubectl/helm` call may pin `gardenio-rwx` as a namespace arg, every
  `kubectl`-using script must declare `NAMESPACE`, fallback defaults must
  point at known dev namespaces (`gardenio-rwx` or `gardenio-e2e`), and no
  service reference may hardcode `gardenio-rwx-{beads,slack-bridge,postgres,nats}`.
  Pure shell + grep — never invokes kubectl/helm/network.
- **Concurrent-ephemeral repro recipe** (run from a developer shell with
  `RWX_ACCESS_TOKEN` set; replaces a CI test since two parallel deploy-rwx
  runs cost ~2× the EKS/RDS footprint and the lint above + the existing
  per-instance teardown reaper give us equivalent assurance offline):

  ```sh
  # Pick two distinct instance IDs that are not currently deployed.
  ID_A=concur-a
  ID_B=concur-b
  COMMIT_SHA=$(git rev-parse HEAD)

  # Deploy both ephemerals in parallel.
  rwx dispatch gardenio-deploy-rwx \
    --param commit-sha=$COMMIT_SHA --param instance-id=$ID_A &
  rwx dispatch gardenio-deploy-rwx \
    --param commit-sha=$COMMIT_SHA --param instance-id=$ID_B &
  wait

  # Smoke both in parallel.
  rwx dispatch gardenio-smoke-rwx \
    --param commit-sha=$COMMIT_SHA --param instance-id=$ID_A &
  rwx dispatch gardenio-smoke-rwx \
    --param commit-sha=$COMMIT_SHA --param instance-id=$ID_B &
  wait
  ```

  Verifies: each smoke run targets `gardenio-rwx-<id>`, port-forwards never
  collide (each shard reserves its own random localhost port), and beads
  state stays per-instance because the daemons live in separate
  namespaces. After the runs complete, the E4 teardown reaper deletes both
  ephemerals on the next 30m tick.

## Open questions (raise as decisions on cycle 1)

1. ~~**Per-instance DB strategy**~~ — RESOLVED 2026-05-05 by D2 (ka-0000005090):
   per-instance database on shared RDS, scoped role per ephemeral. See
   §External dependencies row above.
2. **Slack channel isolation**: parent thread + per-instance threaded replies
   (favored — no API-side channel mgmt) vs per-instance child channel.
3. ~~**Cost cap**~~ — RESOLVED 2026-05-05 by D4 (ka-0000004069): **max=20**
   concurrent `gardenio-rwx-*` ephemerals; reaper evicts oldest beyond cap.
   Configurable via Helm value (default 20). Operator chose the higher cap
   over the proposed 10 to absorb release-window bursts without queueing,
   accepting the additional EKS+RDS spend. E4 implements enforcement.
4. ~~**Ingress wildcard cert scope**~~ — RESOLVED 2026-05-05 by D5
   (ka-0000005757): **existing-wildcard**. The existing
   `*.gardenio-rwx.e2e.example.com` wildcard cert covers all ephemeral
   hostnames; E3 ingress simply attaches to it. No per-instance cert
   issuance, no new wildcard provisioning, no cert-manager ACME path.
   E3 verification checklist: (a) Helm `Ingress` template emits
   `host: <id>.gardenio-rwx.e2e.example.com`; (b) `tls:` block
   references the existing wildcard secret (no per-instance `tls`
   section); (c) first ephemeral deploy `curl -kI` returns a cert chain
   whose SAN matches `*.gardenio-rwx.e2e.example.com`; (d) no
   cert-manager `Certificate` CRDs are created per instance.
5. ~~**MR-IID source for pre-merge dispatches**~~ — RESOLVED 2026-05-05 by D6
   (ka-0000005792): **always use `CI_COMMIT_SHORT_SHA`** as the instance-id
   source — uniform across MR pipelines, detached pipelines, and main-push
   pipelines. Operator chose `sha-always` over the proposed iid-with-sha
   fallback for simplicity (one code path; no MR-vs-non-MR branching in the
   dispatch chain). E2 implements: `instance-id=$CI_COMMIT_SHORT_SHA` in
   GitLab CI passes through to `deploy-rwx.yml` / `smoke-rwx.yml` / `e2e.yml`,
   yielding namespaces named `gardenio-rwx-<short-sha>`.
6. **Does rwx-smoke still gate `main`-push merges, or only release tags?** If
   `main`-push merges no longer block on smoke (because smoke happens in MR
   CI), the cutover order changes.

## Approvals required before any code lands

- **Operator approval of this plan-of-record** (D1).
- **Per-epic operator approval** at each E1–E6 entry point. The gardener files
  a `gb decision` with concrete options before opening MRs.
- **Cutover to production main-push CI**: separate decision after E5 is green
  in shadow mode for ≥ 3 days.

## Gardener operating contract

- Cadence: every 30m (`13,43 * * * *`). Pinned to the umbrella epic.
- Behavior:
  - **Cycle 1**: file decision D1 (approve / revise / defer this plan).
  - **Subsequent cycles**: drive open child epics — spawn crew on unblocked
    ones, watch MRs, gate cutover decisions on prior epics being closed.
  - **Self-completion**: when umbrella epic closes, the gardener disables its
    own schedule.
- Crew dispatch role: `crew` (or `monorepo-crew` if any work crosses repo
  boundaries — not anticipated; everything lives in this monorepo).
- Hard rule: no MR auto-merge; every MR awaits human review.

## Source

- Slack thread, 2026-04-29: Matt Baker — "all MRs fighting over the same rwx
  smoketest instance? we should make smoketest instances ephemeral; this
  deserves a new epic of epics and a gardener to drive it; make it so".
- Tracking task: `ka-0000006532`.
