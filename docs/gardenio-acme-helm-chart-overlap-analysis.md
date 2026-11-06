# Gardenio Deployment Overlap: gardenio vs acme-helm-chart

## Problem Statement

Two independent repos both deploy gardenio to the same Kubernetes cluster, same namespaces, and
same Helm release names. Neither repo knows what the other deployed or when. This creates
version drift, configuration divergence, and unclear ownership.

## Current State: Two Parallel Deploy Paths

### Path 1: Gardenio Monorepo (RWX)

**Source**: `gardenio/.rwx/deploy.yml`
**Chart used**: `gardenio/helm/gardenio/` (monorepo's own chart, deployed directly)
**Values**: `deploy-values/gardenio-rwx.yaml`, `deploy-values/gardenio.yaml`
**Namespaces**: `gardenio-rwx` (staging), `gardenio` (production)
**Release names**: `gardenio-rwx`, `gardenio`
**Trigger**: Automatic on tag push (E2E gate for prod), or manual `rwx dispatch`

The RWX pipeline:
1. Builds images from source, tags as `<calver>-rc`
2. Deploys to `gardenio-rwx` using monorepo chart + monorepo values
3. Runs E2E tests
4. On pass: dispatches `deploy.yml` with `target=prod` to deploy to `gardenio` namespace
5. Also dispatches `.gitlab-ci.yml` manual-deploy which triggers acme-helm-chart (if token set)

### Path 2: acme-helm-chart (GitLab CI)

**Source**: `acme-helm-chart/.gitlab/ci/gardenio-deploy.yml`
**Chart used**: Wrapper chart at `charts/gardenio/` that depends on the upstream gardenio OCI chart
(`oci://<registry>/gardenio/charts/gardenio >=2026.99.1`)
**Values**: `charts/gardenio/values/gardenio.yaml`, `charts/gardenio/values/gardenio-rwx.yaml`
**Namespaces**: `gardenio` (prod), `gardenio-rwx` (staging)
**Release names**: `gardenio`, `gardenio-rwx`
**Trigger**: Manual (`GARDENIO_DEPLOY=1`), scheduled, or API trigger from gardenio monorepo

The acme-helm-chart pipeline:
1. Runs `helm dependency update` to resolve latest upstream OCI chart
2. Deploys wrapper chart with acme-helm-chart's own values files
3. Optionally dispatches E2E tests via RWX

## The Seven Problems

### 1. Same Release, Different Charts

Both repos deploy to the same Helm release names (`gardenio`, `gardenio-rwx`). But they use
**different charts**:

| Aspect | Monorepo (RWX) | acme-helm-chart |
|--------|---------------|-----------------|
| Chart source | `gardenio/helm/gardenio/` (local) | Wrapper around OCI subchart |
| beads3d | Upstream chart templates | Inlined templates (wrapper) |
| Extra templates | None | `agent-namespace-reader-rbac.yaml`, `anthropic-api-key-external-secret.yaml`, `forwardauth-external-secret.yaml` |
| ExternalSecret for Anthropic key | Not present | Present |
| Agent namespace reader RBAC | Not present | Present |
| ForwardAuth ExternalSecret | Not present | Present |

**Impact**: If the monorepo deploys and then acme-helm-chart deploys (or vice versa), the
second deploy will overwrite the first. Resources unique to one chart (e.g. the Anthropic API
key ExternalSecret) will be created or deleted depending on which chart deployed last.

### 2. Divergent Values Files

Both repos maintain **independent copies** of environment values:

| Value | Monorepo (`deploy-values/gardenio.yaml`) | acme-helm-chart (`charts/gardenio/values/gardenio.yaml`) |
|-------|-------|-------|
| Values prefix | Top-level (direct chart) | Nested under `gardenio:` (subchart) |
| PostgreSQL | `postgres:17-alpine` (via acme) / `postgres:16` (monorepo chart default) | `postgres:17-alpine` |
| beads databaseURL | Same (RDS aurora) | Same (RDS aurora) |
| NATS image | chart default `nats:latest` | `public.ecr.aws/docker/library/nats:2.10-alpine` |
| Keycloak realm issuerURL | `https://auth.gardenio-rwx.e2e.example.com/...` (rwx) | `https://auth.e2e.dev.acme.example/...` |
| ForwardAuth | Enabled (RWX values) | Disabled (Phase 1) |
| GitLab Bridge | Not in rwx values | Enabled with full config |
| JIRA Bridge | Not in rwx values | Enabled with full config |

**Impact**: Configuration drifts silently. One deploy might enable ForwardAuth while the other
disables it. Database connection strings, NATS images, and auth URLs differ between the two
sets of values. Whoever deployed last wins, with no record of what changed.

### 3. No Shared Version Pinning

- **Monorepo RWX**: Uses `Chart.yaml` version/appVersion directly (calver). Deploys the exact chart at the tagged commit.
- **acme-helm-chart**: Uses `>=2026.99.1` constraint and `helm dependency update` to resolve the latest OCI chart. The Chart.lock shows `2026.104.3` but the monorepo is at `2026.105.5`.

**Impact**: acme-helm-chart might deploy an older or newer chart version than what the monorepo
intended. The fuzzy version constraint means any `helm dep update` could silently pick up a
new upstream chart.

### 4. Wrapper Chart Has Extra Resources the Monorepo Chart Lacks

acme-helm-chart's wrapper adds templates that don't exist in the upstream gardenio chart:

1. **`agent-namespace-reader-rbac.yaml`**: ClusterRole + ClusterRoleBinding granting the
   `gardenio-agent` ServiceAccount read access to namespaces, pods, logs, events cluster-wide.
   This is critical for agent MR environment inspection.

2. **`anthropic-api-key-external-secret.yaml`**: ExternalSecret pulling the Anthropic API key
   from AWS Secrets Manager. Required for Claude Code agent sessions.

3. **`forwardauth-external-secret.yaml`**: ExternalSecret for ForwardAuth OIDC credentials.

4. **Inlined beads3d templates**: Different from upstream because they add daemon token auth.

**Impact**: When the monorepo deploys, these resources are **not managed by Helm** and become
orphaned. They won't be updated or deleted. If acme-helm-chart deploys, it creates them. If
the monorepo then redeploys, Helm doesn't know about them so they persist as orphans, possibly
with stale configurations.

### 5. Both Target the Same Namespaces with Different Release Metadata

The monorepo deploys release `gardenio-rwx` to namespace `gardenio-rwx`.
acme-helm-chart deploys release `gardenio-rwx` to namespace `gardenio-rwx`.

But the chart names differ:
- Monorepo: chart name = `gardenio` (direct)
- acme-helm-chart: chart name = `gardenio` (wrapper, but contains subchart also named `gardenio`)

Because the release names match, `helm upgrade` from either repo will work on the same release.
But the template outputs differ (wrapper has extra templates). This can cause Helm to delete
resources it doesn't recognize from the other chart, or fail with immutable field errors on
resources with different label selectors.

### 6. Unclear Ownership of Environment Configuration

acme-helm-chart owns:
- AWS ExternalSecret ARNs, IRSA role ARNs, Keycloak realm configuration
- JIRA bridge config (projects, statuses, issue types, poll intervals)
- GitLab bridge config (group ID, bot username, webhook secret)
- Anthropic API key provisioning
- Agent namespace reader RBAC
- IP whitelist source ranges
- beads3d with daemon token auth

Monorepo owns:
- The chart itself (all template definitions)
- CI/CD pipeline (build + E2E + deploy)
- Image builds and calver tagging
- TLS configuration, Keycloak ForwardAuth rollout state
- StorageClass, VolumeSnapshotClass
- NetworkPolicies

Neither declares or documents this split. Both have comments saying "deploy this way" with no
mention of the other.

### 7. The gardenio-rwx Namespace Has Three Deploy Paths

1. **Monorepo RWX** auto-deploys on every tag push (`.rwx/deploy.yml` deploy task)
2. **acme-helm-chart** can deploy via `GARDENIO_RWX_DEPLOY=1` trigger
3. **Monorepo RWX docker.yml** deploys during the build pipeline (`.rwx/docker.yml` deploy task on main push)

Any of these can fire independently, with different chart versions and values.

## Recommendations

### Option A: Consolidate Into the Monorepo (Recommended)

**Goal**: One repo owns the chart, the values, and the deploy pipeline. acme-helm-chart stops
deploying gardenio entirely.

**Steps**:

1. **Port acme-helm-chart-only resources into the upstream chart**:
   - Add `agent-namespace-reader-rbac.yaml` to `gardenio/helm/gardenio/templates/` behind an
     `agentNamespaceReader.enabled` flag
   - Add `anthropic-api-key-external-secret.yaml` to the upstream chart behind an
     `externalSecrets.anthropicApiKey.enabled` flag
   - Add `forwardauth-external-secret.yaml` to the upstream chart (already partially there,
     just needs the AWS SM path)
   - Port the beads3d daemon token auth into the upstream beads3d templates (the whole reason
     the wrapper inlines beads3d)

2. **Merge acme-helm-chart values into monorepo values**:
   - Copy any acme-helm-chart-only config (IRSA ARNs, JIRA bridge details, GitLab bridge
     bot username, Keycloak clients list) into `deploy-values/gardenio.yaml`
   - Ensure `deploy-values/gardenio-rwx.yaml` has everything from
     `acme-helm-chart/charts/gardenio/values/gardenio-rwx.yaml`
   - Delete monorepo values that are already covered by chart defaults

3. **Remove acme-helm-chart gardenio deploy pipeline**:
   - Delete or archive `acme-helm-chart/.gitlab/ci/gardenio-deploy.yml`
   - Delete or archive `acme-helm-chart/charts/gardenio/`
   - Remove the `manual-deploy` job from `gardenio/.gitlab-ci.yml` (no more cross-repo trigger)

4. **Single source of truth**:
   - All gardenio deploys go through `.rwx/deploy.yml`
   - The monorepo chart at `gardenio/helm/gardenio/` is the only chart
   - Environment values live in `deploy-values/`

**Pros**:
- Zero ambiguity about what's deployed and from where
- Chart and values co-evolve in the same commit
- No version drift between chart definition and configuration
- E2E tests validate the exact chart+values combo that goes to prod
- Simpler mental model: one release pipeline

**Cons**:
- acme-helm-chart loses the ability to independently adjust gardenio config
  (this is actually good — unilateral config changes are what caused the drift)
- Requires porting 3 templates and merging 2 values files (one-time effort)

### Option B: Make acme-helm-chart the Sole Deployer

**Goal**: Monorepo builds images and publishes the chart to OCI. acme-helm-chart is the only
thing that runs `helm upgrade`.

**Steps**:

1. Remove deploy tasks from `.rwx/deploy.yml` and `.rwx/docker.yml`
2. Remove `deploy-values/` from the monorepo
3. All environment config lives in acme-helm-chart
4. acme-helm-chart's E2E dispatch already works via RWX
5. The wrapper chart continues to add its extra templates

**Pros**:
- acme-helm-chart already has the environment-specific extras (IRSA, ExternalSecrets)
- Consistent with how acme-helm-chart manages other services (tenant, operators, etc.)

**Cons**:
- **Breaks the fast feedback loop**: Currently, tag push -> build -> deploy -> E2E -> prod is
  a single atomic pipeline. Splitting deploy out to acme-helm-chart adds a cross-repo handoff
  that's slower and harder to debug.
- Chart and values live in different repos, so changes require coordinated PRs
- The wrapper chart pattern is fragile (beads3d is inlined because upstream "lacks" a feature
  that could just be added to upstream)
- Monorepo E2E tests would no longer test the actual chart+values that goes to prod

### Option C: Formalize the Dual-Deploy Pattern (Not Recommended)

Document which repo deploys which namespace, add version checks, coordinate via API.

**Why not**: This preserves the complexity without solving the root problem. Two deploy paths to
the same namespace will always risk overwriting each other. The coordination overhead grows with
every new component.

## Recommended Action Plan

**Pursue Option A** in three phases:

### Phase 1: Port wrapper-only resources to upstream chart (1-2 days)
- Add agent namespace reader RBAC template
- Add Anthropic API key ExternalSecret template
- Add ForwardAuth ExternalSecret template
- Add beads3d daemon token auth to upstream beads3d templates
- All behind feature flags, backwards-compatible

### Phase 2: Merge values and validate (1 day)
- Diff the two values files line-by-line, merge acme-helm-chart specifics into monorepo
- Deploy to gardenio-rwx from monorepo with merged values
- Run full E2E suite
- Deploy to gardenio namespace, verify all resources present

### Phase 3: Retire acme-helm-chart gardenio chart (1 day)
- Delete `acme-helm-chart/charts/gardenio/`
- Delete `acme-helm-chart/.gitlab/ci/gardenio-deploy.yml`
- Remove `manual-deploy` job from monorepo `.gitlab-ci.yml`
- Update CLAUDE.md in both repos
- Communicate the change to the team

## Appendix: Detailed Resource Comparison

### Resources only in acme-helm-chart wrapper (not in upstream chart)

| Resource | Kind | Purpose |
|----------|------|---------|
| `agent-namespace-reader-rbac.yaml` | ClusterRole + ClusterRoleBinding | Grants agent SA cluster-wide read access to namespaces, pods, logs, events |
| `anthropic-api-key-external-secret.yaml` | ExternalSecret | Pulls Anthropic API key from AWS Secrets Manager |
| `forwardauth-external-secret.yaml` | ExternalSecret | Pulls ForwardAuth OIDC credentials from AWS Secrets Manager |
| `beads3d-deployment.yaml` | Deployment | beads3d with daemon token auth nginx sidecar |
| `beads3d-configmap.yaml` | ConfigMap | beads3d nginx config with daemon proxy |
| `beads3d-ingress.yaml` | IngressRoute | beads3d Traefik routing |
| `beads3d-service.yaml` | Service | beads3d ClusterIP |

### Values present in acme-helm-chart but missing from monorepo deploy-values

| Value | acme-helm-chart location |
|-------|-------------------------|
| `agentNamespaceReader.enabled/serviceAccountName` | `values/gardenio.yaml` |
| `gardenio.agents.serviceAccount.annotations` (IRSA) | `values/gardenio.yaml` |
| `gardenio.agents.agentServiceAccount.annotations` (IRSA) | `values/gardenio.yaml` |
| `gardenio.agents.clusterAdmin.*` | `values/gardenio.yaml` |
| `gardenio.agents.s3.bucket/prefix/region` | `values/gardenio.yaml` |
| `gardenio.agents.gitCredentials.*` (ExternalSecret config) | `values/gardenio.yaml` |
| `gardenio.agents.githubTokenSecret` | `values/gardenio.yaml` |
| `gardenio.agents.rwxAccessTokenSecret/*` | `values/gardenio.yaml` |
| `gardenio.agents.mezmoServiceKeySecret/*` | `values/gardenio.yaml` |
| `gardenio.agents.claudeOAuthTokenSecret/*` | `values/gardenio.yaml` |
| `gardenio.jiraBridge.*` (full JIRA config) | `values/gardenio.yaml` |
| `gardenio.gitlabBridge.*` (full GitLab config) | `values/gardenio.yaml` |
| `gardenio.keycloak.clients[]` (8 Keycloak clients) | `values/gardenio.yaml` |
| `gardenio.keycloak.gitlabIdpClientSecret` | `values/gardenio.yaml` |
| `gardenio.nats.image.registry: public.ecr.aws` | `values/gardenio.yaml` |
