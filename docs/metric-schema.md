# Gardenio Prometheus Metric Schema

Complete metric naming, types, labels, and emission points for gardenio cost and usage telemetry.

## Emission Points

Metrics are emitted from four sources, each serving a different purpose:

| # | Source | Endpoint/Method | Update Frequency | Scope |
|---|--------|----------------|-------------------|-------|
| 1 | **Controller** (`internal/metrics/`) | `/metrics` (pull) | Every 30s poll of bead store | Fleet-wide per-agent, per-account |
| 2 | **Coop** (`transport/http/metrics.rs`) | `/metrics` (pull) | Real-time per API response | Single session (per-pod) |
| 3 | **`gb session close`** (`session_metrics_push.go`) | Push to vmagent `/api/v1/import/prometheus` | Once at session end | End-of-session snapshot |
| 4 | **kbeads** (`http_metrics.go`) | `/metrics` (pull) | Cached 30s TTL | Fleet aggregates by project |

### Why multiple emission points?

- **Controller** (1) is the primary scrape target. VictoriaMetrics/Prometheus scrapes it on an interval. Provides the full per-agent and per-account breakdown.
- **Coop** (2) provides real-time session-level metrics from within agent pods. Useful for per-pod dashboards but requires pod-level scraping.
- **Push** (3) ensures end-of-session data lands even if the pod dies before the next scrape interval.
- **kbeads** (4) provides fleet-level rollups directly from the bead store. Useful as a secondary source or for deployments without a controller.

## Metric Registry

### Controller Metrics (emission point 1)

#### Fleet Gauges

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gardenio_agents_active` | Gauge | `project`, `role`, `state` | Number of active agents by project, role, and state |

#### Per-Agent Gauges

All per-agent metrics use labels: **`agent`**, **`project`**, **`role`**.

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_agent_cost_usd` | Gauge | Total cost in USD across all sessions |
| `gardenio_agent_input_tokens_current` | Gauge | Current input tokens consumed |
| `gardenio_agent_output_tokens_current` | Gauge | Current output tokens produced |
| `gardenio_agent_cache_read_tokens_current` | Gauge | Current cache-read tokens |
| `gardenio_agent_cache_write_tokens_current` | Gauge | Current cache-write tokens |
| `gardenio_agent_requests_current` | Gauge | Current API request count |
| `gardenio_agent_sessions_current` | Gauge | Current session count |
| `gardenio_agent_tool_calls_current` | Gauge | Current tool-call count |
| `gardenio_agent_errors_current` | Gauge | Current error count |
| `gardenio_agent_compactions_current` | Gauge | Current context-compaction count |

> **Note on type and suffix**: These are Gauges, not Counters, because they are populated from bead field snapshots (point-in-time reads), not monotonically incremented. They use the `_current` suffix rather than `_total`: the Prometheus convention reserves `_total` for monotonic counters, so a polled gauge that can also fall (e.g. when an agent bead is reset) must not carry it. (Renamed from `*_total` under E2.1, epic ka-0000002391.)

#### Per-Account Gauges

All per-account metrics use labels: **`account`**, **`env`**. The `env` label
is sourced from the account bead's `env` field, falling back to the
controller's `GARDENIO_ENV` (the deployment environment), then `unknown`. It
prevents the same account name from aliasing across prod / gardenio-next /
gardenio-rwx on cross-environment dashboards (E2.2, epic ka-0000002391).

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_account_cost_usd` | Gauge | Total cost in USD |
| `gardenio_account_sessions_current` | Gauge | Current session count |
| `gardenio_account_input_tokens_current` | Gauge | Current input tokens |
| `gardenio_account_output_tokens_current` | Gauge | Current output tokens |
| `gardenio_account_cache_read_tokens_current` | Gauge | Current cache-read tokens |
| `gardenio_account_cache_write_tokens_current` | Gauge | Current cache-write tokens |
| `gardenio_account_requests_current` | Gauge | Current API request count |

#### Tenant Land Credit Gauges

Label: **`land`**. One series per tenant land (an operator land has none),
re-read every 5 minutes from the beads the purse reads
(`internal/metrics/lands_credits.go`). The vmalert rules in
`helm/gardenio/templates/_lands-credits.tpl` turn the land's USD into credits
with them (`gardenio:land_spend_credits:*`, `gardenio:land_credits:*`).

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_lands_credit_rate` | Gauge | Credits per US$1: the `llm_multiplier` of the credit rate in force. Absent when no rate is in force |
| `gardenio_lands_credits_granted` | Gauge | The sum of the land's `credit_grant` ledger |

#### Per-Account Rate-Limit Gauges

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_account_utilization_5h` | Gauge | 5-hour window utilization ratio (0.0-1.0) |
| `gardenio_account_utilization_7d` | Gauge | 7-day window utilization ratio (0.0-1.0) |
| `gardenio_account_throttled` | Gauge | 1 if rate-limited, 0 if allowed |
| `gardenio_account_warming` | Gauge | 1 if in soft-failover warming, 0 if normal |

#### Scrape Metadata

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_metrics_last_scrape_timestamp_seconds` | Gauge | Unix timestamp of last successful scrape |
| `gardenio_metrics_scrape_errors_total` | Counter | Total scrape errors |

