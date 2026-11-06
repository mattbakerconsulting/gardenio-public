# Time-Series Metrics Catalog & Instrumentation Plan (R2)

Research deliverable for [ka-0000007788](../../../). Parent epic: ka-0000007704 (umbrella: gardenio time-series metrics + chart platform).

Companion research:
- R1 — VictoriaMetrics deployment & ops (ka-0000000179)
- R3 — per-agent LLM spend attribution (ka-0000001029)
- R4 — beads events → time-series (ka-0000002387)
- R5 — in-house chart backend (ka-0000004684)
- R6 — charts UX (ka-0000005001)

## TL;DR

1. We already emit ~30 metrics under the `gardenio_*` prefix from the **controller** (`:9092`) and a mixed `gardenio_*` / `beads_*` set from **kbeads** (`:9091`). vmagent scrapes via `prometheus.io/scrape` pod annotations. **No greenfield.**
2. The dominant pattern today is **poll-and-gauge**: the controller scans agent/account beads on an interval and `Set()`s a gauge from a bead field. This works for cumulative-on-bead values (e.g. `total_cost_usd`) but is the **wrong shape for event rates**. v1 needs to add **NATS-event-driven counters** alongside the existing pollers.
3. The current metrics inventory has three quality issues to fix before extending: (a) `gardenio_*` vs `beads_*` prefix split in kbeads — pick one, (b) gauges with `_total` suffix that should be counters (the bead poller can't emit a true counter; reconcile this), (c) several gauges have no `env` label so cross-environment dashboards alias.
4. **Coop (Rust) has no metrics today** — `tracing` only. v1 adds `metrics-exporter-prometheus` exposing `:9093/metrics`, scraped the same way.
5. Per-event counters live in **kbeads server** (the publisher), not a separate consumer. We get atomicity with the publish, no extra deployment surface, and N=1 producer for the canonical event count. The "separate exporter" path is reserved for derived metrics that only a stateful consumer can compute.
6. Hourly bucketing = **PromQL `increase(...[1h])`**, no recording rules. Only promote to a recording rule when a query is shown to be slow on real data and is on a hot dashboard path.

## 1. Existing instrumentation (the floor we're building on)

### 1.1 Controller — `:9092` `/metrics`

Source: `gardenio/controller/internal/metrics/metrics.go` (poller, 8s interval), `gardenio/controller/internal/reconciler/metrics.go` (deferred-agent histogram), `gardenio/controller/cmd/controller/main.go:231-282` (HTTP wiring), `gardenio/controller/cmd/gb/session_metrics_push.go` (per-session push for batch agents).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `gardenio_agents_active` | gauge | `project, role, state` | fleet count by state |
| `gardenio_agent_cost_usd` | gauge | `agent, project, role` | from agent bead `total_cost_usd` (LiteLLM sync) |
| `gardenio_agent_input_tokens_total` | gauge¹ | `agent, project, role` | poller-snapshotted bead field |
| `gardenio_agent_output_tokens_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_cache_read_tokens_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_cache_write_tokens_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_requests_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_sessions_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_tool_calls_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_errors_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_agent_compactions_total` | gauge¹ | `agent, project, role` | " |
| `gardenio_account_cost_usd` | gauge | `account` | per-credential-account |
| `gardenio_tag_cost_usd` | gauge | `dim, value` | real-bill per-dimension (ka-0000000478) |
| `gardenio_account_sessions_total` | gauge¹ | `account` | " |
| `gardenio_account_input_tokens_total` | gauge¹ | `account` | " |
| `gardenio_account_output_tokens_total` | gauge¹ | `account` | " |
| `gardenio_account_cache_read_tokens_total` | gauge¹ | `account` | " |
| `gardenio_account_cache_write_tokens_total` | gauge¹ | `account` | " |
| `gardenio_account_requests_total` | gauge¹ | `account` | " |
| `gardenio_account_utilization_5h` | gauge | `account` | rate-limit probe ratio |
| `gardenio_account_utilization_7d` | gauge | `account` | " |
| `gardenio_account_throttled` | gauge | `account` | 0/1 |
| `gardenio_account_warming` | gauge | `account` | 0/1 |
| `gardenio_metrics_last_scrape_timestamp_seconds` | gauge | — | |
| `gardenio_metrics_scrape_errors_total` | counter | — | |
| `gardenio_agents_deferred_total` | gauge | `project, reason` | reconciler |
| `gardenio_agent_deferred_wait_seconds` | histogram | `project, reason` | reconciler |

¹ **Naming bug to fix in v1.** Suffixed `_total` but is a `gauge`. Either flip type to counter (requires moving emission from poller to event handler) or rename. Prometheus best practice: `_total` ⇒ monotonic counter. Recommend renaming `agent_*_total` poller-gauges to `agent_*_current` and adding **separate** event-driven counters (see §2).

### 1.2 kbeads — `:9091` `/metrics`

Source: `kbeads/internal/server/http_metrics.go` (handler), `kbeads/cmd/kd/serve.go:1-100` (HTTP wiring).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `gardenio_fleet_total_cost_usd` | gauge | — | aggregate |
| `gardenio_fleet_active_agents` | gauge | — | |
| `gardenio_fleet_sessions_total` | gauge¹ | — | |
| `gardenio_fleet_beads_total` | gauge | `type, status` | |
| `beads_embed_duration_seconds` | histogram | (model, backend) | embedding pipeline |
| `beads_embed_errors_total` | counter | (backend) | |
| `beads_semantic_search_duration_seconds` | histogram | — | |
| `beads_semantic_search_results_total` | counter | — | |
| `beads_embedding_coverage_ratio` | gauge | — | |
| `beads_embedding_stale_count` | gauge | — | |
| `beads_tokens_revoked_total` | counter | — | |
| `beads_tokens_revoked_current` | gauge | — | |

**Prefix split**: `gardenio_fleet_*` (cluster-wide aggregates) vs `beads_*` (kbeads-internal subsystems). Inconsistent. v1 normalizes everything to `gardenio_*` (see §3).

### 1.3 coop / coopmux

No metrics emission. `tracing` + `tracing-subscriber` (JSON logs) only — see `coop/Cargo.toml`. No Prometheus or OTel client.

### 1.4 slack-bridge / jira-bridge / gitlab-bridge

No metrics. The bridges live under `gardenio/controller/internal/bridge/` and `gardenio/images/{slack,jira,gitlab}-bridge/`. No `/metrics` HTTP server.

### 1.5 Scrape topology

`gardenio/helm/gardenio/templates/vmagent/configmap.yaml:25` — vmagent discovers pods via `prometheus.io/scrape: "true"` annotation; `prometheus.io/port` and `prometheus.io/path` select endpoint. **No ServiceMonitor / PodMonitor CRDs.** Helm-rendered annotation is set on the controller deployment (`templates/controller/deployment.yaml:25-27`). Adding a new scrape target = annotate the pod template; no Prometheus-Operator CR needed.

Existing dashboards (Grafana ConfigMap):
- `gardenio/helm/gardenio/dashboards/cost-overview.json`
- `gardenio/helm/gardenio/dashboards/kbeads-embedding.json`

## 2. v1 metric inventory (additions)

Goal: instrument the operator-brief items (spend per agent per hour, beads events by type) and fill the obvious gaps that v1 needs to be useful: agent lifecycle as **events** (not just gauge snapshots), MR pipeline state, decision events, tool-call latency, NATS event taxonomy.

Listed by subsystem. **Bold** = new in v1. Plain = existing (kept for context). Unit suffixes follow Prometheus convention: `_total` (counter), `_seconds` (histogram), no suffix (gauge).

### 2.1 `gardenio_agent_*` — agent fleet & lifecycle

Source service: **kbeads** (event-driven) + **controller** (poller).

| Metric | Type | Labels | Source / emission point |
|---|---|---|---|
| **`gardenio_agent_lifecycle_events_total`** | counter | `project, role, event` | kbeads server, on agent bead state-transition event. `event` ∈ `spawned, started, stopped, oom_killed, stuck, drained, despawned`. |
| **`gardenio_agent_pod_phase_transitions_total`** | counter | `project, role, from, to` | controller `internal/statusreporter/`, on pod phase change |
| **`gardenio_agent_pods_pending`** | gauge | `project, role` | controller `internal/statusreporter/`, current-state snapshot of pods in K8s phase=Pending, re-derived each `SyncAll` sweep. Backs the GAP-1 `AgentPodsPendingTooLong` alert (`sum(...) > 0 for 10m`); the incident-direct spawn-stall signal a transition counter cannot express (ka-0000005418). |
| **`gardenio_agent_session_duration_seconds`** | histogram | `project, role, model` | controller, recorded on session-end event (`beads.session.end`). Buckets: `30s, 1m, 5m, 15m, 1h, 4h, 12h, 1d`. |
| **`gardenio_agent_involuntary_deaths_total`** | counter | `family, env, project, role` | controller `emitSessionCloseMetrics`, on session close when the agent bead's `close_reason` carries the `_involuntary` marker (agent died mid-work with a claimed task in flight) or the `context_cancelled_unclaimed` prefix (SIGTERM'd with tool calls on the bead but no claimed issue — C9 ka-0000001146). `family` ∈ `stop_gate_cooldown, stop_gate_escape_hatch, stop_gate_rate_limited, context_cancelled, context_cancelled_unclaimed, other`. This is the **"killed mid-work" signal** (ka-0000008189). It is DISTINCT from `agent_session_duration_seconds{outcome}`, which is derived from the SSE close event TYPE and mislabels a stop-gate involuntary close (which arrives as `AgentDone`) as `outcome="done"`. A clean idle-out, graceful stop, or genuine crash does NOT increment it. Prove-it-stopped query: `sum by (project) (increase(gardenio_agent_involuntary_deaths_total[1d]))` should trend to zero (the `context_cancelled_unclaimed` family is a coarse upper bound — warm-parked thread agents killed between turns land there — and is excluded from the `AgentInvoluntaryDeaths` alert). |
| **`gardenio_agent_tool_call_latency_seconds`** | histogram | `project, role, tool` | **coop** (per tool call), or session_metrics_push if coop instrumentation lands later. `tool` low-cardinality (Read, Edit, Bash, Grep, …). Buckets: exponential, `100ms…120s`. |
| **`gardenio_agent_llm_request_latency_seconds`** | histogram | `project, role, model, status` | coop, on LLM API completion. Buckets: `100ms…300s`. |
| **`gardenio_agent_compactions_total`** | **counter** (rename from gauge) | `project, role` | event-driven, on compaction event |
| `gardenio_agents_active` | gauge | `project, role, state` | controller poller (existing) |
| `gardenio_agent_cost_usd` | gauge | `agent, project, role, model` | controller poller — **add `model` label** (see §3.4) |

