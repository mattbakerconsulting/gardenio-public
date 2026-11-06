# R3 — Per-agent LLM spend attribution & ingestion

> Bead: ka-0000001029 · Parent: ka-0000007704 (umbrella time-series epic)
> Sibling: ka-0000007788 (R2 — metric taxonomy / instrumentation plan)

## TL;DR

**Spend per agent already exists end-to-end** — it just isn't emitted as a
per-hour time series yet. LiteLLM is the authoritative cost source; the
controller already pushes `gardenio_session_cost_usd` to vmagent at
session-close, and aggregates `gardenio_agent_cost_usd{agent,project,role}`
as a cumulative gauge. The shortest path to v1 ("spend per agent per hour")
is **Option C — hybrid**:

1. **Live signal (counter)**: enrich coop's existing per-turn NATS
   `messages.{actor}.usage` event with `model` and `response_cost`, add a
   NATS→vmagent forwarder in the controller, and emit
   `gardenio_agent_spend_usd_total{agent,project,role,model,account}` as a
   Prometheus counter. Per-hour spend = `increase(...[1h])`.
2. **Reconciliation backstop**: keep the existing 5-minute
   `spendSync` against LiteLLM `/key/info` for per-account totals, and
   keep the session-close push as the per-session truth.
3. **Backfill**: replay session beads into vmagent's
   `/api/v1/import` to seed the last N days. LiteLLM `/spend/logs` covers
   gaps the bead store doesn't.

We do **not** need to touch the Anthropic billing API — every API call
already routes through LiteLLM, which carries `response_cost` per call.

## Current-state map

### Cost source-of-truth: LiteLLM

All Claude traffic is proxied by LiteLLM (`gardenio/helm/gardenio/templates/litellm/`).
Each gardenio **account bead** owns a virtual key (`fields.litellm_key`).
Pricing is computed inside LiteLLM and surfaced two ways:

- **Per call**: every JSONL `result`/`assistant` entry carries
  `response_cost` (or `_litellm_response_cost`) injected by LiteLLM. See
  `gardenio/controller/cmd/gb/session_usage.go:81-94, 144-147`.
- **Per key (cumulative)**: `GET {LITELLM_URL}/key/info?key=<vk>` returns
  `info.spend`. Daily/weekly periods come from `GET /spend/logs`. See
  `gardenio/controller/cmd/controller/spend_sync.go`.

There is **no in-house pricing table**, and we do not need one — the
controller never multiplies tokens × $/token. LiteLLM owns the pricing
churn (it ships an upstream price file that drifts as Anthropic updates
SKUs). This is the right boundary: gardenio treats LiteLLM as the meter.

### Per-agent attribution (today)

The chain that ties an LLM call back to an agent identity:

```text
coop session (in pod)
  └─ env: BOAT_AGENT, BOAT_AGENT_BEAD_ID, BOAT_PROJECT, BOAT_ACCOUNT
  └─ writes Claude session JSONL (response_cost per turn from LiteLLM)
       │
       ▼ at session close (gb agent start hook):
session bead (kind=session, type=session)
  └─ fields: agent, agent_bead_id, project, role, account*, total_cost_usd,
             input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
             request_count, tool_call_count, error_count, compaction_count
       │
       ▼ atomic IncrementBeadFields (no read-modify-write race):
agent bead (type=agent)
  └─ metadata.total_cost_usd, metadata.total_input_tokens, …, session_count
       │
       ▼ scraped every interval by controller metrics collector:
Prometheus: gardenio_agent_cost_usd{agent,project,role}
            gardenio_agent_input_tokens_total{...}
            …
```

References:
- `gardenio/controller/internal/beadsapi/client_session.go:25-145`
  (SessionUsage, RegisterSession, CloseSession, UpdateAgentUsage,
  UpdateAccountUsage, ReconcileSession).
- `gardenio/controller/cmd/gb/session.go:280-327, 363-460` (closeSessionBead).
- `gardenio/controller/cmd/gb/session_usage.go` (parseSessionUsage —
  reads JSONL, sums `response_cost`).
- `gardenio/controller/internal/metrics/metrics.go:73-260, 294-339`
  (Prometheus collector, `agentLabels = []string{"agent","project","role"}`).

`*` Account on the session bead is *not* a stored field today — the
account bead update is keyed by project via
`FindAccountBeadIDForProject`. This is fine because pod spawn already
guarantees one project → one account at a time (alphabetical-first
match). For the metric we still want an `account` label, which we read
from `BOAT_ACCOUNT` env var at session-close push.

### Live (in-pod) telemetry — coop

Every agent pod runs **coop**. coop already exposes:

