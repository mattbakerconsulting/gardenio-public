# E1.1 — `gardenio-rwx` literal audit (ephemeral rwx smoketests)

**Bead:** ka-0000000501
**Parent epic:** ka-0000006268 (E1: Helm chart parameterization for ephemeral rwx)
**Umbrella:** ka-0000002007
**Plan of record:** [`gardenio/plans/ephemeral-rwx-smoketests.md`](../../gardenio-plans/ephemeral-rwx-smoketests.md) §Workstream E1
**Audit commit:** `git rev-parse HEAD` at branch `feat/ka-0000000501-e1-literal-audit`

## Purpose

Inventory every place the literal `gardenio-rwx` (or a close variant — namespace
name, helm release name, ingress host, configmap, IRSA-bound SA name, Slack
channel, env-var target, etc.) is hardcoded in the gardenio monorepo. The next
E1 task uses this inventory to drive parameterization against
`gardenio-rwx-<instance-id>` per the plan-of-record.

**Out of scope** per the parent task:
- Doing the parameterization (next E1 task).
- The production singleton namespace `gardenio` (stays shared).
- RDS / S3 / Slack isolation work (E3).

## Prior art

A 2026-04 audit for the *per-MR* ephemeral epic (`docs/research/gardenio-rwx-hardcode-audit.md`,
archived and excluded from this export, ka-0000007158 under epic ka-0000004402)
covered the same literal but with a
different problem framing (per-MR preview environments, not per-smoke ephemerals)
and pre-dated several files now in scope. This audit:

- Re-uses the prior bucket grain where it still applies.
- Adds files that did not exist or were not in scope then: `.rwx/deploy-rwx.yml`,
  `.rwx/smoke-rwx.yml`, root `.gitlab-ci.yml`, `scripts/ci/gardenio-rwx-lock.sh`,
  `scripts/release.sh`, `scripts/seed-uat.sh`, `gardenio/scripts/setup-gardenio-rwx.sh`,
  `gardenio/controller/internal/bridge/init_prompts.go`, `beads3d/helm/beads3d/values/gardenio-rwx.yaml`.
- Drops files no longer relevant (the per-MR epic was paused).

## Classification scheme

Each occurrence is tagged with one of:

| Tag | Meaning | Parameterization shape |
|---|---|---|
| `release-name` | Helm release / `app.kubernetes.io/instance` selector / resource name prefix | Pass `--release-name=gardenio-rwx-<id>` at dispatch; chart already prefixes via `gardenio.fullname`. |
| `namespace` | K8s namespace to install/operate in | Pass `--namespace=gardenio-rwx-<id>` at dispatch; CI/scripts read it from env. |
| `ingress-host` | DNS hostname (Ingress / OIDC redirect / cookie domain / TLS SAN) | Template against `<id>.<base>` host pattern; wildcard cert decision (open). |
| `configmap-name` | ConfigMap or Secret referenced by literal name | Per-instance name `<release>-<role>` or made obsolete by ephemeralization. |
| `irsa-role` | IAM-bound ServiceAccount / ClusterRoleBinding / S3 prefix | Per-instance SA name; per-instance S3 prefix. IRSA trust policy needs widening — **D2 decision candidate**. |
| `slack-channel` | Slack channel referenced by ID/name in chart value or env | Reuse same channel; isolate via threading (E3). |
| `env-var-target` | CI secret / env var named after the staging instance | Rename to `GARDENIO_STAGING_*` or scope per-instance — **D3 decision candidate**. |
| `lock-name` | The 15-min `gardenio-deploy-lock` ConfigMap mutex | Becomes a no-op once each instance is its own lock domain (E2/E6). |
| `prompt-text` | Comment, banner, log line, prompt template referring to staging by name | Replace literal with `$NAMESPACE` or generalize text; cosmetic but user-visible. |
| `staging-only` | Reference describes the long-lived staging environment *as itself* (the singleton RC) | Stays `gardenio-rwx`; no change needed. |
| `test-mock` | Test fixture name only — never reaches production | No change unless test rewrites against ephemeral semantics. |

`release-name`, `namespace`, `ingress-host`, `irsa-role`, `configmap-name`, and
`env-var-target` are the load-bearing categories for E1. `staging-only`,
`prompt-text`, and `test-mock` are tracked but are not blockers for ephemeral
support.

## Cross-cutting summary