Note: per-`agent` labels on `*_lifecycle_events_total`, etc. are **omitted on purpose** — agent IDs are high-cardinality and short-lived (see §3.5). Agent identity is recoverable from logs/exemplars (R3) or kbeads queries.

### 2.2 `gardenio_beads_*` — kbeads event taxonomy

Source service: **kbeads** server. Emission point: NATS publisher (`kbeads/internal/server/*` where `events.NewNATSPublisher` is called).

The 22 NATS topics in `kbeads/internal/events/events.go:11-40` collapse to a small set of counters with an `event` label.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| **`gardenio_beads_events_total`** | counter | `event, type, fleet` | one counter for the full bead taxonomy. `event` ∈ `created, updated, closed, deleted, comment_added, label_added, label_removed, dependency_added, dependency_removed, transferred`. `type` = bead type (epic/task/bug/agent/decision/…) — bounded by config. `fleet` is currently single-valued (`gardenio`) but is in the event payload. |
| **`gardenio_beads_session_events_total`** | counter | `event` | session lifecycle: `end, pre_commit, pre_push, handoff` |
| **`gardenio_beads_advice_events_total`** | counter | `event` | `created, updated, deleted` |
| **`gardenio_beads_jack_events_total`** | counter | `event` | `on, off, extended, expired` |
| **`gardenio_beads_decision_events_total`** | counter | `event` | `created, resolved, expired`. Decisions are first-class for operator UX — separate counter even though they're a kind of bead event. |
| **`gardenio_beads_grpc_request_duration_seconds`** | histogram | `method, code` | gRPC server-side, via interceptor. Buckets: `1ms…10s`. |
| **`gardenio_beads_http_request_duration_seconds`** | histogram | `route, method, code` | HTTP middleware. Buckets: `1ms…10s`. |
| **`gardenio_beads_event_publish_errors_total`** | counter | `topic` | NATS publish failures |
| `gardenio_beads_total` | gauge | `type, status` | renamed from `gardenio_fleet_beads_total` |
| `gardenio_beads_embed_duration_seconds` | histogram | `model, backend` | renamed from `beads_embed_*` |
| `gardenio_beads_embed_errors_total` | counter | `backend` | renamed |
| `gardenio_beads_semantic_search_duration_seconds` | histogram | — | renamed |
| `gardenio_beads_embedding_coverage_ratio` | gauge | — | renamed |
| `gardenio_beads_embedding_stale_count` | gauge | — | renamed |
| `gardenio_beads_tokens_revoked_total` | counter | — | renamed |
| `gardenio_beads_tokens_revoked_current` | gauge | — | renamed |