- **`GET /metrics`** (Prometheus text) on the pod's coop endpoint,
  scraped by vmagent via the `prometheus.io/scrape=true` annotation.
  Metrics: `gardenio_session_input_tokens`, `_output_tokens`,
  `_cache_read_tokens`, `_cache_write_tokens`, `_request_count`,
  `_api_ms_total`. Labels: `agent_name`, `project`, `account`.
  Source: `coop/crates/cli/src/transport/http/metrics.rs`.
- **NATS publish**: when `COOP_NATS_FORWARD=1`, every JSONL turn fans
  out to NATS subjects `{prefix}.messages.{actor}.{kind}` where actor =
  `BOAT_AGENT`. Today three kinds are emitted: `assistant_text`,
  `thinking`, `usage`. The `usage` payload carries `input_tokens`,
  `output_tokens`, `cache_read`, `cache_write`, `duration_ms` + the
  metadata block (project, role, k8s pod info). Source:
  `coop/crates/cli/src/transport/nats_forward.rs:90-180, 229-251`.

**Critical gap for spend**: neither the `/metrics` endpoint nor the
NATS `usage` event carries `model` or `response_cost`. coop has the
data — it parses the JSONL line — it just doesn't forward it.

### Push path — controller already uses it

`gardenio/controller/cmd/gb/session_metrics_push.go` POSTs to
`{VMAGENT_URL}/api/v1/import/prometheus` at session-close and emits:

```
gardenio_session_cost_usd{agent_name,project,account,session_id} <usd>
gardenio_session_tokens_total{agent_name,project,account,session_id,type=...} <n>
gardenio_session_duration_seconds{...}
gardenio_session_request_count{...}
gardenio_session_tool_calls_total{...}
gardenio_session_errors_total{...}
gardenio_session_compactions_total{...}
```

This is the **proven path** for getting per-session cost into VictoriaMetrics
without waiting for a scrape interval. The cost dashboard in
`gardenio/helm/gardenio/dashboards/cost-overview.json` already has panels
that consume these series.

### What's already in the cost-overview dashboard

The dashboard ships expressions like:

- `sum(gardenio_agent_cost_usd)` — total spend
- `sum by (project) (gardenio_agent_cost_usd)` — cost by project
- `gardenio_account_cost_usd` — cost by account
- `sum by (project) (rate(gardenio_session_cost_usd[1h]) * 3600)` — cost rate

The last query is **buggy as-built**: `gardenio_session_cost_usd` is a
one-shot sample at session-close, not a counter that increases over a
session lifetime. `rate()` of one-shot samples is mostly noise. The
hybrid recommendation below replaces that with a real counter so the
panel becomes meaningful.

### Granularity available today

| Source | Granularity | Latency to VM | Per-agent? | Per-model? |
|---|---|---|---|---|
| coop `/metrics` (scrape) | `scrape_interval` (30s) | 30s | yes (agent_name) | **no** |
| coop NATS `usage` | per-turn | unconsumed today | yes (actor) | **no** (would be easy) |
| LiteLLM `response_cost` in JSONL | per-call | session-close only | yes (via session bead) | **no** (model in JSONL but not forwarded) |
| `gardenio_session_cost_usd` push | per-session | seconds at close | yes | no |
| `gardenio_agent_cost_usd` (controller scrape) | bead refresh interval | minutes | yes | no |
| LiteLLM `/key/info` | cumulative | 5min via spendSync | **no** (per-account) | no |
| LiteLLM `/spend/logs` | per-day per-key | 5min via spendSync | **no** (per-account) | per-model in payload, not stored |

**Per-hour fidelity is achievable today** without new infrastructure:
session-close pushes give sub-hourly samples for every agent that closes
a session in that hour. Long-running agents (>1h sessions) and live
visibility are the gap, both fixed by the live counter below.

## Recommended v1 emission strategy: hybrid (Option C)

### Live counter — `gardenio_agent_spend_usd_total`

```
gardenio_agent_spend_usd_total{agent,project,role,model,account} (counter, USD)
```

Implementation outline (R2 will name the metric and own the catalog;
this section describes the data flow):

1. **coop change** (`coop/crates/cli/src/transport/nats_forward.rs`):
   extend `extract_usage` to also parse `model` (top-level or nested
   `message.model`) and `response_cost` (or `_litellm_response_cost`).
   Add to the published JSON; bump the schema version field if/when one
   exists. Cost: ~30 lines + tests.
2. **coop change** (`coop/crates/cli/src/transport/http/metrics.rs`):
   add per-`model` token/cost gauges so the in-pod `/metrics` scrape
   covers the live case independently of NATS. Cost: ~50 lines.