| Category | Count (lines) | Spread (files) | Where |
|---|---:|---:|---|
| `namespace` (literal `gardenio-rwx` as `kubectl -n` / `--namespace`) | ~135 | 6 | `.rwx/deploy.yml`, `.rwx/deploy-rwx.yml`, `.rwx/smoke-rwx.yml`, `.rwx/docker.yml`, `.gitlab-ci.yml`, `gardenio/scripts/setup-gardenio-rwx.sh` |
| `release-name` (helm release / `app.kubernetes.io/instance`) | ~25 | 4 | `.rwx/deploy.yml`, `.rwx/deploy-rwx.yml`, `gardenio/scripts/setup-gardenio-rwx.sh`, `gardenio/helm/values-rwx.yaml` |
| `ingress-host` (`*.gardenio-rwx.e2e.example.com`) | ~20 | 2 | `deploy-values/gardenio-rwx.yaml`, `scripts/seed-uat.sh` |
| `irsa-role` (SA / ClusterRoleBinding name / S3 prefix) | 3 | 1 | `deploy-values/gardenio-rwx.yaml` |
| `configmap-name` (`gardenio-rwx-basic-auth`) | 4 | 1 | `deploy-values/gardenio-rwx.yaml` |
| `env-var-target` (`GARDENIO_RWX_*` secrets in CI) | ~30 | 4 | `.rwx/deploy.yml`, `.rwx/deploy-rwx.yml`, `.rwx/docker.yml`, `.rwx/smoke-rwx.yml` |
| `lock-name` (`gardenio-deploy-lock` + `FORCE_DEPLOY`) | ~25 | 7 | `scripts/ci/gardenio-rwx-lock.sh`, `.rwx/deploy-rwx.yml`, `.rwx/e2e.yml`, `.gitlab-ci.yml`, `scripts/release.sh`, `gardenio/docs/release-pipeline.md`, `gardenio/.github/workflows/release.yml` |
| `prompt-text` (comments, banners, controller prompts) | ~50 | many | `.rwx/*`, `gardenio/controller/internal/bridge/init_prompts.go`, `gardenio/tests/e2e/scripts/*.sh`, `gardenio/tests/e2e/README.md` |
| `staging-only` / `test-mock` | ~25 | many | comments, README, fixtures, dictation-LM, controller tests |

## Surface 1: Helm chart — `gardenio/helm/gardenio/`

Chart templates already use `{{ include "gardenio.fullname" . }}` for resource
names, so the chart itself **does not hardcode** `gardenio-rwx` in any template.
Remaining hits in this tree are doc / comment references to staging.

| File | Lines | Current value | Class | Notes |
|---|---:|---|---|---|
| `helm/gardenio/values.yaml` | 13 | comment: "For staging (gardenio-rwx): use global.latestMode=true …" | `prompt-text` | Optional: re-word to "any non-prod instance". Not a blocker. |
| `helm/gardenio/values.yaml` | 2330 | comment: "managed by another installation (e.g., gardenio-rwx reuses gardenio's realm)." | `prompt-text` | Re-word once per-instance Keycloak realm strategy is decided (E3). |
| `helm/gardenio/values.yaml` | 2449 | comment: "see deploy-values/gardenio-rwx.yaml for the canonical per-host patterns." | `prompt-text` | Re-word to point at template overlay once it lands. |
| `helm/gardenio/README.md` | 111 | "Flip `gardenio-rwx` (staging) to `round_robin`" | `prompt-text` | Cosmetic. |
| `helm/values-rwx.yaml` | 1, 3, 4, 14 | install instructions hardcode `helm install gardenio-rwx … --namespace gardenio-rwx` | `release-name` + `namespace` (in docs) | Extend with an example for `gardenio-rwx-<id>` once parameterization lands. |

**Verdict:** chart templates need **no source change**. The chart already
fully templates against the release name. The values reference file
(`helm/values-rwx.yaml`) is install documentation, not consumed at deploy time
by RWX (the canonical values are at `deploy-values/gardenio-rwx.yaml`); update
its example commands but no template work.

## Surface 2: Deploy values — `deploy-values/`

This is the load-bearing file for E1. The whole file presumes a single staging
instance at the literal hostname `gardenio-rwx.e2e.example.com`. Every
non-comment occurrence is a **must-template** hit for ephemeral support.