The `fleet` label on `gardenio_beads_events_total` is included because event payloads carry it (see `BeadDeleted.Fleet`, `LabelAdded.Fleet` in `kbeads/internal/events/events.go`). It supports the future multi-fleet (gardenio-next, gardenio-rwx) split.

### 2.3 `gardenio_account_*` — credential accounts (existing, kept)

All metrics in §1.1 stay. Add:

| Metric | Type | Labels | Notes |
|---|---|---|---|
| **`gardenio_account_throttle_events_total`** | counter | `account, reason` | controller — increment on entry to throttled or warming state, complementing the gauge |
| **`gardenio_account_credential_rotations_total`** | counter | `account, kind` | controller, on rotation NATS notify |

### 2.4 `gardenio_bridge_*` — slack / jira / gitlab bridges

Source service: **bridges** (each binary opens a `:9094`/`9095`/`9096` metrics endpoint).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| **`gardenio_bridge_slack_messages_total`** | counter | `direction, kind, status` | `direction` ∈ `sent, received`; `kind` ∈ `decision, mail, squawk, thread_reply, …`; `status` ∈ `ok, error` |
| **`gardenio_bridge_slack_message_latency_seconds`** | histogram | `direction, kind` | Buckets: `100ms…30s` |
| **`gardenio_bridge_jira_webhooks_total`** | counter | `event, status` | |
| **`gardenio_bridge_gitlab_webhooks_total`** | counter | `event, status` | |
| **`gardenio_bridge_decision_resolutions_total`** | counter | `outcome` | `outcome` ∈ option ID class (clamped to bounded set) |
| **`gardenio_bridge_nats_subscriber_lag_seconds`** | gauge | `subject` | time since last received message; drives a "subscriber dead" alert |

