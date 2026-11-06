# `query_prometheus` reach: what gardenio agents can already ask about themselves

> Track A, A5 rescope (ka-0000003130). Parent epic: ka-0000005390
> ("Track A — mcp-grafana → gardenio agents"). Synthesis: ka-0000006594.

This doc answers one question: **once an agent has the mcp-grafana
`query_prometheus` tool, what gardenio internals can it already reason over — and
through which dashboards and alerts is that same data surfaced to humans?**

The short version: **most of the "bidirectional dream" is already real with no
new exporter.** The kbeads daemon and the controller already publish a large
`gardenio_*` / `agent_*` / `account_*` metric surface; it is scraped into the
in-chart VictoriaMetrics (vmsingle, ~1y retention in prod); and `query_prometheus`
reads that same store. A5's only genuine net-new was a couple of point-in-time
*queue-depth* gauges (see [§5](#5-net-new-queue-depth-gauges)); everything else
in this doc already shipped.

## 1. How `query_prometheus` reaches the data

`query_prometheus` is one tool exposed by the standalone **mcp-grafana** server
(Track A A1, `gardenio/helm/gardenio/templates/mcp-grafana/deployment.yaml`). That
server:

- talks to the in-cluster gardenio **Grafana** API over a Bearer token
  (`gardenio.mcpGrafana.grafanaURL` / `gardenio.grafana.inClusterBaseURL`,
  `_helpers.tpl:335`), so no datasource credential ever enters an agent
  `.mcp.json`;
- runs **read-only** in the first slice — `--disable-write` + a Viewer token
  (decision ka-0000007279, `deployment.yaml:73`);
- resolves `query_prometheus` against the Grafana **Prometheus-type datasource**,
  which points at the in-chart VictoriaMetrics single node (`vmsingle`). Query
  surface is PromQL — instant and range queries.

So an agent's `query_prometheus` call runs PromQL against the very metrics
described below. Retention is whatever `vmsingle` is configured for in the target
environment (~1y in prod per the A5 synthesis); at ~30s scrape interval every
series below is queryable at fine resolution over that window.

### What feeds vmsingle

Two `VMServiceScrape` objects put the gardenio-internal metrics into vmsingle:

| Scrape | Emitter | Port / path | File |
|---|---|---|---|
| `gardenio-beads` | kbeads daemon | `metrics` → `/metrics` (9091) | `templates/beads/vmservicescrape.yaml` |
| `gardenio-controller` | controller | `metrics` → `/metrics` | `templates/controller/vmservicescrape.yaml` |

The beads `/metrics` endpoint is a **separate internal port** (`BEADS_METRICS_ADDR`,
default `:9091`) — not the public API ingress. Its DB-backed collectors refresh on
a background ticker and `Collect()` only reads an in-memory snapshot, so scrape
latency is independent of DB latency (ka-0000006261).

## 2. The gardenio-internals metric catalog (already scraped)

These are the families an agent can query today. Names are exact; labels are the
emitted label set.

### 2a. Bead / fleet aggregates — kbeads `fleetCollector`
`kbeads/internal/server/http_metrics.go`

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gardenio_beads_total` | gauge | `type`, `status` | Count of live beads by type and status — the whole bead graph, fleet-wide. |
| `gardenio_beads_total_cost_usd` | gauge | `project` | Accumulated agent cost USD per project. |
| `gardenio_beads_active_agents` | gauge | `project` | Agents `open`/`in_progress` per project. |
| `gardenio_beads_sessions_total` | counter | `project` | Agent sessions per project. |
| `gardenio_beads_open_queue_depth` | gauge | `project`, `type` | **Point-in-time count of OPEN operator-facing queue beads** (`decision`, `escalation`, `mail`) per project — the undrained backlog a human still has to act on. Adds the project dimension `gardenio_beads_total` lacks. |
| `gardenio_beads_open_queue_oldest_age_seconds` | gauge | `project`, `type` | **Age of the OLDEST open queue bead** per project — head-of-line staleness of that backlog. NET-NEW in this rescope ([§5](#5-net-new-queue-depth-gauges)). |
| `gardenio_beads_fleet_metrics_staleness_seconds` | gauge | — | Age of the fleet-metrics snapshot; climbs if the background refresher stalls (alert on it). |
| `gardenio_beads_fleet_lifetime_metrics_staleness_seconds` | gauge | — | Age of the agent lifetime rollup behind `_total_cost_usd` and `_sessions_total` (5 min refresh); climbs if that refresher stalls. |

`gardenio_beads_total{type="decision",status="open"}` already gives fleet-wide open
decision counts; the queue-depth gauges add the **per-project** and **staleness**
dimensions that count alone cannot express.

### 2b. Bead events, search, embeddings — kbeads
`kbeads/internal/server/*metrics*.go`, `metrics_beadevents.go`

| Metric | Type | Meaning |
|---|---|---|
| `gardenio_beads_events_total` | counter | Bead lifecycle events (create/claim/close/…) by event kind. |
| `gardenio_beads_created_by_operator_total` | counter | Beads authored by a human operator. |
| `gardenio_beads_authoring_human_gate_total` | counter | Human-gate authoring decisions. |
| `gardenio_beads_references_extracted_total`, `_extract_duration_seconds` | counter / histogram | Typed-mention reference extraction volume + latency. |
| `gardenio_beads_semantic_search_duration_seconds`, `_results_total` | histogram | Semantic-search latency + result counts. |
| `gardenio_beads_embed_duration_seconds`, `_embed_errors_total` | histogram / counter | Embedding backfill latency + errors. |
| `gardenio_beads_embedding_coverage_ratio`, `_embedding_stale_count`, `_embedding_stats_staleness_seconds` | gauge | Embedding coverage / staleness of the corpus. |

### 2c. Security / authz / attribution — kbeads

| Metric | Meaning |
|---|---|
| `gardenio_authz_denials_total`, `gardenio_authz_legacy_fallthrough_total` | Authorization denials + legacy ownership fall-through. |
| `gardenio_beads_writes_rejected_total`, `gardenio_beads_edge_auth_verify_total` | Rejected writes; edge-auth verifications. |
| `gardenio_beads_token_revocations_total`, `gardenio_beads_revoked_tokens`, `gardenio_beads_token_revocation_latency_seconds` | Token-revocation volume / current count / latency. |
| `gardenio_beads_db_auth_errors_total`, `gardenio_beads_query_canceled_errors_total` | DB-auth errors; canceled queries. |
| `gardenio_beads_operated_by_*` (writes_total, writes_by_source_total, header_arrival_total, fallback_total, ambiguous_agent_total, resolver_errors_total, legacy_sentinel_total) | `operated_by` attribution pipeline health. |
| `gardenio_service_field_crossfleet_write_total`, `gardenio_beads_operator_fleet_fallback_total`, `gardenio_beads_writes_with_agent_operator_total`, `gardenio_beads_xactor_override_total` | Cross-fleet / operator-attribution edge cases. |

### 2d. Per-agent + per-account cost & usage — controller
`gardenio/controller/internal/metrics/metrics.go`

Per-agent gauges (labels include agent identity/project): `agent_cost_usd`,
`agent_input_tokens_current`, `agent_output_tokens_current`,
`agent_cache_read_tokens_current`, `agent_cache_write_tokens_current`,
`agent_requests_current`, `agent_sessions_current`, `agent_tool_calls_current`,
`agent_errors_current`, `agent_compactions_current`.

Per-account aggregates + ratelimit probes: `account_cost_usd`,
`account_sessions_current`, `account_input_tokens_current`,
`account_output_tokens_current`, `account_cache_read_tokens_current`,
`account_cache_write_tokens_current`, `account_requests_current`,
`account_utilization_5h` / `_7d`, `account_throttled`, `account_warming`.

Also on the controller surface (histograms/counters used by the latency/lifecycle
dashboards): `gardenio_agent_session_duration_seconds`,
`gardenio_agent_llm_request_latency_seconds`, `gardenio_agent_tool_call_latency_seconds`,
`gardenio_agent_deferred_wait_seconds`, `gardenio_agent_lifecycle_events_total`,
`gardenio_agent_involuntary_deaths_total`, `gardenio_agent_spend_usd_total`,
`gardenio_agents_active`, plus warm-pool / FSM resource-mapping / advice-catalog
gauges (see the emitter comments in `metrics.go`).

### 2e. Operator-keyed metrics — the machine-checked contract
`gardenio/controller/internal/obspii/allowlist.go` (`OperatorKeyedMetrics`)

These carry operator attribution and are label-validated in CI (PII deny-list):

| Metric | Allowed labels |
|---|---|
| `gardenio_operator_activity_total` | `operator_id`, `project`, `activity_type`, `surface` |
| `gardenio_operator_session_duration_seconds` | `operator_id`, `project`, `model` |
| `gardenio_operator_cost_usd_total` | `operator_id`, `project`, `model` |
| `gardenio_decisions_lifecycle_total` | `event`, `urgency` |
| `gardenio_escalations_total` | `escalated_by_role`, `outcome` |
| `gardenio_user_request_latency_seconds` | `surface`, `endpoint`, `status_code` |

> Note: `gardenio_decisions_lifecycle_total` is a **lifecycle COUNTER** (events over
> time), *not* a point-in-time depth. That gap is exactly what
> `gardenio_beads_open_queue_depth` (a gauge) fills — see the A5 synthesis.

## 3. Dashboards that already consume these metrics
`gardenio/helm/gardenio/dashboards/*.json`

| Dashboard | Consumes |
|---|---|
| `cost-overview.json` | `gardenio_account_cost_usd`, `gardenio_agent_*_current`, `gardenio_agent_spend_logs_usd`, `account_throttled`/`_warming` |
| `dark-spend.json` | `gardenio_agent_spend_usd_total`, `gardenio_agents_active`, per-agent token gauges |
| `fleet-lifecycle.json` | `gardenio_agent_lifecycle_events_total`, `gardenio_agent_session_duration_seconds`, `gardenio_beads_events_total` |
| `agent-latency-distribution.json` | `gardenio_agent_*_latency_seconds_bucket`, `gardenio_agent_deferred_wait_seconds`, `gardenio_beads_semantic_search_duration_seconds` |
| `latency-cost-efficiency.json` | `gardenio_agent_spend_usd_total`, `gardenio_beads_events_total`, `gardenio_beads_created_by_operator_total`, account token gauges |
| `kbeads-embedding.json` | `gardenio_beads_embed_*`, `gardenio_beads_embedding_coverage_ratio`, `_stale_count` |
| `operated-by.json` | `gardenio_beads_operated_by_*`, `gardenio_operator_activity_total`, `gardenio_beads_authoring_human_gate_total` |

## 4. Alerts (vmalert) that fire on these metrics
`gardenio/helm/gardenio/templates/vmalert/configmap.yaml`

| Alert | Keys on |
|---|---|
| `CostMetricsAbsent` | `absent(gardenio_account_cost_usd) or absent(gardenio_agent_cost_usd)` |
| `AccountThrottled` | `gardenio_account_throttled` |
| `AgentInvoluntaryDeaths` | `gardenio_agent_involuntary_deaths_total` |
| `AgentProjectFailureRate` | `gardenio_agent_involuntary_deaths_total` / `gardenio_agent_lifecycle_events_total` |
| `DarkSpendOverThreshold` | `gardenio_agent_spend_usd_total` |
| `AdviceContextOverBudget` | `gardenio_prime_hook_bytes{source="advice"}` |
| `BeadsDaemonDown` / `BeadsDaemonAbsent` | `up{component="beads"}` (backed by the `/metrics` endpoint above) |
| `BeadsDBAuthErrors` | `gardenio_beads_db_auth_errors_total` |

(The vmalert configmap carries ~40 more rules across pods, bridges, VictoriaMetrics
and the trigger engine; the table above is the subset that keys on the
fleet/cost/bead metrics this doc catalogs.)

## 5. Net-new queue-depth gauges (this rescope)

A5's residual, per the synthesis (ka-0000006594), was **point-in-time queue-depth
gauges** — because decisions were only a lifecycle *counter*
(`gardenio_decisions_lifecycle_total`), never a depth *gauge*. Delivered without
duplicating anything already scraped:

1. **`gardenio_beads_open_queue_depth{project,type}`** — count of OPEN
   `decision`/`escalation`/`mail` beads per project (shipped in a sibling MR,
   ka-0000002235; commit `eb25290d4`). Complements `gardenio_beads_total{type,status}`
   by adding the project dimension.

2. **`gardenio_beads_open_queue_oldest_age_seconds{project,type}`** — age of the
   oldest open queue bead per project (this MR). Complements the *count* with
   *head-of-line staleness*: a bucket can be shallow yet stale when one item sits
   un-actioned. Absent (not `0`) for an empty queue, so it never reads as a
   misleadingly-fresh empty backlog.

Both aggregate entirely in Postgres (bounded by `#projects × #queue types` rows —
a handful), reuse the same `FleetQueueTypes` set and project-label resolution, and
sit on the same background-refreshed collector, so neither adds scrape-path DB load.

### Example agent queries

```promql
# Which project has the deepest un-actioned operator backlog right now?
topk(3, sum by (project) (gardenio_beads_open_queue_depth))

# Any decision/escalation/mail that has sat open for more than a day?
max by (project, type) (gardenio_beads_open_queue_oldest_age_seconds) > 86400

# My project's open decision backlog vs. how stale its oldest item is
gardenio_beads_open_queue_depth{project="gardenio", type="decision"}
gardenio_beads_open_queue_oldest_age_seconds{project="gardenio", type="decision"}

# Fleet-wide open bead distribution (no project dimension, but every type)
gardenio_beads_total{status="open"}
```