| File | Lines | Current value | Class | Parameterization shape |
|---|---:|---|---|---|
| `deploy-values/gardenio-rwx.yaml` | 1, 2, 12 | header docstring | `prompt-text` | Re-word as "staging or per-instance overlay generated from `gardenio-rwx-ephemeral.yaml.tmpl`". |
| | 15-20 | env vars `GARDENIO_RWX_DEFAULT_ACCOUNT`, `…_LITELLM_URL`, `…_SLACK_CHANNEL`, `…_TLS_DNS_TARGET`, `…_KEYCLOAK_CLIENT_SECRET`, `…_KEYCLOAK_COOKIE_SECRET` | `env-var-target` | These are CI secret names; renaming creates churn in RWX vault. **Recommendation: keep names; treat them as "shared staging-environment secrets" reused across ephemerals.** Open Q (D-secret-rename) if operator prefers a rename. |
| | 29 | `global.basicAuth.secret: gardenio-rwx-basic-auth` | `configmap-name` | Per-instance: `${RELEASE_NAME}-basic-auth` OR a single shared secret reused (decision: do ephemerals need per-instance basic-auth? — likely no, they share creds). Default proposal: keep one shared `gardenio-rwx-basic-auth` Secret in a sentinel namespace synced via ExternalSecret. |
| | 105 | `beads.ingress.host: beads.gardenio-rwx.e2e.example.com` | `ingress-host` | `beads.<id>.gardenio-rwx.e2e.example.com` (or whatever wildcard scope is decided). |
| | 156 | `agents.agentServiceAccount.name: gardenio-rwx-agent` | `irsa-role` | Cluster-scoped IAM/SA collisions across concurrent ephemerals → **MUST be release-scoped**: `${RELEASE_NAME}-agent`. Requires IRSA trust policy to allow `system:serviceaccount:gardenio-rwx-*:${RELEASE_NAME}-agent` (regex / multi-condition). **D2 decision candidate.** |
| | 160 | `agents.s3.prefix: tenants/gardenio-rwx/sessions` | `irsa-role` (data) | Per-instance prefix `tenants/${INSTANCE_ID}/sessions`. Bucket stays shared. |
| | 165 | `agents.clusterAdmin.name: gardenio-rwx-agent-cluster-admin` | `irsa-role` | Cluster-scoped ClusterRoleBinding; **MUST be unique per instance** or installs collide silently. `${RELEASE_NAME}-agent-cluster-admin`. |
| | 211, 420, 450, 496, 528 | Ingress hosts: `coopmux.gardenio-rwx.…`, `gardenio-rwx.e2e.example.com`, `beads3d.gardenio-rwx.…`, `beads-viewer.gardenio-rwx.…`, `roles.gardenio-rwx.…` | `ingress-host` | All five service hosts must be templated against `${HOST_BASE}`. |
| | 403, 407 | TLS wildcard `*.gardenio-rwx.e2e.example.com` | `ingress-host` | **D-tls decision candidate**: (a) parent wildcard `*.<base>.e2e.example.com` covers all ephemerals (1 cert, simple — confirms a sub-wildcard is unsupported, would need a 2-level wildcard which Let's Encrypt does not issue free), (b) per-instance cert at deploy time (more issuance load, longer rollout), (c) per-instance cert with ACME DNS-01 + caching. |
| | 425, 455, 533 | Per-service emergency basic-auth fallback `gardenio-rwx-basic-auth` | `configmap-name` | Same as line 29. |
| | 572 | comment "real gardenio-rwx domain patterns" | `prompt-text` | Cosmetic. |
| | 580, 611, 642, 675, 706, 739, 773 | Keycloak `redirectUris` / `webOrigins` for the seven per-service clients | `ingress-host` | Per-instance client redirectUris, each carrying `${HOST_BASE}`. Realm itself stays shared (E3 decision: shared realm + dynamic clients vs per-instance realm). |
| | 871, 872, 879 | `forwardAuth.cookieDomain: gardenio-rwx.e2e.example.com` + ingress host | `ingress-host` | Cookie domain must match the ephemeral's host suffix; SSO scope drives the wildcard decision. |
| `deploy-values/gardenio.yaml` | 105 | comment: "Mirrored from gardenio-rwx (ka-0000000853)" | `prompt-text` | Pure comment. No change needed (prod is out of scope per E1 plan). |

**Templating PR scope (E1.2):** introduce `deploy-values/gardenio-rwx-ephemeral.yaml.tmpl`
with `${RELEASE_NAME}`, `${INSTANCE_ID}`, `${HOST_BASE}` placeholders. Generate
at dispatch time via `scripts/render-rwx-values.sh`. Keep the canonical
`deploy-values/gardenio-rwx.yaml` as the long-lived staging values file (it is
"instance-id = gardenio-rwx" — the *singleton* staging instance and an example
caller of the template).

## Surface 3: RWX dispatch chain — `.rwx/`

### `.rwx/deploy-rwx.yml` — main-push staging deploy (the file E2 will fork)

This is the canonical deploy task that the ephemeral path is forking from.
Every `gardenio-rwx` literal here is either a `namespace`/`release-name` or a
`prompt-text`.

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 1, 60, 190, 282, 421, 459, 465 | Comments / progress text | `prompt-text` | Replace literals with `$NAMESPACE` once param wiring lands. |
| 232 | `LOCK_NS="gardenio-rwx"` | `lock-name` (namespace target of the lock) | E2: drop the lock entirely for ephemeral instances (each instance is its own lock domain). |
| 283 | `kubectl create namespace gardenio-rwx` | `namespace` | `kubectl create namespace ${NAMESPACE}`. |
| 292, 294, 297, 299, 301, 303–322, 330, 336, 345, 364, 367–368, 386, 399–406, 422–423, 436–438, 444–447, 452, 455, 458 | All `-n gardenio-rwx`, helm release `gardenio-rwx`, label selectors, JSON metadata.namespace patches, helm uninstall on stuck install | `namespace` + `release-name` | Replace with `${NAMESPACE}` and `${RELEASE_NAME}` (same value in the staging case; different per-instance). |
| 355 | `VALUES=deploy-values/gardenio-rwx.yaml` | `release-name` (values-file path) | Replace with rendered template path: `VALUES=$(./scripts/render-rwx-values.sh "$INSTANCE_ID")`. |
| 343–347, 425–428 | Sets `agents.defaultAccount`, `agents.litellmURL`, `slackBridge.slack.channel`, `tls.dns.target` from `$GARDENIO_RWX_*` env | `env-var-target` | Pass-through. Keep env names (rename creates RWX-vault churn for no functional gain); these become "shared ephemeral settings" rather than "staging-only". |
| 466, 475–480 | `init.force-deploy` + `GARDENIO_RWX_*` secrets exposure | `env-var-target` + `lock-name` | The deploy lock is unnecessary for ephemerals → `force-deploy` becomes obsolete (E6). For E1 transitional state, keep the env passthrough. |

**E1.2 PR shape:** add `instance-id` param to `on.dispatch.params`, derive
`NAMESPACE` and `RELEASE_NAME` from it, replace every `gardenio-rwx` literal in
`namespace` / `release-name` rows with the variable, leave the lock logic as-is
(E2 removes it). Backwards-compatible: `instance-id` defaults to `gardenio-rwx`
so main-push CI is unchanged.

### `.rwx/deploy.yml` — combined deploy (staging + prod target)

Same structure as `deploy-rwx.yml` for the staging branch. The prod branch
(target=prod, namespace=`gardenio`) is **out of scope** — keep it untouched.

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 1, 7, 203, 285, 311, 326, 329, 332, 353, 380, 388, 415 | Comments / banner / progress | `prompt-text` | Same treatment. |
| 147–148, 154 | values file path `deploy-values/gardenio-rwx.yaml` | `release-name` (values path) | Same template-render treatment. |
| 209, 211–212 | `kubectl create namespace gardenio-rwx` + values | `namespace` | Templated. |
| 215, 217, 222, 224, 226, 229–238, 246, 254, 266, 276, 286–287, 289, 291–308, 313, 315, 319, 320–321, 327, 337, 340, 343, 346, 348, 350, 354, 356, 359, 361–362, 364, 366–367, 370, 377, 391, 394, 397, 401, 405, 412 | `-n gardenio-rwx`, helm release name, label selectors, helm `meta.helm.sh/release-namespace` annotations | `namespace` + `release-name` | Templated. |
| 274–278, 323–328 | `GARDENIO_RWX_*` env-driven helm sets | `env-var-target` | Same: keep names, treat as shared ephemeral inputs. |
| 377–382 | secret env exposures | `env-var-target` | Pass-through. |
| 484 | comment "ephemeral gardenio-rwx env, catastrophic for prod" | `prompt-text` | Re-word once ephemerals are live ("ephemeral instance under gardenio-rwx-*"). |

### `.rwx/smoke-rwx.yml` — main-push 5-module smoke (NEW since prior audit)

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 1, 8, 96 | Comments referring to the staging gate | `prompt-text` | Re-word. |
| 106 | `NAMESPACE="gardenio-rwx"` | `namespace` | Replace with `NAMESPACE="${NAMESPACE:?required}"` once `instance-id` is wired in (default to `gardenio-rwx` until E2 lands). |
| 120 | `kubectl -n "$NAMESPACE" get deploy gardenio-rwx-agents` | `release-name` | The deploy name is release-prefixed. `${RELEASE_NAME}-agents`. |
| 133 | `kubectl … rollout status deploy/gardenio-rwx-agents` | `release-name` | Same. |
| 163 | `LITELLM_URL: ${{ secrets.GARDENIO_RWX_LITELLM_URL }}` | `env-var-target` | Pass-through; keep name. |

### `.rwx/e2e.yml` — full-suite tag/scheduled E2E

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 14, 15 | Param description + default `gardenio-rwx` | `namespace` | Default OK for ad-hoc; per-MR / per-tag dispatchers must pass an explicit `--param namespace=…`. |
| 35 | `cli.init.namespace: "gardenio-rwx"` | `namespace` | Same — CLI fallback. |
| 44 | `namespace: "gardenio-rwx"` (dispatch init mapping) | `namespace` | Same. |
| 136, 151, 174, 207, 222, 245 | Multiple `NAMESPACE="${NAMESPACE:-gardenio-rwx}"` shell defaults | `namespace` | Keep default; ephemeral dispatcher always passes explicit value. |
| 258–259, 346, 523–524 | `source scripts/ci/gardenio-rwx-lock.sh` | `lock-name` | E2 removes the lock for ephemerals (per-instance has no shared lock domain). For E1 transitional, the lock is namespace-scoped — change `LOCK_NS` to `$NAMESPACE` so the staging singleton still serializes while ephemerals are no-op. |
| 289 | banner: `"  gardenio-rwx running:    $DEPLOYED_TAG"` | `prompt-text` | Replace with `"  $NAMESPACE running:    $DEPLOYED_TAG"`. |
| 325 | banner: "To actually test new app code in gardenio-rwx" | `prompt-text` | Replace literal with `$NAMESPACE`. |

### `.rwx/docker.yml` — image build + post-build smoke (legacy event-triggered path)

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 2130, 2133, 2135 | Comments referring to "gardenio-rwx after deploy" | `prompt-text` | Cosmetic. |
| 2145 | `NAMESPACE="gardenio-rwx"` | `namespace` | Same treatment as smoke-rwx.yml line 106. |
| 2163, 2176, 2177, 2181 | `kubectl -n "$NAMESPACE" get deploy gardenio-rwx-agents` / `…-beads` | `release-name` | Use `${RELEASE_NAME}-agents` / `${RELEASE_NAME}-beads`. |
| 2395 | `LITELLM_URL: ${{ secrets.GARDENIO_RWX_LITELLM_URL }}` | `env-var-target` | Keep. |

**Note on `.rwx/docker.yml` post-build smoke:** the in-docker.yml smoke task is
the "old" event-triggered gate that the GitLab-CI dispatcher (`smoke-rwx.yml`)
replaced for main-push. It still runs on docker-event triggers and shares the
same hardcoding. Treat it as the same surface for E1 templating.

### `.rwx/README.md`

Lines 36, 37, 38, 67, 73, 78 — install docs / dispatch examples. `prompt-text`.
Update once dispatchers accept `instance-id`.

## Surface 4: GitLab CI — root `.gitlab-ci.yml`

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 32 | Comment: "Emergency prod deploy bypass: GARDENIO_DEPLOY=1" | `prompt-text` | Unchanged for E1. |
| 119, 142, 481 | `if: $GARDENIO_DEPLOY == "1"` rule | `prompt-text` (variable name) | Variable is staging/release-bypass-shaped — keep. |
| 131, 134 | Comment block "Stage: deploy → gardenio-rwx (staging)" | `prompt-text` | Re-word once dispatcher passes `instance-id`. |
| 168, 174, 202 | Same kind of stage banners | `prompt-text` | Cosmetic. |
| 244 | `--param "namespace=gardenio-rwx"` in `rwx-e2e` job | `namespace` | This is the canonical "always pass an explicit namespace" call — for ephemerals the dispatcher must compute `gardenio-rwx-<id>` and pass it instead. |
| 471, 767 | Comment / Slack notify referencing GARDENIO_DEPLOY | `prompt-text` | Cosmetic. |

**Note:** there is **no `gardenio/.gitlab-ci.yml`** (the path the bead description
mentions). The only GitLab CI file is the root `.gitlab-ci.yml`. Recording this
as a bead description error to fix during MR review.

## Surface 5: Scripts — `scripts/` and `gardenio/scripts/`

### `scripts/ci/gardenio-rwx-lock.sh` — the 15-min ConfigMap mutex

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 2, 6, 18 | Header comments | `prompt-text` | Re-word once lock is namespace-scoped. |
| 23 | `LOCK_CM="gardenio-deploy-lock"` | `lock-name` | E1.2 transitional: keep this CM name; lock per-namespace. E6: delete the script entirely. |
| 24 | `LOCK_NS="gardenio-rwx"` | `namespace` | **Must be parameterized**: `LOCK_NS="${LOCK_NS:-gardenio-rwx}"` so callers can scope per-instance. |
| 30, 31, 83 | `FORCE_DEPLOY` escape hatch | `lock-name` | Same — irrelevant once ephemerals don't share a lock. |

### `scripts/release.sh`

Lines 12, 19, 62, 63, 68 — comments and progress text describing the staging
deploy stage. `prompt-text` only. No code change for E1.

### `scripts/seed-uat.sh` — UAT-data seeder

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 2, 5, 6 | Header / usage examples | `prompt-text` | Cosmetic. |
| 138, 141, 145–150 | UAT bead description text containing six host literals | `ingress-host` (in seed data) | These bake the staging hostnames into the seed dataset. **For ephemerals: do not run this script against ephemerals** (UAT seeding is a staging-singleton concern). Add a guard: `if [ "$NAMESPACE" != "gardenio-rwx" ]; then exit 0; fi` to make this explicit. |

### `gardenio/scripts/setup-gardenio-rwx.sh` — manual staging bootstrap

| Lines | Concern | Class | Notes |
|---:|---|---|---|
| 2, 9, 91 | Header / usage / progress | `prompt-text` | Re-word once parameterized. |
| 16 | `NAMESPACE="gardenio-rwx"` | `namespace` | `NAMESPACE="${NAMESPACE:-gardenio-rwx}"`. |
| 17 | `RELEASE="gardenio-rwx"` | `release-name` | `RELEASE="${RELEASE:-$NAMESPACE}"`. |

This script is operator-run, not CI. Low risk to parameterize.

### `gardenio/scripts/release.sh`, `verify-deploy.sh`, `verify-image-tags.sh`

No `gardenio-rwx` literal hits.

### `scripts/build-e2e-bug-bead-desc.py`, `scripts/build-dictation-lm/*`

| File | Lines | Class | Notes |
|---|---:|---|---|
| `scripts/build-e2e-bug-bead-desc.py` | 81 | `staging-only` | Bead description writer assumes ns is `gardenio-rwx`. For E2 ephemerals: take `--namespace` arg and embed in the bead description. |
| `scripts/build-dictation-lm/Sources/build-dictation-lm/main.swift` | 248 | `test-mock` | Dictation language-model corpus example string — completely unrelated to deploy. No change. |
| `scripts/build-dictation-lm/README.md` | 59 | `test-mock` | Same. |

### `scripts/setup-perimeter-guard.sh`

Line 12 — a usage-example comment showing `KD_HOST=http://beads.gardenio-rwx:8080`.
`prompt-text`. Cosmetic.

## Surface 6: E2E test scripts — `gardenio/tests/e2e/`

The prior audit (`docs/research/gardenio-rwx-hardcode-audit.md`, archived and
excluded from this export) covered these in detail (§"`gardenio/tests/e2e/scripts/*.sh`"). The shape is
unchanged for E1: every test script reads `NAMESPACE` from env with a default
of `gardenio-rwx`. The default is a convenience for ad-hoc runs against staging;
ephemerals always set `NAMESPACE` explicitly.

Files affected (all `namespace`-class, single line each):
`wait-for-stack-ready.sh:17`, `test-tls-certs.sh:24`, `test-config-bead-hotreload.sh:22`,
`test-jira-bridge.sh:17`, `test-gitlab-bridge.sh:18`, `test-oidc-auth.sh:30`,
`test-credential-rotation.sh:26`, `test-cred-rotation-resume.sh:38`,
`test-litellm-cost-tracking.sh:27`, `test-mr-merge-flow.sh:28`,
`test-escalation-execution.sh:28`, `test-escalation-approval.sh:31`,
`test-escalation-slack-notify.sh:27`, `test-semantic-search.sh:18`,
`run-shard.sh:29`.

`run-shard.sh:62` (banner: `"gardenio-rwx running:"`) — `prompt-text`. Replace
with `"$NAMESPACE running:"`.

`e2e-dispatch-and-check.sh:19` — `NAMESPACE="gardenio-rwx"` (no env override).
**Class: `namespace` — must be parameterized** before the script can be used
against an ephemeral.

`gardenio/tests/e2e/formulas/gardenio-e2e-master.json:3` and four other
occurrences — formula description strings. `prompt-text` (formula description
visible to humans only).

`gardenio/tests/e2e/README.md` — six install/usage examples (`helm upgrade
--install gardenio-rwx`, port-forward, "Cluster: gardenio-rwx namespace").
`prompt-text`. Add ephemeral usage examples once the path lands.

## Surface 7: Controller — `gardenio/controller/`

These are **Go source / test fixture** hits. None are runtime hardcoding of the
deploy target — the controller learns its namespace from env / pod metadata.

| File | Lines | Class | Notes |
|---|---:|---|---|
| `internal/e2eguardrail/guardrail.go` | 22 | `prompt-text` | Comment listing recognized E2E namespaces. The guardrail logic uses prefix matching (`gardenio-` family) — already works for `gardenio-rwx-*`. |
| `internal/e2eguardrail/guardrail_test.go` | 15 | `test-mock` | Test fixture URL `gardenio-beads.gardenio-rwx.svc.cluster.local`. Add a fixture for `gardenio-rwx-foo` in E5. |
| `internal/pvgc/collector_test.go` | 29, 152 | `test-mock` | PVC garbage-collector test fixtures. **Important for E4 (reaper):** the reaper must accept `gardenio-rwx-*` as the per-instance namespace family, not just `gardenio-rwx`. Update the test data and the matcher when E4 lands. |
| `internal/podmanager/manager_volumes_test.go` | 360 | `prompt-text` | Comment about admission rejection. No code change. |
| `internal/bridge/bot.go` | 80, 262 | `prompt-text` | Doc comments showing `beads.gardenio-rwx.e2e.example.com` as an example URL. Cosmetic. |
| `internal/bridge/init.go` | 822 | `prompt-text` | Label `role:gardenio-rwx-tester` — this is a gardener role name. The role name happens to encode the namespace. **For ephemerals: keep the role name** (it identifies the *type* of work, not which instance — the gardener runs E2E against whichever namespace the dispatch points to). Re-naming to `role:rwx-tester` is a follow-up cosmetic, not an E1 blocker. |
| `internal/bridge/init_test.go` | 140, 182, 186, 189, 352, 533 | `test-mock` | Tests for the role name above. Same disposition. |
| `internal/bridge/init_prompts.go` | 348, 349, 352, 353, 357, 358, 359, 376, 377, 381, 409, 415, 422, 424, 426, 431, 435 | `prompt-text` | The `gardenio-rwx-tester` gardener prompt template hardcodes `kubectl … -n gardenio-rwx` and `rwx dispatch gardenio-e2e --param namespace=gardenio-rwx`. **For E2 / E5: these prompts must be updated** so the gardener dispatches against an ephemeral namespace per its assigned task — but for E1 (literal audit only), no change needed. **Decision candidate D-tester-prompt:** does the rwx-tester gardener (a) own ephemerals end-to-end (deploy → e2e → teardown), or (b) only dispatch against an existing ephemerals fleet? |
| `internal/bridge/init_prompts_test.go` | 148 | `test-mock` | Same. |
| `internal/bridge/gitlab_commitstatus_test.go` | 48 | `test-mock` | Test fixture for "RWX: Deploy: gardenio-rwx" commit status name. The commit-status name is a string; ephemerals don't write commit statuses (no MR backing them). No change. |
| `cmd/gb/fleet_test.go`, `cmd/gb/ci_test.go` | (fixture strings) | `test-mock` | No deploy impact. |

## Surface 8: beads3d — `beads3d/helm/beads3d/values/`

`beads3d/helm/beads3d/values/gardenio-rwx.yaml`:
- Line 1: header comment `# gardenio-rwx deployment values` — `prompt-text`.
- Line 13: `url: http://gardenio-rwx-beads:8080` — `release-name` (in-cluster
  service hostname). For ephemerals: `url: http://${RELEASE_NAME}-beads:8080`,
  rendered from a template.

This file is consumed by the **separate** beads3d Helm chart. **Important
caveat:** beads3d is a sibling chart, not a subchart of gardenio. If beads3d is
deployed inside the gardenio-rwx-<id> namespace as part of the ephemeral, this
file must be templated alongside `deploy-values/gardenio-rwx.yaml`. If beads3d
is deployed *separately* and only points at the gardenio ephemeral, only the
`url` line matters and it's an outbound reference rather than an inbound
identity.

**Open scoping question (D-beads3d-shape):** is beads3d part of the ephemeral
instance, or is it a long-lived viewer that points at whichever ephemeral is
under test? The plan-of-record §"Architecture sketch" implies beads3d is part
of the chart family deployed into the ephemeral namespace (the ingress hosts
include `beads3d.gardenio-rwx.e2e.example.com`), in which case templating
is required. Confirming with the operator before E1.2 chooses an approach.

## Surface 9: Documentation

| File | Lines | Class | Notes |
|---|---:|---|---|
| `gardenio/docs/release-pipeline.md` | 47, 118, 169, 178, 191, 198, 210 | `lock-name` + `prompt-text` | Describes the deploy lock and FORCE_DEPLOY runbook. **E6 deletes both.** No change for E1. |
| `gardenio/docs/secret-rotation.md`, `…/user-guide/11-e2e-testing.md`, `…/rbac-audit-2026-04-21.md`, `…/research/*` | various | `prompt-text` | Reference docs. Update once ephemerals are live. |
| `gardenio/docs/audits/*` | various | `prompt-text` | Audit snapshots. Out of date as soon as ephemerals land; replace with new audit at E6. |
| `docs/research/gardenio-rwx-hardcode-audit.md` | full | `prompt-text` | The prior per-MR audit. Cross-reference from this doc, leave intact. |
| `docs/research/per-mr-namespace-lifecycle.md` | various | `prompt-text` | Companion doc to the above. Out of scope (per-MR was paused). |
| `docs/rwx-agent-guide.md` | various | `prompt-text` | Agent reference. Cosmetic. |
| `gardenio/.github/workflows/release.yml` | 59 | `prompt-text` | `GARDENIO_DEPLOY=1` reference. Unchanged. |

## Open scoping questions (raise as `gb decision` before E1.2 starts)

The audit surfaces several forks where E1.2's parameterization shape depends
on a choice that has not yet been made. Each is a candidate for a `gb decision`
filed by the gardener in its next cycle:

1. **D-irsa-trust** — IRSA trust policy widening for `gardenio-rwx-*` SAs. The
   current trust policy bound to the IAM role likely names
   `system:serviceaccount:gardenio-rwx:gardenio-rwx-agent` exactly. Ephemerals
   need either (a) a single regex/multi-condition policy matching
   `system:serviceaccount:gardenio-rwx-*:*-agent`, or (b) per-instance role
   creation at deploy time (slow, IAM-rate-limit risk). **Recommendation:**
   (a). Open until operator confirms with AWS-side IAM owner.
2. **D-tls-cert-scope** — wildcard cert scope. Options as in the
   `deploy-values` row 403/407 above: parent wildcard vs per-instance issuance.
   **Recommendation:** parent wildcard `*.<base>.e2e.example.com` covering
   all ephemerals (1 cert, simple ops, requires DNS subdomain restructure).
3. **D-secret-rename** — keep `GARDENIO_RWX_*` secret names or rename to
   `GARDENIO_STAGING_*` / `GARDENIO_EPHEMERAL_*`. **Recommendation:** keep names
   (rename is high-churn, low-value).
4. **D-basic-auth-shape** — per-instance basic-auth secret vs single shared.
   **Recommendation:** single shared (basic-auth is an emergency fallback only,
   not the primary auth path).
5. **D-beads3d-shape** — is beads3d part of the ephemeral or a long-lived
   viewer? Affects whether `beads3d/helm/beads3d/values/gardenio-rwx.yaml` is
   per-instance or singleton.
6. **D-tester-prompt** — `gardenio-rwx-tester` gardener prompt template scope:
   own ephemerals end-to-end vs dispatch-only. Affects E4/E5 scope.

These are **deferred to the gardener's next cycle** per the plan-of-record's
"per-epic operator approval" rule. None block the audit's correctness; they
only shape what E1.2 produces.

## Surfaces NOT in scope (confirmed empty)

- `coop/` — no `gardenio-rwx` hits (tested with grep).
- `kbeads/` — no `gardenio-rwx` hits (tested with grep).
- `gardenio/helm/gardenio/templates/` — no template hardcoding (chart already
  fully release-name-templated).
- `gardenio/.gitlab-ci.yml` — file does not exist (only the root
  `.gitlab-ci.yml` exists; the bead description mentions a stale path).
- Production singleton `gardenio` namespace — explicitly out of scope per E1
  plan §Goal.

## Summary — what E1.2 must do

| Step | Surface | Shape |
|---|---|---|
| 1 | `deploy-values/gardenio-rwx.yaml` | Convert into `gardenio-rwx-ephemeral.yaml.tmpl` with `${RELEASE_NAME}`, `${INSTANCE_ID}`, `${HOST_BASE}` placeholders. Keep canonical staging file as the rendered output for `instance-id=gardenio-rwx`. |
| 2 | `scripts/render-rwx-values.sh` | New script: `render-rwx-values.sh <instance-id>` → emits a rendered values yaml. |
| 3 | `.rwx/deploy-rwx.yml` + `.rwx/deploy.yml` | Add `instance-id` param (default `gardenio-rwx`); replace every `namespace`/`release-name` literal with `${NAMESPACE}` / `${RELEASE_NAME}`; values file from step 2. |
| 4 | `.rwx/smoke-rwx.yml` + `.rwx/e2e.yml` + `.rwx/docker.yml` | Same `instance-id` plumb-through; replace banner literals with `$NAMESPACE`. |
| 5 | `scripts/ci/gardenio-rwx-lock.sh` | Make `LOCK_NS` env-overridable (transitional; E6 deletes the file). |
| 6 | `gardenio/scripts/setup-gardenio-rwx.sh` | Make `NAMESPACE` / `RELEASE` env-overridable. |
| 7 | `gardenio/tests/e2e/scripts/e2e-dispatch-and-check.sh` | Add `NAMESPACE="${NAMESPACE:-gardenio-rwx}"` envvar shape (currently hardcoded). |
| 8 | `beads3d/helm/beads3d/values/gardenio-rwx.yaml` | Conditional on D-beads3d-shape. If part of ephemeral: template against `${RELEASE_NAME}-beads`. |
| 9 | `gardenio/.gitlab-ci.yml` reference in bead description | **Bead description fix:** path doesn't exist; only root `.gitlab-ci.yml` is in scope. Note in MR description for parent-epic update. |
| 10 | Verification | `helm install gardenio-rwx-foo -n gardenio-rwx-foo` against an EKS scratch namespace. **Awaits operator approval** per umbrella plan. |

Steps 1–9 are docs/code. Step 10 is implementation and **must not be done in
this audit MR**; it is the verification gate at the end of E1 per the plan-of-record.

## Source of audit

```
git rev-parse HEAD                                     # commit at audit time
grep -rn 'gardenio-rwx' \
  gardenio/helm/ \
  deploy-values/ \
  .rwx/ \
  .gitlab-ci.yml \
  scripts/ \
  gardenio/scripts/ \
  gardenio/tests/ \
  gardenio/controller/ \
  gardenio/docs/ \
  beads3d/

grep -rn 'GARDENIO_RWX\|gardenio-deploy-lock\|FORCE_DEPLOY\|GARDENIO_DEPLOY' \
  .rwx/ .gitlab-ci.yml scripts/ gardenio/
```

Counts in the cross-cutting summary are line-level totals from these greps.
The "Spread" column is the count of distinct files contributing to the row.

## Next task

E1.2 — *Parameterize the helm chart + dispatch chain against `instance-id`*.
Filed once the gardener's next cycle reviews this audit and the open decisions
above are resolved.