### 2.5 `gardenio_mr_*` — merge request pipeline state

Source service: **gitlab-bridge** (it's already polling/listening). Pulls from GitLab API — does not require kbeads schema change.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| **`gardenio_mr_state_transitions_total`** | counter | `project, role, from, to` | `state` ∈ `opened, draft, ready, approved, merged, closed`. `project` = git repo project label (not bead project — disambiguate in label name later if needed). |
| **`gardenio_mr_pipeline_status_total`** | counter | `project, status` | `status` ∈ `success, failed, canceled, manual, skipped` |
| **`gardenio_mr_open_count`** | gauge | `project, role, author_class` | `author_class` ∈ `agent, human` (low-cardinality grouping, NOT the GitLab username) |
| **`gardenio_mr_time_to_merge_seconds`** | histogram | `project, role, author_class` | observed at merge time. Buckets: `5m, 30m, 2h, 12h, 1d, 3d, 7d` |

### 2.6 `gardenio_coop_*` — coop / coopmux (Rust)

Source: **coop** binary (in-pod multiplexer) and **coopmux** (cluster-side credential broker).

| Metric | Type | Labels | Notes |
|---|---|---|---|
| **`gardenio_coop_credential_assignments_total`** | counter | `account, status` | coopmux profile assignments |
| **`gardenio_coop_credential_pool_active`** | gauge | `account` | active sessions per pool entry |
| **`gardenio_coop_session_duration_seconds`** | histogram | `project, role` | coop, on session end |
| **`gardenio_coop_tool_call_latency_seconds`** | histogram | `project, role, tool` | (same as 2.1) |
| **`gardenio_coop_health_poll_drift_total`** | counter | `account` | when health-poller observes credential drift |

Implementation note: in Rust, use `metrics` crate + `metrics-exporter-prometheus`. Expose on `:9093` (coop) and `:9094` (coopmux). Annotate pods for vmagent scrape.

### 2.7 `gardenio_controller_*` — controller-internal

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `gardenio_agents_deferred_total` | gauge | `project, reason` | existing, **rename** to `gardenio_controller_agents_deferred` (it's not a counter) |
| `gardenio_agent_deferred_wait_seconds` | histogram | `project, reason` | existing, rename → `gardenio_controller_agent_deferred_wait_seconds` |
| **`gardenio_controller_reconcile_duration_seconds`** | histogram | `loop` | reconciler timing |
| **`gardenio_controller_pod_ops_total`** | counter | `op, status` | `op` ∈ `create, delete, drain` |
| **`gardenio_controller_subscriber_events_total`** | counter | `event, status` | what the controller's NATS subscriber actually processed |

## 3. Conventions

### 3.1 Naming

`gardenio_<subsystem>_<thing>_<unit>`. Subsystems for v1:

| Subsystem | Owns |
|---|---|
| `agent` | per-agent fleet metrics (lifecycle, sessions, tools, LLM requests) |
| `account` | per-credential-account aggregates and rate-limit state |
| `beads` | kbeads-server metrics (events, gRPC/HTTP, embeddings, jacks, advice) |
| `bridge` | slack/jira/gitlab bridges |
| `controller` | gardenio-controller internals (reconciler, podmgr, subscriber) |
| `coop` | coop + coopmux |
| `mr` | merge-request lifecycle (gitlab-bridge-emitted) |
| `fleet` | (deprecated) cluster-wide aggregates — fold into `agent`/`beads` |

Rules:
1. Counters end in `_total`. Histograms in `_seconds` / `_bytes` / `_ratio`. Gauges have no suffix.
2. Do not put `_total` on a gauge (existing bug; see §1.1).
3. Use `event`, not `type`, for the discriminator label on event counters when possible (avoid clashing with `type` = bead-type).
4. American spelling, lowercase snake_case, no hyphens, no plurals (`request_total` not `requests_total` — though Prometheus convention is fairly tolerant of plural; we keep existing `requests_total` for back-compat and use the singular for new metrics).

### 3.2 Mandatory labels (target labels)

Every metric carries these via the **scrape job**, not the application:

| Label | Source | Cardinality |
|---|---|---|
| `cluster` | vmagent external_labels | 1–3 |
| `env` | vmagent external_labels | 2–3 (`dev`, `e2e`, `prod`) |
| `service` | scrape config (Helm-templated) | bounded |
| `instance` | Prometheus default | pod-count |
| `pod` / `namespace` | Prometheus k8s SD | bounded |

**Action**: add `env` and `cluster` external_labels to vmagent if not already present (`gardenio/helm/gardenio/templates/vmagent/configmap.yaml`). Required so cross-environment dashboards alias correctly.

### 3.3 Per-metric labels — bounded set

These are allowed on individual metrics:

| Label | Cardinality budget | Rationale |
|---|---|---|
| `project` | ≤ 50 | gardenio, gardenio-next, monorepo, kbeads, beads3d, … |
| `role` | ≤ 50 | crew, gardener, thread, scheduler, jira-bridge, … |
| `model` | ≤ 20 | claude-opus-4-7, claude-sonnet-4-6, … |
| `account` | ≤ 30 | credential-pool size |
| `state` / `status` / `phase` | ≤ 10 | enum |
| `event` | ≤ 30 | enum (NATS topic tail) |
| `tool` | ≤ 20 | Bash, Read, Edit, Grep, Glob, WebFetch, … |
| `code` (gRPC/HTTP) | ≤ 20 | response code class |
| `method` (gRPC) | ≤ 100 | RPC name |
| `route` (HTTP) | ≤ 50 | parameterized template, NOT raw URL |
| `op` / `kind` / `direction` | ≤ 10 | enum |
| `from` / `to` (transitions) | ≤ 10 | enum |

### 3.4 Cardinality budget

Total active series per VictoriaMetrics instance, v1 target: **≤ 200k**.

Rough back-of-envelope: 50 metrics × (10 projects × 10 roles × 5 models) = 25k for the per-agent counters; another 25k for histograms (10× bucket inflation); another 50k for kbeads and bridge metrics. Comfortable headroom.

### 3.5 Forbidden labels (route to logs/exemplars instead)

| Label | Why forbidden | Alternative |
|---|---|---|
| `agent` | bound is unbounded over time (one row per agent ever spawned) | aggregate by `project, role`. For per-agent forensics, query kbeads. |
| `bead_id` / `task_id` | unbounded | log line, exemplar on histogram, or PromQL via VictoriaMetrics streaming aggregation |
| `user` / `slack_user` | PII + unbounded | hash bucket if needed, else log only |
| `mr_iid` / `pipeline_id` | unbounded | log only |
| `commit_sha` | unbounded | log only |
| `error_message` | unbounded freeform | bucket into `error_class` (≤ 10) |
| `url` (raw) | unbounded | `route` template |

**Exemplars** (Prometheus / OpenMetrics format) are how you keep the histogram aggregate while preserving a sample link to a specific event / bead / mr. VictoriaMetrics supports OpenMetrics exemplars on histograms.

Existing controller metrics violate this: `agent` label is on every per-agent gauge (`gardenio_agent_cost_usd`, etc.). **Mitigation**, not removal: agent beads are closed within hours-to-days, the poller drops series for closed agents, and Prometheus' staleness marker prunes them after `~5 minutes` of absent scrapes. `agent` label is **reluctantly grandfathered** for the gauges populated by polling open beads — it stops growing as beads close. Do **not** add `agent` to event counters (which would never let series die).

## 4. Ingestion path

### 4.1 Go services

**Recommend: Prometheus client (`prometheus/client_golang`) + scrape**, not push.

Why:
1. Already deployed in the controller and kbeads. Working.
2. vmagent SD is already discovering pods via annotation — adding a new pod is one-line annotation.
3. Push (vmagent's `remote_write` from `pushgateway`-style endpoints) is right for ephemeral/batch jobs. Long-lived agents/services ≠ batch.
4. `gb session_metrics_push.go` already covers the only push case we have today: per-session metrics from short-lived agent pods (correctly using push). Keep this pattern for **per-session** metrics, but emit normal scrape metrics for **per-pod** counters.

| Service | Endpoint | New scrape target? |
|---|---|---|
| controller | `:9092/metrics` | existing |
| kbeads | `:9091/metrics` | existing |
| slack-bridge | `:9094/metrics` | **new** |
| jira-bridge | `:9095/metrics` | **new** |
| gitlab-bridge | `:9096/metrics` | **new** |
| coop (in agent pod) | `:9093/metrics` | **new** — annotate the agent pod template |
| coopmux | `:9094/metrics` | **new** |

### 4.2 Coop (Rust)

**Recommend: `metrics` crate + `metrics-exporter-prometheus`**.

Why not OTLP:
1. We don't have a collector deployed. Adding `otelcol` is non-trivial (auth, scaling, retention) — punted scope.
2. VictoriaMetrics speaks Prometheus natively. OTLP would need a collector to translate.
3. `metrics` + `metrics-exporter-prometheus` is the smallest delta — single dep, exposes `/metrics`, scraped via the same vmagent annotation mechanism.

Tracing (logs) stays as it is. If we later need distributed traces, we revisit OTLP at that point as a separate decision.

### 4.3 Shared `metrics` package per Go module

Each Go module gets a `metrics/` package that:
1. Defines all the `prometheus.NewCounterVec` / `Histogram` etc. as package-level vars.
2. Exposes a `Register(reg prometheus.Registerer) error` function.
3. Lets handlers call `metrics.AgentLifecycleEvents.WithLabelValues(...).Inc()` directly — no global registry magic.

Pattern is **already in use** in `gardenio/controller/internal/metrics/metrics.go`. Replicate in:
- `kbeads/internal/metrics/` (consolidate `http_metrics.go` here)
- `gardenio/controller/internal/bridge/metrics/`
- `coop/crates/metrics/` (Rust analog using the `metrics` crate's described/registered macro)

## 5. Per-event metrics: where do counters live?

**Decision: kbeads server emits beads-event counters directly, in the same code path as the NATS publish.**

Rationale:
1. **Single source of truth.** Counter increment happens in the same function that publishes the NATS message. No risk of drift between "what got published" and "what got counted".
2. **Atomic.** Postgres event row + NATS publish + counter increment can sit in one helper. If publish fails, the counter doesn't increment.
3. **Zero new deployment surface.** kbeads already exposes `/metrics`.
4. **Cardinality is hand-tuned.** The metric author controls labels; a generic NATS-aggregator-exporter would either over-label (one label per topic) or under-label (no `type`/`event` discrimination).

**When the "separate consumer/exporter" pattern wins** (and we should reach for it later):
- **Derived metrics that require state**: e.g. "time-since-last-message-in-thread", "decisions-open-for-N-hours". A consumer can hold a window of events, kbeads cannot.
- **Cross-instance aggregation in HA**: when we run multiple kbeads replicas, in-process counters double-count if both replicas process the same event, or under-count if one is leader. (Today we run one replica; revisit when scaling.)
- **Re-emission for backfill**: replaying historical events into a fresh metric without re-deploying kbeads.

For v1 we run **one kbeads replica** (current state per Helm chart) and stay with in-server emission. Document the HA caveat as a known v2 issue.

## 6. Cost-tracking metric design (prep for R3)

R3 owns the implementation; here we lock the **shape** so dashboards and queries can be written against it.

```
gardenio_agent_spend_usd_total{project, role, model, account}
gardenio_agent_tokens_total{project, role, model, account, kind}   # kind ∈ input, output, cache_read, cache_write
```

- `agent` label **excluded** (cardinality, see §3.5). Per-agent attribution uses log/exemplar lookup: a histogram exemplar on `gardenio_agent_llm_request_latency_seconds` carries `{agent, bead_id}` and is dereferenceable through VictoriaMetrics' exemplar API.
- `account` label **included** because credential-pool exhaustion is a top-level operator concern.
- This is a **counter** (monotonic), populated by the kbeads server when a session-end event arrives carrying spend deltas. Today's `gardenio_agent_cost_usd` (gauge) is poller-snapshotted from the bead — keep both. The gauge is "current cumulative spend on this agent bead", the counter is "sum of incremental spend events" and supports rate queries.
- **Zero/near-zero sessions — intentional bead-vs-counter asymmetry (ka-0000006190, verified 2026-06-08).** A genuinely-$0 session (LiteLLM `close == baseline`, i.e. the agent made no billable requests) writes `total_cost_usd = 0` onto the bead but does **not** advance `gardenio_agent_spend_usd_total` — `RecordSpend` drops non-positive costs (`metrics/spend.go:65`) so the monotonic counter is never nudged by a no-op, and a clamped negative delta (`reconcile_session_spend.go:199-205`) is likewise dropped. The two views therefore differ only in **series presence**, never in **dollars**: a $0 session contributes 0 to both `sum(agent total_cost_usd)` and `sum(increase(gardenio_agent_spend_usd_total[…]))`. Live measurement on prod (1941 agent beads with `total_cost_usd`): 1527 read $0 — 1523 are never-reconciled defaults (no `litellm_spend_close` field; the reconciler never ran) and only 4 are genuine reconciled-$0 (`close == baseline`); the smallest *positive* reconciled cost is $0.18, so no sub-cent "near-zero" sessions occur in practice. **Verdict: no code change — the asymmetry is correct.** Counting a $0 session would create a spurious zero-valued counter series with no analytic value; dropping it keeps `increase()`/`rate()` honest. When auditing the two sources, compare **dollar sums**, not series counts: the gauge (and `sum` over bead fields) will always show more zero-valued/zero-series agents than the counter, and that is expected.
- Source of truth for the deltas: **R3 (LiteLLM sync / session-end events)**. R3 must answer: do sessions emit deltas inline at end-of-session, or is there a sync poller that emits synthetic deltas? Either is fine; the counter shape is the contract.

R3 follow-up (for that doc, not this one):
- Choose: emit on session-end (low latency, must aggregate) vs poll LiteLLM (lossless, ~1m latency).
- Decide whether `gardenio_agent_cost_usd` (gauge, agent-labeled, per-agent-bead) stays or is dropped in favor of the counter + exemplar lookup. Recommend **drop the gauge** once R3 lands the counter; the counter is the canonical answer to operator's brief.

### 6.1 Real-bill per-dimension gauge `gardenio_tag_cost_usd` (ka-0000000478)

The at-close counter `gardenio_agent_spend_usd_total` ticks **once per session at close**, so it lags ~1d and reads multiples LOW versus the live LiteLLM bill (ka-0000006213). `gardenio_account_cost_usd` (the per-account real-bill gauge, polled from `/key/info`) fixed that for the by-account view, but it carries no role/project/model labels — so by-role/by-project/by-model stayed on the low counter.

`gardenio_tag_cost_usd{dim, value, env}` closes that gap. It is the **real bill sliced by a single attribution dimension**, where `dim ∈ {role, project, model}`:

- **Producer:** the podspec tags every agent request with `role:<role>`, `project:<project>`, `model:<model>` (alongside the existing `agent:<name>`) via `x-litellm-tags` (`cmd/controller/podspec.go`).
- **Why the slices are each the full bill:** LiteLLM's `GET /spend/tags` books the **full per-request cost under EVERY tag on the request** — verified live: a 4-tagged request booked identical spend under all four tags, with no splitting. So `sum(role:*) == sum(project:*) == sum(model:*) == ` the real account bill.
- **Poller:** `cmd/controller/tag_spend_sync.go` reads `/spend/tags` from every distinct LiteLLM proxy (the fleet runs more than one, each with its own spend DB), **sums** each `(dim,value)` across proxies, and Sets the gauge every 5 min — the same cadence + provenance as the account gauge.
- **Query shape (identical to the account gauge):** `sum by(value)(increase((max by(dim, value, env)(gardenio_tag_cost_usd{dim="role"}))[<window>:5m]))`. It reconciles with the `/costs` headline fleet rate.
- **Cardinality:** role/project/model are each O(10) distinct values → O(35) series total. `agent:` is **excluded** (thousands of values, §3.5); by-agent spend stays on `gardenio_agent_spend_usd_total`, which also carries the per-row bead-linking the cost rollup uses.
- **Limitation — no cross-dimension intersection.** `/spend/tags` reports per-single-tag totals, so the gauge cannot answer "role=gardener **AND** project=monorepo". The `/costs` rollup therefore routes by-{role,project,model} to this gauge **only when no cross-dimension filter is active**; with a cross filter it falls back to the cross-filterable (but settled) `gardenio_agent_spend_usd_total` and captions the table accordingly (`cmd/beads-viewer/costs_rollup.go`, `resolveRollupSource`).
- **Model granularity caveat:** the `model` tag uses the account's default model (mirrors the counter's `model` label), so a role/project model **override** is attributed to the account default. Tightening to the override-effective model is a noted follow-up.
- **Cross-method validation + alert.** `gardenio_tag_cost_usd` and `gardenio_account_cost_usd` are the same LiteLLM bill sliced two ways, so each tag dimension's fleet total must reconcile with the account fleet total: `sum(role:*) == sum(project:*) == sum(model:*) == sum(account:*)`. The rollup dashboard's **accounting cross-check** panel overlays the two for an eyeball check, and `helm/gardenio/templates/tag-cost-validation-alert.yaml` (`tagCostValidation`, off by default until the gauge backfills) pages on `TagCostDriftFromAccountGauge` (sustained >5% fleet divergence) and `TagCostDimensionAbsent` (a whole role/project/model dimension stops reporting while the fleet spends).

### 6.2 Attribution split — `gardenio_agent_attributed_cost_usd` / `gardenio_unattributed_cost_usd` (ka-0000003988)

Approach B of the drift plan (ka-0000000041): per-agent settlement can never book requests that carry no `agent:` tag, so the bill is split explicitly and the residual becomes an alertable number instead of a vibe.

- **Producer:** the same `tag_spend_sync.go` tick. Per proxy it reads `/spend/tags` (the `agent:*` tag totals = the attributable bill) **and** `/global/spend` (the proxy's whole lifetime bill, tagged or not); a tick that fails either read carries the previous per-proxy snapshot forward whole, so the published split is always internally consistent.
- **Identity:** `attributed{env} + unattributed{env} ≡ Σ proxies' /global/spend` — settled + unattributed reconstructs the bill **by construction** every tick. The per-proxy residual is clamped at 0 so one proxy's indexing skew cannot mask another's genuine residual.
- **Labels:** `{env}` only — two series per environment, no cardinality cost.
- **Query shape:** absolute values are meaningless (they include all pre-tagging history); use `increase(gardenio_unattributed_cost_usd[1h])` etc.
- **Alert:** `UnattributedSpendHigh` (vmalert gap-alert pack, `vmalert.gapAlerts.unattributedSpend`, ON by default) fires when the unattributed share of the trailing-1h bill exceeds `ratioThreshold` (0.15 at arming; ratchet to ~0.05 once the rotation tag-loss bug ka-0000000333 is fixed). Runbook: `docs/alerting-runbook.md#unattributedspendhigh`.

## 7. Hourly bucketing — query-time vs recording rule

**Recommend: query-time PromQL.** Add recording rules only for queries proven slow on real data.

Operator ask: "spend per agent per hour". Three layers of answer:

1. **Per-account hourly spend** (the typical dashboard panel):
   ```promql
   sum by (account) (
     increase(gardenio_agent_spend_usd_total[1h])
   )
   ```

2. **Per-(project, role) hourly spend**:
   ```promql
   sum by (project, role) (
     increase(gardenio_agent_spend_usd_total[1h])
   )
   ```

3. **Per-agent hourly spend** (forensic, not dashboard): join `gardenio_agent_spend_usd_total` (no `agent` label) against kbeads bead-list at query time, OR look up via exemplar from the histogram.

Why not recording rules by default:
- Recording rules add a moving piece (vmalert / vmrecord) with its own evaluation cadence and failure modes.
- VictoriaMetrics' `increase()` over 1h is cheap on a counter with our cardinality (≤200k series).
- Recording rules are a one-way door: dashboards become coupled to a specific rule name, and renaming/restructuring is painful.

**Promote to a recording rule when**:
- A panel takes >2s to load on real data.
- The query is on a "first dashboard a human opens" path (cost overview).
- The same query is referenced by ≥3 panels or alerts.

## 8. Mapping: operator brief → concrete metrics

| Operator ask | v1 metrics | Query |
|---|---|---|
| Spend per agent per hour | `gardenio_agent_spend_usd_total{...}` | `sum by (project, role) (increase(gardenio_agent_spend_usd_total[1h]))` |
| Spend per account per hour | same | `sum by (account) (increase(gardenio_agent_spend_usd_total[1h]))` |
| Beads events by type | `gardenio_beads_events_total{event=...}` | `sum by (event) (rate(gardenio_beads_events_total[5m]))` |
| Beads created/closed today | `gardenio_beads_events_total{event="created\|closed"}` | `sum by (event) (increase(gardenio_beads_events_total{event=~"created\|closed"}[1d]))` |
| Decision events (operator UX) | `gardenio_beads_decision_events_total` | dashboard panel + alert on `created` rate vs `resolved` rate |
| Agent OOM rate | `gardenio_agent_lifecycle_events_total{event="oom_killed"}` | `sum by (project, role) (rate(...[1h]))` |
| Killed-mid-work agents (prove it stopped) | `gardenio_agent_involuntary_deaths_total{family=...}` | `sum by (family) (increase(gardenio_agent_involuntary_deaths_total[1d]))` — should trend to zero |
| MR throughput | `gardenio_mr_state_transitions_total{to="merged"}` | `sum by (project) (increase(...[1d]))` |
| Slack bridge healthy? | `gardenio_bridge_nats_subscriber_lag_seconds` | alert on `> 5m` |
| Tool-call p95 latency | `gardenio_agent_tool_call_latency_seconds` | `histogram_quantile(0.95, rate(...[5m]))` |
| Throttled accounts right now | `gardenio_account_throttled` (existing) | `gardenio_account_throttled == 1` |

## 9. Open decisions for the synthesis bead (ka-0000000386)

These are forks where this doc has a recommendation but the synthesis pass should formally adopt or reject:

1. **Rename `gardenio_agent_*_total` poller-gauges to `_current`?** Recommend **yes** (Prom convention violation). Cost: dashboard rewrites.
2. **Normalize `beads_*` → `gardenio_beads_*`?** Recommend **yes**. Cost: dashboard rewrites + one alert.
3. **Drop `gardenio_agent_cost_usd` (gauge) once `gardenio_agent_spend_usd_total` (counter) is live?** Recommend **yes**, after R3.
4. **HA kbeads + counter double-counting**: defer to v2 (single replica today).
5. **Coop metrics scrape-target = agent pod or coopmux only?** Both. Coop runs in the agent pod (annotate the pod template) and coopmux runs cluster-side (annotate the deployment). Two scrape targets.
6. **Bridge metrics ports**: assigned `9094`/`9095`/`9096`. Verify no conflicts with existing pod listeners before implementation.

## 10. References

Code (this monorepo):
- `gardenio/controller/internal/metrics/metrics.go` — controller poller, all 22 controller metrics
- `gardenio/controller/internal/reconciler/metrics.go` — deferred-agent histogram
- `gardenio/controller/cmd/controller/main.go:231-282` — health + metrics HTTP servers
- `gardenio/controller/cmd/gb/session_metrics_push.go` — per-session push (only push case today)
- `kbeads/internal/server/http_metrics.go` — kbeads collectors
- `kbeads/cmd/kd/serve.go:1-100` — kbeads server wiring (3 ports: gRPC 9090, HTTP 8080, metrics 9091)
- `kbeads/internal/events/events.go:11-40` — NATS topic taxonomy (canonical input to §2.2)
- `gardenio/helm/gardenio/templates/vmagent/configmap.yaml:25` — vmagent annotation-driven SD
- `gardenio/helm/gardenio/dashboards/{cost-overview,kbeads-embedding}.json` — current Grafana dashboards
- `coop/Cargo.toml` — confirms tracing-only state

Beads:
- ka-0000007788 — this task
- ka-0000007704 — umbrella epic
- ka-0000000179 — R1 (VictoriaMetrics ops)
- ka-0000001029 — R3 (cost attribution; consumes §6 contract)
- ka-0000002387 — R4 (beads events; consumes §2.2)
- ka-0000004684 — R5 (chart backend)
- ka-0000005001 — R6 (charts UX)
- ka-0000000386 — synthesis (consumes §9)
