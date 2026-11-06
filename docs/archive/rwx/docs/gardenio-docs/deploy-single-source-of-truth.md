# Prod gardenio deploy — single source of truth for bridge Jira values

> **Superseded (2026-10-27)** by [`releases-design.md`](../../../../../gardenio/docs/releases-design.md) (gd-hls6BkIEiO). The EKS deploy path it describes (`.rwx/deploy.yml`, `deploy-values/gardenio.yaml`) is gone; a release's deployment row is the deploy record now. Kept for history.

**Bead:** ka-0000000149 (Phase 2.3 of the JIRA cutover ka-0000000900 /
ka-0000003475; precondition #3 of ka-0000008197)

> **POST-P4.4 UPDATE (2026-07-05, ka-0000006191):** the prod stack has migrated
> to namespace `gardenio-system` on `prod-gardenio-eks` (acct `123456789012`), and
> the live prod deploy is now a targeted `terraform apply -target=module.gardenio`
> of `composition/canonical-prod` (`prod-real.tfvars`) — NOT `helm upgrade`.
> `deploy-values/gardenio.yaml` is no longer the live prod values source; the
> "Path A" mechanism described below is superseded. What REMAINS live from this
> doc is the `assert-jira-single-source` grep guard in `.rwx/deploy.yml`, which
> still greps `deploy-values/gardenio.yaml` for the bridge Jira identity. Whether
> that grep guard is still meaningful under the terraform deploy is an open
> follow-up (ka-0000000678). The account/cluster/namespace details in this
> doc's body below refer to the pre-cutover (909 america-e2e) topology.

## TL;DR

The prod `gardenio` helm release (historically: namespace `gardenio`, cluster
`america-e2e-eks`, account `234567890123` — see post-P4.4 banner above) has
historically been reachable
by **two** deploy paths that both run
`helm upgrade --install gardenio -n gardenio` with **different values files**.
Whichever ran last won — so a routine deploy down the wrong path could
silently revert the bridge Jira identity (and the
`JIRA_OUTBOX_ENABLED` / `JIRA_FIELD_ALLOWLIST_MODE` cutover toggles).

This doc pins **exactly one** authoritative path and values file, and a CI
guard (`.rwx/deploy.yml → assert-jira-single-source`) fails the prod deploy
fast if the bridge Jira identity ever drifts from that file.

## The two paths

### Path A — AUTHORITATIVE (in-repo RWX)

- **Config:** `.rwx/deploy.yml`, task `deploy-prod` (this repo).
- **Chart:** `gardenio/helm/gardenio/` (in-repo; header: "No external
  acme-helm-chart dependency needed").
- **Values:** `deploy-values/gardenio.yaml`  ← **the single source of truth.**
- **Invocation:** `helm upgrade --install gardenio gardenio/helm/gardenio
  --values deploy-values/gardenio.yaml` with only four sensitive `--set`
  overrides (slack channel, dns target, keycloak idp secret) — **none touch
  any Jira value.**
- **Trigger:** GitLab CI `rwx-deploy-prod` (`.gitlab-ci.yml`) dispatches
  `gardenio-deploy --param target=prod` after `rwx-e2e` passes.
- **Safety:** carries a monotonic-version guard (ka-0000001063 /
  ka-0000003492) and a non-destructive pending/superseded rollback recovery.

Bridge Jira values live here, in `deploy-values/gardenio.yaml`:

    jiraBridge.jira.email       = gardenio@acme.example
    jiraBridge.jira.projects    = PE,DEVOPS
    jiraBridge.jira.secretName  = jira-api-token   (ES remoteRef gardenio/jira-api-token)

The Phase-2.3 toggles (`JIRA_OUTBOX_ENABLED` via `jiraBridge.jira.outboxEnabled`,
`JIRA_FIELD_ALLOWLIST_MODE` via `jiraBridge.jira.fieldAllowlistMode`) live in the
chart; when their values are pinned for prod they MUST be set here and nowhere
else.

### Path B — LEGACY / NON-AUTHORITATIVE (acme-helm-chart wrapper)

- **Repo:** `book` / acme-helm-chart.
- **Config:** `.gitlab/ci/gardenio-deploy.yml`, job `deploy-gardenio`.
- **Chart:** `charts/gardenio/` — a *wrapper* that pulls the gardenio OCI
  subchart via `helm dependency update`, then applies its **own**
  `charts/gardenio/values/gardenio.yaml`.
- **Invocation:** `helm upgrade --install gardenio ./ -n gardenio --values
  values/gardenio.yaml` — SAME release, SAME namespace as Path A.
- **Divergence (the hazard):** its `values/gardenio.yaml` carries
  **different** Jira identity:
  - `gardenio.jiraBridge.jira.email = matthew.baker@acme.example`
    (vs authoritative `gardenio@acme.example`)
  - Jira token ES remoteRef `monorepo/jira-api-token`
    (vs authoritative `gardenio/jira-api-token`)
  Its `Chart.lock` is also pinned to a **stale** upstream `2026.115.1`.
- **No monotonic guard.** Rules still allow it to fire via **cron
  schedule**, **manual UI** (`GARDENIO_DEPLOY=1`), **API**, and **trigger
  token**. Its documented "automatic RWX gate-prod-deploy" trigger was
  **already removed** (ka-0000001296, see `.rwx/docker.yml` note) — so the
  job is now orphaned but still armed.

## Why this breaks the cutover

Any `deploy-gardenio` run in acme-helm-chart re-templates the prod release
from its stale, divergent `values/gardenio.yaml`, overwriting Path A's
values — flipping the bridge Jira account back to `matthew.baker@` and
pointing at the wrong token secret. Once the Phase-2.3 toggles land in
`deploy-values/gardenio.yaml`, the same run would silently unset them
(chart defaults: `jiraBridge.enabled=false`, empty email), reverting the
cutover with no error.

## The fix (single source of truth)

1. **Authoritative path = Path A only.** `deploy-values/gardenio.yaml` in
   this repo is the sole source for the bridge Jira identity and (future)
   FSM-sync / allowlist toggles.

2. **CI guard (this repo, implemented here):** `.rwx/deploy.yml` gains an
   `assert-jira-single-source` task, wired into `prepare-chart-prod`, that
   fails the prod deploy fast unless `deploy-values/gardenio.yaml` still
   declares the expected bridge Jira identity
   (`jiraBridge.jira.email: "gardenio@acme.example"`). This both documents
   the invariant and prevents a Path-A deploy that has lost the value.
   When the Phase-2.3 toggles are added, extend the guard's `REQUIRED`
   list to include them.

3. **Disarm Path B (companion change — SEPARATE REPO, NOT in this MR):**
   in acme-helm-chart, the `deploy-gardenio` job (which targets the prod
   `gardenio` release) must be neutralized so it can no longer overwrite the
   prod release. Minimal safe options, smallest first:
   - Delete the `deploy-gardenio` job (keep only `deploy-gardenio-rwx` /
     `deploy-gardenio-next`, which target *other* namespaces), **or**
   - Gate every `deploy-gardenio` rule behind a value that is never set
     (hard-disable), **or**
   - At minimum, make its `values/gardenio.yaml` a thin overlay that does
     NOT redeclare `jiraBridge.jira.*` (defer to the OCI subchart's values).

   This change lives in the acme-helm-chart repo and is **out of scope for
   this docs-only MR** — see "Operator / other-repo action" below.

## Operator / other-repo action required

- **acme-helm-chart repo:** land the Path-B disarm (item 3 above) as a
  companion MR. Until then, do **not** trigger `GARDENIO_DEPLOY=1` in
  acme-helm-chart, and check that no GitLab **schedule** on that project
  sets `GARDENIO_DEPLOY=1` (a scheduled pipeline is the most dangerous
  vector — it fires unattended).
- Verify no active GitLab pipeline schedule on acme-helm-chart carries
  `GARDENIO_DEPLOY=1`; if one exists, disable it.

## Files

- Authoritative values: `deploy-values/gardenio.yaml` (this repo)
- Authoritative deploy: `.rwx/deploy.yml` task `deploy-prod` (this repo)
- CI guard: `.rwx/deploy.yml` task `assert-jira-single-source` (this repo)
- Legacy path to disarm: acme-helm-chart `.gitlab/ci/gardenio-deploy.yml`
  job `deploy-gardenio` + `charts/gardenio/values/gardenio.yaml`
