# LiteLLM Production Cutover Runbook

Reversible cutover plan for migrating production gardenio to the bundled LiteLLM proxy.

For per-environment health checks on the AWS POC (egress + OAuth-forwarding probes),
see [`litellm-poc-smoke-runbook.md`](./litellm-poc-smoke-runbook.md).

## Table of Contents

- [Overview](#overview)
- [Architecture](#architecture)
- [Pre-Cutover Checklist](#pre-cutover-checklist)
- [Cutover Steps](#cutover-steps)
- [Rollback Steps](#rollback-steps)
- [Monitoring](#monitoring)
- [Appendix: Key References](#appendix-key-references)

---

## Overview

The bundled LiteLLM proxy runs inside the gardenio namespace as a Kubernetes deployment.
It acts as a reverse proxy to Anthropic's API, providing:

- **Cost tracking** via virtual keys and the spend_sync loop (5-minute interval)
- **Model alias routing** so Claude Code's internal model names resolve to OAuth-enabled model groups
- **Per-account budget visibility** via `total_cost_usd`, `daily_cost_usd`, `weekly_cost_usd` fields on account beads

Cutover means switching production agents from their current LLM endpoint (external LiteLLM or
direct Anthropic) to the in-cluster bundled LiteLLM proxy.

## Architecture

```
Agent Pod
  │
  │  ANTHROPIC_BASE_URL = http://gardenio-litellm:4000
  │  x-litellm-api-key: Bearer <virtual_key>
  │  Authorization: Bearer <OAuth token>
  │
  ▼
┌──────────────────────┐
│  LiteLLM Proxy       │  (gardenio-litellm:4000)
│  - Model routing     │
│  - Spend tracking    │
│  - Client header     │
│    forwarding        │
└──────────┬───────────┘
           │  Authorization: Bearer <OAuth token>  (forwarded)
           ▼
    Anthropic API (api.anthropic.com)
```

**Data flow for cost tracking:**

```text
LiteLLM DB (postgres:litellm)
  │
  │  GET /key/info?key=<virtual_key>
  │  GET /spend/logs?api_key=<sha256_hash>
  │
  ▼
Controller (spend_sync loop, every 5 min)
  │
  │  PATCH account bead fields:
  │    total_cost_usd, daily_cost_usd, weekly_cost_usd
  │
  ▼
Account Beads (kbeads)
```

## Pre-Cutover Checklist

Complete all items before proceeding with the production cutover.

### Staging Validation (gardenio-rwx)

- [ ] **Bundled LiteLLM healthy in gardenio-rwx for >= 7 days** without restarts or OOM kills.
  Check: `kubectl get pods -n gardenio-rwx -l app.kubernetes.io/component=litellm --show-labels`
- [ ] **E2E tests green** — the `litellm-cost-tracking` test module passes consistently.
  Check: `kd list --type=molecule -l formula:litellm-cost-tracking --status=closed`
- [ ] **Cost tracking validated against Anthropic billing** — compare `total_cost_usd` from
  account beads in gardenio-rwx against actual Anthropic Console usage for the same period.
  Allow up to 5% variance due to timing differences.
- [ ] **Model alias sync working** — controller logs show `"synced model aliases"` at startup,
  no warnings about missing aliases. Check:
  `kubectl logs -n gardenio-rwx deploy/gardenio-rwx-controller | grep -i "model alias"`
- [ ] **No spend_sync errors** — controller logs show successful spend sync cycles.
  Check: `kubectl logs -n gardenio-rwx deploy/gardenio-rwx-controller | grep -i "spend"`

### Production Prerequisites

- [ ] **PostgreSQL has capacity** for the `litellm` database. The bundled LiteLLM uses the
  shared postgres instance (`litellm.database: litellm`). Verify postgres disk and connection
  limits are adequate.
- [ ] **LiteLLM master key secret exists** in the production namespace. Either:
  - An ExternalSecret is configured to sync from AWS Secrets Manager, or
  - The secret is created manually:
    ```bash
    kubectl create secret generic gardenio-litellm-master-key \
      --from-literal=token="$(openssl rand -hex 32)" \
      -n gardenio
    ```
- [ ] **Virtual keys prepared** for each production account. Generate via LiteLLM admin API
  after the proxy is running (see [Step 2: Create Virtual Keys](#step-2-create-virtual-keys)).
- [ ] **Maintenance window communicated** — cutover will briefly disrupt in-flight agent
  sessions when their `base_url` changes. Schedule during a low-activity period.

---

## Cutover Steps

### Step 1: Enable LiteLLM in Production Helm Values

In the production values file (`acme-helm-chart/charts/gardenio/values/gardenio.yaml`), add:

```yaml
litellm:
  enabled: true

  # Use defaults for image, port, and database unless overrides are needed.
  # Key defaults:
  #   image: ghcr.io/berriai/litellm:main-latest
  #   port: 4000
  #   database: litellm
  #   forwardClientHeaders: true  (required for OAuth forwarding)
  #   dbInit.enabled: true        (creates DB on first deploy)

  nodeSelector:
    kubernetes.io/arch: amd64

  resources:
    requests:
      cpu: 200m
      memory: 1Gi
    limits:
      cpu: "2"
      memory: 2Gi
```

Deploy the Helm update:

```bash
# Via the standard release pipeline (preferred):
# Push the values change to acme-helm-chart and trigger a deploy.

# Or via manual Helm upgrade:
cd acme-helm-chart/charts/gardenio
helm dependency update
helm upgrade gardenio ./ -n gardenio \
  --values values/gardenio.yaml \
  --wait --timeout 5m
```

**Verify LiteLLM is running:**

```bash
kubectl get pods -n gardenio -l app.kubernetes.io/component=litellm
kubectl logs -n gardenio deploy/gardenio-litellm --tail=20
# Expect: "LiteLLM Proxy running on http://0.0.0.0:4000"

# Health check:
kubectl exec -n gardenio deploy/gardenio-litellm -- \
  curl -sf http://localhost:4000/health/readiness
# Expect: {"status": "healthy"}
```

**Verify database created:**

```bash
kubectl exec -n gardenio deploy/gardenio-postgres -- \
  psql -U postgres -c "\l" | grep litellm
# Expect: litellm | postgres | UTF8 | ...
```

> **At this point, LiteLLM is running but no agents are using it yet.** The controller
> will auto-detect `LITELLM_URL` (auto-wired from `litellm.enabled=true`) and start the
> spend_sync loop and model alias sync. But agents still use their existing `base_url`.

### Step 2: Create Virtual Keys

For each production account bead, generate a LiteLLM virtual key:

```bash
# Get the LiteLLM master key
MASTER_KEY=$(kubectl get secret -n gardenio gardenio-litellm-master-key \
  -o jsonpath='{.data.token}' | base64 -d)

# Port-forward to LiteLLM (if not using ingress)
kubectl port-forward -n gardenio svc/gardenio-litellm 4000:4000 &

# Generate a virtual key for each account. The `models` allow-list MUST be the
# UNION of every model any role/project/agent pinned to this account may
# request — see "Model allow-list rule" below.
curl -sf http://localhost:4000/key/generate \
  -H "Authorization: Bearer ${MASTER_KEY}" \
  -H "Content-Type: application/json" \
  -d '{
    "key_alias": "account-<ACCOUNT_NAME>",
    "models": ["claude-opus-5-oauth", "claude-opus-4-8-oauth", "claude-opus-4-7-oauth", "claude-opus-4-6-oauth", "claude-sonnet-5-oauth", "claude-sonnet-4-6-oauth", "claude-fable-5-1-oauth", "claude-fable-5-oauth", "anthropic/claude-haiku-4-5"],
    "max_budget": null
  }'
# Response includes: {"key": "sk-..."}
```

Record each virtual key — you will need it in the next step.

#### Model allow-list rule (multi-model accounts)

> **Rule:** the per-account virtual key's `models` allow-list MUST be the
> **UNION** of `account.model` **plus every `model` override** set on any
> role, project, or agent that routes through that account.

Since epic [ka-0000005488], the model is no longer baked onto the `account`
bead alone — a `model` field on a **role**, **project**, or **agent** overrides
the account default under the precedence `global < project < role < agent`
(see `cmd/controller/podspec.go` `applyModelOverride`). One `gardenio` account
(one OAuth credential, one LiteLLM key) can therefore back fable / opus / sonnet
roles by setting `model` on the role bead — no account cloning.

For that to work, the account's LiteLLM virtual key must **authorize** every
model those overrides may request. LiteLLM enforces the key's `models` list: a
request for a model not on the list returns `401 key_model_access_denied`. So
the allow-list has to be the union of all referenced models, not just the
account default.

**Concrete rule for a key backing account `A`:**

```
key(A).models  ⊇  { account_A.model }
                ∪  { role.model    for every role    using A }
                ∪  { project.model for every project using A }
                ∪  { agent.model   for every agent   pinned to A }
```

**Implementation:** `gardenio/scripts/seed-litellm-virtual-keys.sh` already
provisions every account key with the wide union list
(`claude-opus-5-oauth`, `claude-opus-4-8-oauth`, `claude-opus-4-7-oauth`,
`claude-opus-4-6-oauth`, `claude-sonnet-5-oauth`, `claude-sonnet-4-6-oauth`,
`claude-fable-5-1-oauth`, `claude-fable-5-oauth`,
`anthropic/claude-haiku-4-5`),
matching the design decision to keep a single wide per-account key rather than
minting per-`(account, model)` keys. When you add a **new** model that a role or
project will pin (e.g. a future `claude-*-oauth`), add it to:

1. the `generate_key` body in `gardenio/scripts/seed-litellm-virtual-keys.sh`,
2. the manual `/key/generate` snippets in this runbook, and
3. `claudeCodeModelAliases` in `cmd/controller/litellm_aliases.go` if Claude
   Code sends an undated alias for it,

then re-seed the affected keys with `--force` so the new model is authorized.

**Failure mode:** if a role pins a model the account key does not list, the pod
comes up but the agent's first LLM call 401s with `key_model_access_denied`
(this is exactly what blocked the first fable rollout). There is no controller
reconciler that auto-syncs the key allow-list to referenced models today; keep
the seed-script union and this runbook in lockstep when introducing a new model.
A future fail-loud guard (CLI check that flags an account key missing a model a
referencing role requests) is tracked under [ka-0000007012].

### Step 3: Update Account Beads

For each production account, update three fields:

```bash
# Set base_url to the in-cluster LiteLLM service
# Set litellm_key to the virtual key generated above
kd update <account-bead-id> \
  -f 'base_url=http://gardenio-litellm:4000' \
  -f 'litellm_key=sk-...'
```

**Important:** The controller's credential sync loop picks up account bead changes via SSE
events. Existing running agent pods will receive updated credentials on the next sync cycle
(within ~60 seconds). New pods will inherit the updated `base_url` immediately.

**Cutover order — do one account at a time:**

1. Pick the lowest-traffic account first (canary).
2. Update its `base_url` and `litellm_key`.
3. Watch for 5-10 minutes (see [Monitoring](#monitoring)).
4. If healthy, proceed to the next account.
5. Repeat until all accounts are migrated.

### Step 4: Update Controller LITELLM_URL (if using explicit override)

If production uses `agents.litellmURL` as an explicit override (rather than auto-wiring from
`litellm.enabled`), update it to point to the bundled service:

```yaml
# In production values:
agents:
  litellmURL: "http://gardenio-litellm:4000"
```

If `litellm.enabled: true` is set and `agents.litellmURL` is empty, the Helm chart auto-wires
`LITELLM_URL` to `http://gardenio-litellm:4000` — no manual override needed.

### Step 5: Verify Cutover

```bash
# 1. Check controller detected LiteLLM
kubectl logs -n gardenio deploy/gardenio-controller | grep -i "litellm"
# Expect: "spend sync started", "synced model aliases"

# 2. Check spend_sync is updating account beads
kd list --type=account --status=open
# For each account, verify cost fields are populated:
kd show <account-id> --json | jq '.fields | {total_cost_usd, daily_cost_usd, weekly_cost_usd}'

# 3. Check a running agent is routing through LiteLLM
kubectl logs -n gardenio <agent-pod> | grep -i "base_url\|litellm"

# 4. Verify LiteLLM is receiving traffic
kubectl logs -n gardenio deploy/gardenio-litellm --tail=50
# Expect: request logs showing model calls
```

---

## Rollback Steps

Rollback can be performed at any point during or after cutover. Each step is independent —
you can rollback a single account or all accounts.

### Rollback a Single Account (Instant)

Revert the account bead to its previous endpoint:

```bash
# Option A: Point back to external LiteLLM
kd update <account-bead-id> \
  -f 'base_url=https://external-litellm.example.com' \
  -f 'litellm_key='

# Option B: Point directly to Anthropic (no proxy)
kd update <account-bead-id> \
  -f 'base_url=' \
  -f 'litellm_key='
```

Setting `base_url` to empty causes agents to use the Anthropic API directly (no proxy).
Setting `litellm_key` to empty disables virtual key auth headers.

The controller's credential sync propagates changes to running agents within ~60 seconds.

### Rollback All Accounts

Repeat the single-account rollback for every account bead. Script it:

```bash
# List all account beads
for id in $(kd list --type=account --status=open --json | jq -r '.[].id'); do
  echo "Rolling back $id..."
  kd update "$id" -f 'base_url=' -f 'litellm_key='
done
```

### Rollback Controller LITELLM_URL

If you explicitly set `agents.litellmURL`, revert it:

```yaml
# In production values:
agents:
  litellmURL: "https://external-litellm.example.com"  # or "" to disable
```

Deploy the Helm update. The controller will restart and stop the spend_sync loop if
`LITELLM_URL` becomes empty.

### Disable Bundled LiteLLM (Optional)

If rolling back completely, disable the LiteLLM deployment to free resources:

```yaml
litellm:
  enabled: false
```

Deploy the Helm update. This removes the LiteLLM Deployment, Service, and ConfigMap.
The `litellm` database in postgres is **not** deleted — cost data is preserved for
future reference.

### Rollback Verification

```bash
# 1. Confirm agents are no longer routing through bundled LiteLLM
kubectl logs -n gardenio <agent-pod> | grep -i "base_url"
# Should show the reverted URL (or empty for direct Anthropic)

# 2. Confirm LiteLLM is not receiving traffic (if still running)
kubectl logs -n gardenio deploy/gardenio-litellm --tail=20
# Should show no new request logs after rollback

# 3. Confirm agents can still make API calls
# Check agent session logs for successful Claude interactions
kubectl logs -n gardenio <agent-pod> --tail=50
```

---

## Monitoring

### During Cutover — What to Watch

| Signal | How to Check | Healthy | Unhealthy |
|--------|-------------|---------|-----------|
| Agent API errors | `kubectl logs -n gardenio <agent-pod> \| grep -i error` | No new API errors | 401/403/5xx errors from LiteLLM |
| LiteLLM latency | `kubectl logs -n gardenio deploy/gardenio-litellm \| grep -i latency` | < 200ms overhead | > 1s overhead or timeouts |
| LiteLLM health | `kubectl exec ... curl http://localhost:4000/health/readiness` | `{"status":"healthy"}` | Non-200 or timeout |
| Cost data gaps | `kd show <account-id>` — check `total_cost_usd` | Updates every 5 min | Stale for > 15 min |
| Pod restarts | `kubectl get pods -n gardenio -l app.kubernetes.io/component=litellm` | RESTARTS = 0 | Increasing restarts |
| Controller spend_sync | `kubectl logs ... deploy/gardenio-controller \| grep spend` | `"spend sync completed"` | `"spend sync: query failed"` |
| Model alias sync | Controller logs at startup | `"synced model aliases"` | `"failed to sync aliases"` |

### Declaring Success

Wait **at least 24 hours** after full cutover before declaring success. Criteria:

1. **Zero agent API errors** caused by the LiteLLM proxy (excluding pre-existing issues).
2. **Cost data updating** — `total_cost_usd` on all accounts reflects real spend.
3. **Cost accuracy** — daily spend within 5% of Anthropic Console billing.
4. **No LiteLLM pod restarts** in the last 24 hours.
5. **spend_sync loop healthy** — no persistent errors in controller logs.

### Post-Cutover Operational Notes

- **LiteLLM upgrades**: The image tag `main-latest` tracks LiteLLM's latest release. Pin
  to a specific tag (e.g., `v1.55.0`) in production for stability. Update via Helm values.
- **Database maintenance**: LiteLLM stores spend logs in postgres. Monitor table sizes with:
  ```bash
  kubectl exec -n gardenio deploy/gardenio-postgres -- \
    psql -U postgres -d litellm -c "SELECT relname, pg_size_pretty(pg_total_relation_size(relid)) FROM pg_catalog.pg_statio_user_tables ORDER BY pg_total_relation_size(relid) DESC LIMIT 10;"
  ```
- **Virtual key rotation**: Generate new keys periodically. Update the account bead
  `litellm_key` field — agents pick up the change automatically via credential sync.

---

## Appendix: Key References

### Helm Values

| Value | Purpose | Default |
|-------|---------|---------|
| `litellm.enabled` | Master switch for bundled LiteLLM | `false` |
| `litellm.port` | Proxy listen port | `4000` |
| `litellm.database` | Postgres database name | `litellm` |
| `litellm.masterKeySecret` | K8s secret with master key | `<release>-litellm-master-key` |
| `litellm.forwardClientHeaders` | Forward OAuth tokens to Anthropic | `true` |
| `litellm.dbInit.enabled` | Auto-create database | `true` |
| `agents.litellmURL` | Explicit controller LITELLM_URL override | `""` (auto-wires when litellm.enabled) |

### Controller Environment Variables

| Variable | Source | Purpose |
|----------|--------|---------|
| `LITELLM_URL` | Helm auto-wire or explicit | LiteLLM proxy URL for spend_sync and fallback |
| `LITELLM_MASTER_KEY` | K8s secret | Auth for LiteLLM admin API (/key/info, /spend/logs) |
| `LITELLM_ADMIN_SECRET` | K8s secret name | Secret containing admin key for model alias sync |

### Account Bead Fields

| Field | Type | Purpose |
|-------|------|---------|
| `base_url` | string | LLM API endpoint (e.g., `http://gardenio-litellm:4000`) |
| `litellm_key` | string | Virtual key for spend tracking |
| `total_cost_usd` | number | Cumulative spend (updated by spend_sync) |
| `daily_cost_usd` | number | Today's spend (updated by spend_sync) |
| `weekly_cost_usd` | number | This week's spend (updated by spend_sync) |

### Agent Pod Environment

| Variable | Source | Purpose |
|----------|--------|---------|
| `ANTHROPIC_BASE_URL` | Account bead `base_url` | Routes agent API calls through LiteLLM |
| `ANTHROPIC_CUSTOM_HEADERS` | Account bead `litellm_key` | `x-litellm-api-key: Bearer <key>` |

### LiteLLM Admin API Endpoints

| Endpoint | Method | Auth | Purpose |
|----------|--------|------|---------|
| `/health/readiness` | GET | None | Health check |
| `/key/generate` | POST | Master key | Create virtual key |
| `/key/info?key=<vk>` | GET | Master key | Get cumulative spend for a key |
| `/spend/logs?api_key=<hash>` | GET | Master key | Get spend entries by date range |
| `/get/config/callbacks` | GET | Admin key | Read current router config |
| `/config/update` | POST | Admin key | Update model aliases |

### Key Source Files

| File | Purpose |
|------|---------|
| `gardenio/helm/gardenio/values.yaml` (line 2062) | LiteLLM Helm defaults |
| `gardenio/helm/gardenio/templates/litellm/` | Deployment, Service, ConfigMap templates |
| `gardenio/controller/cmd/controller/spend_sync.go` | Cost tracking loop |
| `gardenio/controller/cmd/controller/litellm_aliases.go` | Model alias sync |
| `gardenio/controller/cmd/controller/podspec.go` | Agent env var injection |
| `gardenio/controller/internal/config/config.go` (line 278) | LITELLM_URL config |
| `gardenio/tests/e2e/scripts/test-litellm-cost-tracking.sh` | E2E test module |

---

## Appendix: AWS POC Enablement (epic ka-0000004609)

The AWS POC environment (`values-aws-poc.yaml.tftpl`) enables the bundled LiteLLM
proxy via the tiered rollout in epic ka-0000004609:

| Tier | Bead | Change |
|------|------|--------|
| h1 | ka-0000001624 | ExternalSecret template for in-chart master-key + salt-key |
| h2 | ka-0000005272 | `litellm.enabled: true` + `externalSecrets.enabled: true` in the POC overlay; DB wiring via `postgresExternal` (RDS Aurora) |
| h4 | ka-0000002922 | Explicit `agents.litellmURL: "http://gardenio-litellm:4000"` on the controller deployment + this runbook section |

After tier h2 + h4 land and `terraform apply` propagates the rendered values:

- The chart materializes K8s Secret `gardenio-litellm-master-key` with `master-key`
  + `salt-key` from AWS Secrets Manager (`/gardenio/aws-poc/litellm/{master-key,salt-key}`).
- The `gardenio-litellm` pod boots against the POC Aurora `litellm` DB.
- The controller starts with `LITELLM_URL=http://gardenio-litellm:4000` and engages
  `spend_sync` (only when `LITELLM_MASTER_KEY` is also injected — see
  `externalSecrets.litellmMasterKey` for the prod-style sync) and
  `litellm_aliases` (always, against the in-cluster URL).

### Seeding virtual keys per account bead (operator-driven)

Once the proxy is healthy, generate a per-account virtual key and stamp the
account bead so agents route through the proxy with cost attribution.

Use the helper script (idempotent, skips accounts whose `litellm_key` is
already set unless `--force` is passed):

```bash
# From the monorepo checkout, with kubectl context pointing at the POC cluster:
gardenio/scripts/seed-litellm-virtual-keys.sh --dry-run                # preview
gardenio/scripts/seed-litellm-virtual-keys.sh                          # seed all open accounts
gardenio/scripts/seed-litellm-virtual-keys.sh --account my-account     # one bead
gardenio/scripts/seed-litellm-virtual-keys.sh --force                  # rotate existing keys
```

The script:

1. Pulls the LiteLLM master key from the in-cluster Secret
   (`gardenio-litellm-master-key`, key `master-key` or `token`).
2. Execs `curl` inside the running `gardenio-litellm` pod against
   `http://127.0.0.1:4000/key/generate` (no port-forward required).
3. Writes `base_url=http://gardenio-litellm:4000` and `litellm_key=sk-...` onto
   each open account bead via `kd update`.

The controller's credential sync loop picks up the bead changes via SSE and
propagates new credentials to running agents within ~60s.

Equivalent manual flow (for one-off operations or air-gapped debugging):

```bash
MASTER_KEY=$(kubectl get secret -n gardenio gardenio-litellm-master-key \
  -o jsonpath='{.data.master-key}' | base64 -d)

LITELLM_POD=$(kubectl -n gardenio get pods \
  -l app.kubernetes.io/component=litellm \
  -o jsonpath='{.items[0].metadata.name}')

VKEY=$(kubectl -n gardenio exec "$LITELLM_POD" -c litellm -- \
  curl -sf -X POST http://127.0.0.1:4000/key/generate \
    -H "Authorization: Bearer ${MASTER_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"key_alias":"account-foo","models":["claude-opus-5-oauth","claude-opus-4-8-oauth","claude-opus-4-7-oauth","claude-opus-4-6-oauth","claude-sonnet-5-oauth","claude-sonnet-4-6-oauth","claude-fable-5-1-oauth","claude-fable-5-oauth","anthropic/claude-haiku-4-5"]}' \
  | jq -r .key)

kd update <account-bead-id> \
  -f "base_url=http://gardenio-litellm:4000" \
  -f "litellm_key=${VKEY}"
```

### POC-specific notes

- Smoke-testing is operator-driven (Slack thread ka-0000004785). Agents must
  not autonomously smoke against the POC environment.
- The `agents.litellmURL` override in the POC overlay is intentionally
  explicit even though `litellm.enabled: true` already auto-wires the same
  value via the `gardenio.litellm.fullname` helper. The explicit value
  shields the controller from helper-name drift and makes the controller
  deployment readable without re-rendering helpers.
- Tier h5/h6 (separately tracked) covers account-bead labelling rollout and
  cost dashboard wiring — out of scope for the h4 change.