### Reconciler Metrics (emission point 1, registered alongside controller)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gardenio_agents_deferred_total` | Gauge | `project`, `reason` | Agents deferred (waiting for pod capacity) per reconcile pass |
| `gardenio_agent_deferred_wait_seconds` | Histogram | `project` | Time since bead creation for each deferred agent |

Histogram buckets: `[60, 300, 600, 1800, 3600, 7200, 14400]` seconds.

### Coop Per-Session Metrics (emission point 2)

All metrics use labels: **`agent_name`**, **`project`**, **`account`**.

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_session_input_tokens_total` | Counter | Cumulative input tokens this session |
| `gardenio_session_output_tokens_total` | Counter | Cumulative output tokens this session |
| `gardenio_session_cache_read_tokens_total` | Counter | Cumulative cache-read tokens this session |
| `gardenio_session_cache_write_tokens_total` | Counter | Cumulative cache-write tokens this session |
| `gardenio_session_cost_usd` | Gauge | Cumulative cost in USD this session |
| `gardenio_session_request_count_total` | Counter | Cumulative API requests this session |
| `gardenio_session_api_ms_total` | Counter | Cumulative API latency in milliseconds |

### Session Close Push Metrics (emission point 3)

All metrics use labels: **`agent_name`**, **`project`**, **`account`**, **`session_id`**.

| Metric | Type | Description |
|--------|------|-------------|
| `gardenio_session_tokens_total` | Gauge | Final token count (with `type` sub-label: `input`, `output`, `cache_read`, `cache_write`) |
| `gardenio_session_duration_seconds` | Gauge | Total session duration |
| `gardenio_session_request_count_total` | Gauge | Total API requests |
| `gardenio_session_tool_calls_total` | Gauge | Total tool calls |
| `gardenio_session_errors_total` | Gauge | Total errors |
| `gardenio_session_compactions_total` | Gauge | Total context compactions |

> **No cost metric is pushed here (ka-0000000999).** The gb session-close push
> formerly emitted `gardenio_session_cost_usd` and the operator-keyed
> `gardenio_operator_cost_usd_total` from the in-pod JSONL `response_cost`
> estimate — a low-biased, never-reconciled value (turns missing `response_cost`
> contribute 0). `gardenio_session_cost_usd` was dropped entirely; the
> authoritative per-session/per-agent cost is the controller-emitted
> `gardenio_agent_spend_usd_total` (and operator rollup
> `gardenio_operator_cost_usd_total`), both reconciled from LiteLLM `/spend/tags`
> at session close in `reconcile_session_spend.go`. The session-close push still
> emits the exact token/duration/count series above (`gardenio_operator_session_duration_seconds`
> too — duration is in-pod-exact, not an estimate).

### kbeads Fleet Metrics (emission point 4)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `gardenio_beads_total_cost_usd` | Gauge | `project` | Total accumulated cost for agents in a project |
| `gardenio_beads_active_agents` | Gauge | `project` | Active agents (open or in_progress) by project |
| `gardenio_beads_sessions_total` | Counter | `project` | Total agent sessions by project |
| `gardenio_beads_total` | Gauge | `type`, `status` | Total beads by type and status |
| `gardenio_beads_fleet_metrics_staleness_seconds` | Gauge | — | Age of the fleet snapshot (active agents, beads totals, queue gauges); refreshed every 30 s |
| `gardenio_beads_fleet_lifetime_metrics_staleness_seconds` | Gauge | — | Age of the agent lifetime rollup behind `total_cost_usd` and `sessions_total`; refreshed every 5 min |

## Label Dictionary

| Label | Cardinality | Used By | Description |
|-------|-------------|---------|-------------|
| `agent` / `agent_name` | High | Controller, Coop, Push | Agent name (e.g., `crew-gardenio-my-task-abc`) |
| `project` | Low (~5) | All | Project name (e.g., `gardenio`, `monorepo`) |
| `account` | Low (~10) | Controller, Coop, Push | API credential account name |
| `role` | Low (2) | Controller | Agent role: `crew`, `gardener` |
| `state` | Low (~5) | Controller | Agent state: `working`, `yielding`, `idle`, `unknown` |
| `session_id` | High | Push only | Session bead ID (e.g., `ka-ABC123`) |
| `type` | Medium (~15) | Push, kbeads | Token type or bead type |
| `status` | Low (4) | kbeads | Bead status: `open`, `in_progress`, `deferred`, `closed` |
| `reason` | Low (~3) | Reconciler | Deferral reason |
| `model` | Low (~5) | (proposed) | Model ID (e.g., `claude-opus-4-6`) — not yet emitted |

### Cardinality Notes

- **`agent` / `agent_name`**: High cardinality (~50-200 concurrent, ~1000+ lifetime). Acceptable for VictoriaMetrics which handles high cardinality well. Controller resets gauges per-scrape so departed agents disappear.
- **`session_id`**: Only used in the push path (emission point 3), not scraped. Each session pushes once, so cardinality is bounded by total sessions over the retention window.
- **`model`**: Proposed in the original task but not yet implemented in any emission point. Adding it to controller per-agent metrics would require tracking model in agent bead fields. Recommended for future work.

## Naming Conventions

All metrics follow [Prometheus naming best practices](https://prometheus.io/docs/practices/naming/):

- **Namespace**: `gardenio_` prefix on all metrics
- **Subsystem**: `session_`, `agent_`, `account_`, `fleet_`, `agents_` as appropriate
- **Unit suffix**: `_usd` (dollars), `_seconds` (duration), `_total` (cumulative count), `_ms` (milliseconds)
- **Base units**: Seconds for duration (not minutes/hours), USD for cost (not cents)

## Scraping Configuration

### Controller (primary)

```yaml
# Helm values.yaml
agents:
  metrics:
    serviceMonitor:
      enabled: true
      interval: 30s