3. **Controller forwarder** (new, `gardenio/controller/internal/spendforwarder/`):
   subscribe to `messages.*.usage`, increment a Prometheus counter, and
   either (a) expose it on the controller's existing `/metrics` for
   vmagent to scrape, or (b) push to vmagent with `/api/v1/import` on a
   tick. Recommend (a) — vmagent is already discovering the controller
   pod via the standard scrape annotation; one more counter is free.

The counter is **per-agent and per-model**. Per-hour spend rolls up at
query time:

```promql
sum by (agent) (increase(gardenio_agent_spend_usd_total[1h]))
sum by (project, model) (increase(gardenio_agent_spend_usd_total[1h]))
```

Cardinality budget: agents × models. Active agents typically <200,
models in production <8 → ≤1.6k series. Adding `account` brings it to
≤4.8k. Comfortably inside any reasonable VM budget; R2 owns the formal
budget.

### Reconciliation backstop

Keep `spend_sync.go` running every 5 minutes for the **account**-level
totals. This is the "did the live counter drift?" check. Add a periodic
reconciler that compares `sum by (account) (gardenio_agent_spend_usd_total)`
against `gardenio_account_cost_usd` and logs a warning when divergence
exceeds, e.g., 2% over an hour. No automatic correction — divergence is
a bug worth investigating, not a value to silently overwrite.

Do **not** try to back-fill the live counter from `/spend/logs` on a
tick. That double-counts.

### Existing session-close push: keep

`pushSessionMetrics` should keep emitting `gardenio_session_cost_usd`.
It's a backstop for sessions that crash before the live counter
flushes, and the existing dashboard panels already consume it. The buggy
`rate(gardenio_session_cost_usd[1h])` panel should be replaced with the
live counter expression above (R2 dashboard work).

## Proposed metric shape

```
# Primary live metric (new)
gardenio_agent_spend_usd_total{
    agent="<bead-name>",       # e.g. "thread-1777141509-460959"
    project="<project>",       # e.g. "gardenio"
    role="<role>",             # e.g. "crew", "gardener"
    model="<litellm-model-name>", # e.g. "claude-sonnet-4-6", "claude-opus-4-7"
    account="<account-name>",  # e.g. "ajl-pro-2"
}  COUNTER  USD
```

Companion token counters (mirror cost so attribution stays consistent):

```text
gardenio_agent_input_tokens_total{agent,project,role,model,account}        COUNTER
gardenio_agent_output_tokens_total{agent,project,role,model,account}       COUNTER
gardenio_agent_cache_read_tokens_total{agent,project,role,model,account}   COUNTER
gardenio_agent_cache_write_tokens_total{agent,project,role,model,account}  COUNTER
gardenio_agent_requests_total{agent,project,role,model,account}            COUNTER
```

**These supersede** the existing `gardenio_agent_*_total` *gauges*
(populated from agent bead fields) for live querying — the gauges
remain useful for "show me the lifetime total of this dead agent"
queries, but for time-series the counter is correct. R2 will decide
whether to keep both, prefix one (e.g. `gardenio_agent_lifetime_*` for
the bead-derived gauges), or just deprecate the gauge form.

### Labels NOT to add

- `task_id`, `bead_id`, `session_id` — high-cardinality. Use exemplars
  or log lines, not labels. (R2 owns the cardinality budget.)
- `slack_user_id` — privacy concern (see below) and not needed for
  spend-by-agent.
- `pod_name` / `instance` — duplicated by k8s scrape labels; vmagent
  adds them automatically.

## Backfill plan

**v1 (this work)**: replay closed `session` beads into vmagent's
`/api/v1/import/prometheus` with backdated timestamps. Each session
bead has the cost, timestamps, and agent/project/role/account it
needs. A one-shot CLI under `gb` (e.g. `gb session backfill-metrics
--since 30d`) is enough — no new infrastructure.

**Why not LiteLLM `/spend/logs` for backfill**: it gives per-day
per-key totals but no per-agent breakdown — the agent identity exists
only at the gardenio layer, not in LiteLLM's tags today.

**v2 (later)**: tag every LiteLLM call with `metadata.tags=["agent:<id>","project:<p>"]`
(LiteLLM supports this on `/chat/completions`) and use `/spend/tags`
for per-tag historic queries. This is the only way to recover spend
attribution for sessions older than the bead store. Filed as future
work; the live counter + bead-replay backfill is sufficient for v1.

## Privacy / labels

