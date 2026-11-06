# Pipeline Stages and Timing

## Pipeline Structure

```
.pre (30s) -> build (3-8min) -> test (parallel) -> security -> deploy (5-12min) -> e2e (10-20min) -> agents -> release
```

7 stages, defined in `~/book/monorepo/.gitlab-ci.yml`.

## Critical Path (MR Pipeline)

| Phase | Duration | Bottleneck? |
|-------|----------|-------------|
| `.pre` checks | ~30s | No |
| **Build** (parallel per service) | 3-8min | Cache-dependent |
| **Deploy-mr** (sequential) | 5-12min | **Yes** -- single job, helm --wait |
| **E2e tests** (3 shards) | 10-20min | **Yes** -- flakiness drives retries |
| Coverage merge | 1-2min | No |
| **Total** | **20-45min** | |

Unit tests run in the `test` stage parallel with builds -- not on critical path.

## Build Stage

Config: `~/book/monorepo/cicd/gitlab/build/template.yml`

MR pipelines build **x86_64 only** (single-step). Main/release builds are multi-arch (x86+arm64+manifest).

Images built per full MR:
- `acme-playwright`, `sponsor-api`, `sponsor-setup-db`, `sponsor-external-api`
- `site-api`, `site-setup-db`, `study-web`, `migration-web`
- `sponsor-connect`, `site-connect`, `migration-api`, `migration-api-setupdb`
- `data-dictionary-api`, `data-dictionary-ingest`

Uses `--cache-from` for layer caching. Istanbul instrumentation enabled for MR builds.

## Deploy-mr Stage

Config: `~/book/monorepo/cicd/gitlab/eks/template.yml`

Sequential within job:
1. AWS auth + kubectl config (~15-30s)
2. Helm chart pull (~10s)
3. Stuck release cleanup (~0-120s)
4. **Helm install/upgrade --wait --wait-for-jobs --timeout 10m** (the bottleneck)
5. Background watcher: every 15s for 40 iterations, checks pods and captures setup job logs

Key optimizations already applied (bd-t0czg):
- `skipSetupOnUpgrade=true` for existing releases
- Image pre-puller DaemonSet
- Faster health probes
- Removed `--force` flag
- Selective helm repo update

**Result:** Deploy-mr reduced from 376s to 85s (77% improvement). But still blocks on `--wait` for all pods Ready.

## E2E Stage

Config: `~/book/monorepo/cicd/gitlab/e2e/template.yml` + `cicd/gitlab/e2e/run.sh`

Flow per shard:
1. `bun install` (~15-30s)
2. Service warmup -- health endpoint polling, 12 attempts x (10s timeout + 5s sleep) per service
3. Shard stagger: `(shard - 1) * 15s`
4. **Test execution**: `bun run test:ci --workers=6 --retries=2`
5. JUnit parsing + min_passing_tests check (20 minimum)
6. Coverage collection (frontend Istanbul + backend `/__coverage__`)
7. Badge generation + Slack notification

**Quarantine** runs separately: `e2e-quarantine` job, `allow_failure: true`, 3 workers, 0 retries.

## Test Stage (Not on Critical Path)

Runs parallel with build:

| Service | Jobs | Runner |
|---------|------|--------|
| sponsor-api | 4-way split pytest + coverage merge + external-api + analyze + rust | saas-linux-small-amd64 |
| site-api | pytest + analyze | saas-linux-small-amd64 |
| study-web | vitest + analyze + meticulous | default |
| sponsor-connect | analyze + test | saas-linux-small-amd64 |
| site-connect | analyze + test | saas-linux-small-amd64 |
| apikit | analyze + test (Postgres) | saas-linux-small-amd64 |
| data-dictionary | analyze + test (OpenSearch) | saas-linux-small-amd64 |
| e2e | analyze (biome + eslint + tsc) | default |

## Runner Tags

| Tag | Used By | Type |
|-----|---------|------|
| `saas-linux-small-amd64` | Unit tests, linting | GitLab SaaS (2 vCPU, 8GB) |
| (default) | JS/Bun jobs | GitLab SaaS medium |
| `kube-x86-xlarge` | **E2e shards + quarantine** | Self-hosted K8s |
| `aarch64` | Multi-arch builds | Self-hosted ARM |
| `kube-aarch64-dedicated` | Helm chart pipeline | Self-hosted K8s ARM |

## Optimization Opportunities

### Already Done (bd-t0czg, closed)
- Deploy-mr: 376s -> 85s (77% reduction)
- skipSetupOnUpgrade, faster probes, image pre-puller, no --force

### In Progress (beads-wwk3)
- Parallelize site-api + sponsor-api setup (-50s)
- Cache helm chart (-30s)
- Health-check polling for earlier e2e start (-100-200s)
- Pre-build base images nightly (-20s)

### New Opportunities
1. **App log collection** -- correlate 503s with server errors to fix root causes (reduces retries)
2. **Quarantine investigation** -- 50 failing tests are ISF v2 503s; fixing backend could recover 22 spec files
3. **E2E_FAST tradeoff** -- disabling traces/screenshots saves time but loses failure diagnostics
4. **Runner resource monitoring** -- confirm kube-x86-xlarge has enough CPU/memory for 6 Chromium instances
5. **Warm pod pool** -- keep app pods warm between deploys instead of recreating

## Run Results

### Run 1 (Pipeline #2347870105) -- Baseline

**Date:** 2026-02-25, **Branch:** perf/ci-test-acceleration

| Metric | Value |
|--------|-------|
| Total pass rate | 0/295 (0%) |
| Shard 1 | 0/96 (27 failed, 69 skipped) |
| Shard 2 | 0/103 (35 failed, 68 skipped) |
| Shard 3 | 0/96 (28 failed, 68 skipped) |
| Root cause | test-data-restore pre-install hook BackoffLimitExceeded |
| Deploy-mr | Failed (allow_failure=true, so e2e proceeded against broken app) |
| Build | Only site-api + acme-playwright built (others skipped, no matching tag) |

**Root cause:** ACK EKS controller in CrashLoopBackOff (missing Capability CRD). Pod Identity
credentials never provisioned for new namespace. **Fixed:** Applied CRD, controller recovered.

### Run 2 (Pipeline #2347934696) -- After ACK fix

**Date:** 2026-02-25, **Branch:** perf/ci-test-acceleration

| Metric | Value |
|--------|-------|
| Total pass rate | 2/295 (0.7%) |
| Shard 1 | 2/96 (24 failed, 70 skipped/not run) |
| Shard 2 | 0/103 (36 failed, 67 not run) |
| Shard 3 | 0/96 (28 failed, 68 not run) |
| Deploy-mr | **Success (209s)** |
| Root cause | 401 Unauthenticated from sponsor-api |
| Build | acme-playwright only (site-api cached) |

**Root cause:** test-data-restore (hook weight 4) runs in parallel with sponsor-api-setup (weight 4).
test-data-restore drops/recreates databases from S3 seeds, clobbering sponsor-api-setup's migrations
and Keycloak client configuration. See beads-mvq8 for tracking.
