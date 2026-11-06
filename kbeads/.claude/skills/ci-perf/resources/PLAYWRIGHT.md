# Playwright Test Analysis

## Test Structure

Tests live in `~/book/monorepo/tests/e2e/specs/` organized by numeric-prefixed modules:

```
specs/
  001-login/            # Login tests
  002-masterlist/       # Study/country/site/personnel
  003-form-builder/     # CRF form builder
  004-milestones/       # Milestone lists
  005-operational-forms/ # Operational forms
  006-date-format/      # Date handling
  009-admin/            # Audit export
  010-startup-sponsor/  # Sponsor startup
  020-conduct-sponsor/  # Sponsor conduct phase
  030-etmf/            # Electronic Trial Master File
  040-startup-site/    # Site startup
  050-conduct-site/    # CRF data entry
  060-isf/             # Investigator Site File
  070-coverage/        # API coverage (non-UI)
```

## Architecture Layers

1. **Fixtures** (`tests/e2e/fixtures/`): Faker-based data factories, seeded per-worker for determinism
2. **Page objects** (`tests/e2e/pages/`): All extend `BasePage` with MUI helpers, DataGrid, DatePicker
3. **API clients** (`tests/e2e/api/`): Extend `BaseApi` with CRUD + `waitForSync` (Kafka polling)
4. **Auth** (`tests/e2e/env/auth.ts`): Custom `test` fixture with shared browser context, Istanbul coverage
5. **Seed** (`tests/e2e/env/seed.ts`): `seedStudy` creates study hierarchy via API with retry

## CI Configuration

| Setting | Value | Source |
|---------|-------|--------|
| Workers per shard | 6 | `cicd/gitlab/e2e.yml` input `test_concurrency` |
| Shards | 3 | `cicd/gitlab/e2e.yml` input `parallelism` |
| Retries | 2 | `cicd/gitlab/e2e.yml` input `test_retry_count` |
| Test timeout | 90s (cloud) | `playwright.config.ts` + `env/vars.ts` |
| Assertion timeout | 45s (cloud) | `playwright.config.ts` |
| Action timeout | 45s (cloud) | `playwright.config.ts` |
| Browser | Chromium only | `playwright.config.ts` |
| Viewport | 1280x720 | `playwright.config.ts` |
| Screenshots | disabled (`E2E_FAST=true`) | `playwright.config.ts` |
| Video | disabled (`E2E_FAST=true`) | `playwright.config.ts` |
| Traces | disabled (`E2E_FAST=true`) | `playwright.config.ts` |
| Runner tag | `kube-x86-xlarge` | Self-hosted K8s runners |

## Shard Weighting

File: `~/book/monorepo/tests/e2e/shard-weights.json`

Shards are runtime-weighted (not round-robin). 77 spec files across 3 shards, ~2292s each (extremely well-balanced).

**Heaviest test files:**
1. `030-etmf/04-review-approval-workflow.spec.ts` -- 789s
2. `002-masterlist/41-edit-vendor.spec.ts` -- 498s
3. `030-etmf/05-upload-document-types.spec.ts` -- 483s
4. `002-masterlist/43-edit-site.spec.ts` -- 419s
5. `030-etmf/08-send-document-to-isf.spec.ts` -- 347s

With 6 workers: theoretical wall-clock ~382s (~6.4min) per shard before retries.

Shard stagger: `(shard - 1) * 15s` delay to avoid thundering herd.

## Reporting

**CI reporters (sharded):**
- Blob reporter for merge-friendly format
- JUnit XML per-shard: `playwright-junit-${CI_NODE_INDEX}.xml`
- ReportPortal (optional, when `RP_API_KEY` set)

**Local reporters:**
- HTML reporter at `playwright-report/`
- JUnit XML: `playwright-junit.xml`

**Report merging** (coverage-all job):
```bash
npx playwright merge-reports --reporter=html reports/blob-report
```

**Artifacts stored:** `${CI_PROJECT_DIR}/reports/` (blob-report, badges, backend-coverage, JUnit XML)

## Quarantine

File: `~/book/monorepo/tests/e2e/quarantine.json`

22 spec files quarantined (50 failing / 86 total tests). Most are ISF v2 503 errors from site-api.
Quarantine runs separately with `allow_failure: true`, `min_passing_tests: 0`.

## Timeouts Reference

| Timeout | Cloud | Local | Purpose |
|---------|-------|-------|---------|
| `TICK` | 15ms | 15ms | Minimal wait |
| `RERENDER` | 300ms | 150ms | React rerender |
| `ROUNDTRIP` | 3s | 750ms | Network roundtrip |
| `ANIMATION` | 1s | 500ms | CSS animation |
| `ASSERTION` | 45s | 5s | Single assertion |
| `PAGE` | 45s | 10s | Page load |
| `LOGIN` | 60s | 20s | OIDC auth |
| `DIALOG` | 20s | 20s | Dialog visibility |
| `SAVE` | 20s | 20s | Form save |
| `DOWNLOAD` | 60s | 60s | File export |
| `TEST` | 90s | 30s | Per-test |
| `TEST_EXTENDED` | 270s | 90s | Complex scenarios |
| `API_REQUEST` | 60s | 10s | Backend API |
| `SYNC_WAIT` | 120s | 10s | Kafka propagation |

## Error Handling Patterns

- **`withRetry`** (`env/retry.ts`): 3 attempts, exponential backoff (3s, 6s), for `beforeAll` blocks
- **`withAuthRetry`** (`env/seed.ts`): Re-acquires token on 401, retries on 409/500/503
- **`BaseApi.waitForSync`** (`api/base.ts`): Poll Kafka-synced data, 500ms intervals, up to SYNC_WAIT
- **Playwright retries**: 0 in config, overridden to 2 in CI via `--retries=2`

## Analyzing JUnit Results

```bash
# Parse JUnit XML for pass/fail counts
xmllint --xpath "//testsuite/@tests" reports/playwright-junit-1.xml
xmllint --xpath "//testsuite/@failures" reports/playwright-junit-1.xml

# Find slowest tests
xmllint --xpath "//testcase/@time" reports/playwright-junit-1.xml | tr ' ' '\n' | sort -t= -k2 -rn | head -20

# Find all failures with error messages
xmllint --xpath "//testcase[failure]/@name" reports/playwright-junit-1.xml
```