```

Pod annotations (always present):
```yaml
prometheus.io/scrape: "true"
prometheus.io/port: "8091"
prometheus.io/path: "/metrics"
```

### Operator-Label PII Allow-List (defense-in-depth)

Operator-keyed and security metrics (`gardenio_operator_*`, `gardenio_decisions_lifecycle_total`,
`gardenio_escalations_total`, `gardenio_user_request_latency_seconds`) must only carry labels
from the bounded allow-list `{operator_id, project, role, action, reason, bead_type,
activity_type, surface, provider, status, revocation_type, event, urgency, outcome,
route_template, endpoint, status_code, model, escalated_by_role}`. Person-identifying labels
(`oidc_email`, `slack_user_id`, `display`, `raw_url`, `session_id`-on-operator-counters) are
forbidden — metric labels are the worst place to leak PII (they explode cardinality and persist
for the full retention window).

Three independent layers enforce this (W2 §6, `gardenio/docs/research/obs-centralization-w2-pii.md`):

1. **pii-gateway catalog** (`observability/pii-gateway/catalog.yaml`) — downstream OTEL
   catch-all that hashes any email-shaped operator label (`pii-direct.operator.email`,
   action=hash) and drops the deny-list labels before the common collector.
2. **vmagent metric_relabel backstop** (`gardenio/helm/gardenio/templates/vmagent/configmap.yaml`)
   — `labeldrop`s the forbidden labels and drops any operator series carrying `session_id`
   before remote_write to vmsingle.
3. **gb producer-side sanitiser** (`gardenio/controller/internal/obspii`) — the SSOT allow/deny
   lists + `SanitizeOperatorLabelValue`, which HMAC-hashes an accidentally-emitted email at the
   session-push boundary. CI tests fail if a forbidden label is introduced on any operator metric.

### VictoriaMetrics Long-Term Storage

```yaml
# kube-prometheus-stack remoteWrite config
prometheus:
  prometheusSpec:
    remoteWrite:
      - url: http://<release>-vmsingle.<namespace>.svc:8428/api/v1/write
        writeRelabelConfigs:
          - sourceLabels: [__name__]
            regex: 'gardenio_.*'
            action: keep
```

### vmagent Push (session close)

Set `VMAGENT_URL` on agent pods. `gb session close` pushes to `$VMAGENT_URL/api/v1/import/prometheus`.

## Proposed Additions (Not Yet Implemented)

The original task description proposed metrics not yet present in the codebase:

| Proposed Metric | Status | Notes |
|----------------|--------|-------|
| `gardenio_session_input_tokens_total{model}` | Partial | Coop emits per-session but without `model` label |
| `gardenio_session_cost_usd_total{model}` | Partial | Push emits but without `model` label |
| `gardenio_session_final_cost_usd_bucket` (Histogram) | Not implemented | Requires histogram aggregation at session close |
| `gardenio_active_agents{project, role}` | Implemented | As `gardenio_agents_active{project, role, state}` |
| `gardenio_session_duration_seconds` | Implemented | In push path |
| `gardenio_session_api_latency_ms` | Implemented | As `gardenio_session_api_ms_total` in Coop |

### Recommended Next Steps

1. **Add `model` label** to push metrics and controller per-agent metrics (requires tracking model in agent bead fields)
2. **Add histogram** `gardenio_session_final_cost_usd` at session close for cost distribution analysis
3. **Normalize label names** across emission points (`agent` vs `agent_name` — pick one)
4. **`_total` suffix consistency** — DONE (ka-0000006272): the coop per-session counters now carry the `_total` suffix (`gardenio_session_input_tokens_total`, `…_output_tokens_total`, `…_cache_read_tokens_total`, `…_cache_write_tokens_total`, `…_request_count_total`). The push path still emits `gardenio_session_request_count_total` only as a one-release alias for `gardenio_session_turns_total`.