- `slack_user_id` lives on agent beads (e.g. `bot_agent_kill.go`,
  `bot_concierge.go`) — do **not** propagate it to metrics. It is the
  raw Slack user identifier and would tie individual employees to spend
  graphs. Use `agent` (bead name) or `project` instead. If a "spend by
  Slack user" view is ever needed, do the join at query/dashboard time
  against the agent bead, never as a metric label.
- `account` is fine to label — it identifies a credential, not a
  person. Multiple agents share an account.
- `model` is fine — public model names.
- `agent` (bead name) is fine — it's already exposed throughout the
  beads-viewer UI and Slack notifications.

## Risks / unknowns

1. **coop schema break**: adding `model` and `response_cost` to the
   NATS `usage` payload is additive but kbeads or any other consumer
   that asserts a closed schema needs a quick check. (No consumer
   exists today — verified by grepping `messages..*\.usage`. So this is
   safe but gets harder if a consumer lands first. Coordinate with R2.)
2. **response_cost can be missing**: not every JSONL entry carries it
   (e.g. local mock runs, claudeless tests, non-LiteLLM-routed calls).
   The counter must skip zero-cost turns silently — token counters
   stay accurate, USD just doesn't move. This is already how
   `accumulateUsage` behaves for session beads.
3. **Model-name normalization**: the JSONL `model` may be the raw
   Anthropic ID (`claude-opus-4-7`) or a LiteLLM alias
   (`claude-opus-4-7-oauth`). LiteLLM's `model_group_alias` is the
   canonical mapping (`gardenio/helm/gardenio/templates/litellm/configmap.yaml`).
   Normalize at emission time using `litellm_aliases.go`'s map so the
   metric label space stays small. Otherwise dashboards split a single
   model across 2-3 series.
4. **Counter resets across pod restarts**: the live counter is
   in-process to whichever service emits it (recommended: controller).
   If the controller restarts mid-hour, vmagent's `increase()` handles
   the reset correctly only if we use it correctly — the counter must
   be `prometheus.NewCounterVec` (monotonic) and never `.Set()`-ed.
5. **Dual-counted live + reconciliation**: the live counter and
   `gardenio_account_cost_usd` (account-level cumulative gauge from
   spendSync) can disagree by up to one scrape interval. This is
   expected; do not subtract one from the other in a panel.
6. **Pricing churn**: Anthropic releases new models/SKUs. LiteLLM
   typically ships pricing within a release; we inherit their cadence.
   No work for us beyond keeping LiteLLM bumped.
7. **High-cardinality footgun**: if a future change adds `task_id` or
   `session_id` to the live counter, series count explodes. R2's
   cardinality budget enforcement (e.g. a CI check or a vmagent
   relabel) is the right place to catch this.

## Open decisions for the synthesis epic (ka-0000000386)

- Where does the NATS→counter forwarder live: in `controller`, in
  `kbeads` (it already runs the bead daemon and is on NATS), or as a
  new tiny service? Recommendation: `controller` — it already has the
  Prometheus registry, the daemon client, and the metrics collector.
- Do we keep the bead-derived `gardenio_agent_cost_usd` gauge alongside
  the new counter, deprecate it, or rename it
  `gardenio_agent_lifetime_cost_usd`? R2 call.
- Backfill CLI shape: `gb session backfill-metrics` vs. one-shot script.
  Either works; pick whichever R2's instrumentation plan ends up using
  for other backfills.

## Appendix: file index for implementers

| Concern | File |
|---|---|
| LiteLLM virtual key + per-key spend | `gardenio/controller/cmd/controller/spend_sync.go` |
| Per-call cost extraction (JSONL) | `gardenio/controller/cmd/gb/session_usage.go` |
| Session bead lifecycle | `gardenio/controller/internal/beadsapi/client_session.go` |
| Session-close push to vmagent | `gardenio/controller/cmd/gb/session_metrics_push.go` |
| Atomic agent/account bead increments | `gardenio/controller/internal/beadsapi/client_session.go:121-145` |
| Controller Prometheus collector | `gardenio/controller/internal/metrics/metrics.go` |
| coop in-pod `/metrics` | `coop/crates/cli/src/transport/http/metrics.rs` |
| coop NATS forwarder | `coop/crates/cli/src/transport/nats_forward.rs` |
| coop usage state | `coop/crates/cli/src/usage.rs` |
| LiteLLM helm config | `gardenio/helm/gardenio/templates/litellm/configmap.yaml` |
| LiteLLM model aliases | `gardenio/controller/cmd/controller/litellm_aliases.go` |
| vmagent helm config | `gardenio/helm/gardenio/templates/vmagent/configmap.yaml` |
| Cost dashboard | `gardenio/helm/gardenio/dashboards/cost-overview.json` |
