# Research: Beads Events → Time-Series Metrics

**Bead:** ka-0000002387 (R4)
**Parent:** ka-0000007704 (umbrella: gardenio time-series + chart platform)
**Sibling research:** R1 vmops (ka-0000000179), R2 taxonomy (ka-0000007788), R3 spend (ka-0000001029), R5 chart backend (ka-0000004684)
**Status:** research only; no implementation until Phase 1 synthesis lands and operator approves via `gb decision`.

## 1. Goal

Define how beads lifecycle events (create, claim, comment, status transitions, decisions, jacks, agent lifecycle, mutations) become Prometheus-style time-series in VictoriaMetrics. This doc covers (a) what events exist today, (b) where to count them, (c) the v1 metric+label schema, (d) how to backfill from postgres, and (e) the open questions for the gardener to resolve before any code lands.

## 2. Current event surface (inventory)

The codebase already emits a *very rich* event stream. Two distinct event systems are in flight:

### 2.1 Bead-domain events (`kbeads/internal/events/events.go`)

Published by every store mutation (typically inside `store.RecordEvent()`) and persisted to the postgres `events` table. Subjects are flat strings of the form `beads.<entity>.<verb>`.

| NATS subject | Payload type | Notes |
| --- | --- | --- |
| `beads.bead.created` | `BeadCreated{Bead}` | Initial create. |
| `beads.bead.updated` | `BeadUpdated{Bead, Changes}` | Catch-all mutation; `Changes` is `map[string]any`. Status transitions, claims, releases, owner changes — all show up here today. |
| `beads.bead.closed` | `BeadClosed{Bead, ClosedBy}` | Distinct from `updated` (terminal). |
| `beads.bead.deleted` | `BeadDeleted{BeadID, Fleet}` | Rare. |
| `beads.bead.transferred` | `BeadTransferred{Bead, FromFleet, ToFleet}` | Cross-fleet move. |
| `beads.dependency.added` | `DependencyAdded{Dependency, Fleet}` | |
| `beads.dependency.removed` | `DependencyRemoved{BeadID, DependsOnID, Type, Fleet}` | |
| `beads.label.added` | `LabelAdded{BeadID, Label, Fleet}` | |
| `beads.label.removed` | `LabelRemoved{BeadID, Label, Fleet}` | |
| `beads.comment.added` | `CommentAdded{Comment, Fleet}` | |
| `beads.advice.{created,updated,deleted}` | `Advice*` | Advice CRUD. |
| `beads.jack.{on,off,extended,expired}` | `Jack*` | Jack lifecycle. |
| `beads.session.{end,pre_commit,pre_push,handoff}` | `SessionEvent{AgentID, Trigger, CWD}` | Hook-emitted agent session beats. |

Notable observations:

- **No version field** anywhere on the wrapper or payloads. That's an open question (§7).
- `claim` and `release` and `status:open→in_progress` are NOT first-class subjects today — they are squashed into `beads.bead.updated` with the change set in `Changes`. Counting "claims" as a metric will require parsing `Changes` (e.g., `assignee` going `""→x` for claim, `x→""` for release; `status` field for transitions). Either we (a) introduce richer subjects (`beads.bead.claimed`, `beads.bead.released`, `beads.bead.status_changed`) or (b) the metric increment site infers from `Changes`.

### 2.2 Eventbus (`kbeads/internal/eventbus/types.go`, `bus.go`)

A second event surface, dispatched via `eventbus.Bus.Dispatch()` to JetStream. Subjects are scoped: `<group>.<scope?>.<EventType>`. Used for hooks, decisions, agent lifecycle, mutations, gate, mail, jack, config, message-stream, etc. Roughly 40 distinct event types.

Key families relevant to v1 beads-event metrics:

| Subject pattern | Event types | Payload shape |
| --- | --- | --- |
| `agents.<EventType>` | `AgentStarted`, `AgentStopped`, `AgentCrashed`, `AgentIdle`, `AgentHeartbeat` | `AgentEventPayload{AgentID, AgentName, Role, SessionID, Reason, Uptime}` |
| `decisions.<requested_by>.<EventType>` | `DecisionCreated`, `DecisionResponded`, `DecisionEscalated`, `DecisionExpired` | `DecisionEventPayload{decision_id, question, urgency, requested_by, option_count, …}` |
| `mutations.<EventType>` | `MutationCreate`, `MutationUpdate`, `MutationDelete`, `MutationComment`, `MutationStatus` | `MutationEventPayload{type, issue_id, title, assignee, actor, old_status, new_status, labels, agent_state}` |
| `gate.<EventType>` | `GateSatisfied`, `GateCleared` | `GateEventPayload{gate_id, agent, mechanism, actor, session_id}` |
| `jack.<EventType>` | `JackOn`, `JackOff`, `JackExpired`, `JackExtend` | (overlaps with `beads.jack.*`; eventbus copy is the consumable one) |
| `mail.<EventType>` | `MailSent`, `MailRead` | |
| `hooks.<actor>.<EventType>` | `SessionStart`, `Stop`, `PreToolUse`, `PostToolUse`, `Notification`, `PreCompact`, … | |
| `config.<EventType>` | `ConfigSet`, `ConfigUnset` | |
| `coop.events.messages.<actor>.<EventType>` | `AssistantText`, `Thinking`, `Usage` | Coop-side message stream. |

The wrapper `eventbus.Event` has `Type`, `SessionID`, `Actor`, `PublishedAt`, `Raw json.RawMessage`. The raw JSON of the typed payload is preserved, so consumers don't need a hard import dependency on the payload struct.

**Decision-makers note:** there are *two* mutation/lifecycle surfaces (the bead-domain `beads.bead.updated` and the eventbus `mutations.MutationUpdate`). For v1 we should pick one as the source of truth for counting. `mutations.*` is the more structured option (`old_status`, `new_status`, `assignee`, `actor` are first-class fields); `beads.bead.*` is closer to the storage layer (cheaper to instrument, no risk of being in flight without a corresponding DB row).

### 2.3 Existing subscribers

Today only a small subset of subjects is consumed:

- Controller: `coop.events.messages.>`, `gardenio.rotation.>`, `*.*.account.exhausted`, `*.*.auth.budget_exceeded` (account-rotation probes). The controller does **not** subscribe to `hooks.>`.
- Slack-bridge: `hooks.>`, rotation events.
- **No active consumer** for `agents.*`, `decisions.*`, `mutations.*`, `gate.*`, `jack.*`, `mail.*`, `beads.>`. They're published but only read offline (postgres replay).

> **Corrected 2026-09-21.** An earlier revision of this section listed `hooks.>`
> under the controller. It is not there: the only non-test subscriber is
> `bridge.HookStream` (`gardenio/controller/internal/bridge/hookstream.go:229`),
> and it is constructed in exactly one place,
> `gardenio/controller/cmd/slack-bridge/main.go:591`. Since slack-bridge is
> disabled on the live cluster (`deploy-values/gardenio-k3s.yaml:574`),
> `hooks.>` has **no consumer in production at all** — agents publish a full
> activity stream into a `DiscardOld` ring that nothing reads. See
> `gardenio/plans/claude-session-subagent-tracking.md`.

### 2.4 Persistence

`kbeads/internal/store/postgres/migrations/001_initial.up.sql` (lines 64–75) defines:

```sql
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    topic      TEXT NOT NULL,
    bead_id    TEXT NOT NULL,
    actor      TEXT NOT NULL DEFAULT '',
    payload    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_events_bead_id ON events (bead_id);
CREATE INDEX idx_events_topic  ON events (topic);
```

Every `beads.*` event currently lands here. Eventbus events are JetStream-only — they are **not** persisted to this `events` table (they live in JS streams + on disk as long as JS retention holds). This matters for backfill (§5).

## 3. Existing Prometheus instrumentation

Prometheus is already wired up in this repo. R2 (taxonomy) is the canonical reference — what follows is the slice relevant to events.

- **kbeads** serves `/metrics` on `:9091` (separate listener, `internal/server/http_metrics.go`). Existing collectors: `fleetCollector` (queries beads for fleet-aggregate gauges), token revocation counters, embedding-pipeline histograms (`beads_embed_*`, `beads_semantic_search_*`).
- **gardenio-controller** serves `/metrics` on `:9092` (`internal/metrics/metrics.go`). Per-agent and per-account gauges populated by a 30s polling loop over beads.
- **slack-bridge** has the prometheus.io/scrape annotations and listens on `:9093`, but no explicit collectors are wired in code yet.
- **No OpenTelemetry / OTLP** anywhere.
- **Helm chart** (`gardenio/helm/gardenio/values.yaml` lines 960–1041) ships `vmsingle`, `vmagent`, and Grafana — all `enabled: false` today. `vmagent` is pre-wired to discover pods via `prometheus.io/scrape="true"` annotations on ports 9091/9092/9093. No `ServiceMonitor` / `PodMonitor` (we are not depending on Prometheus Operator).

This means **the wire is already there**. Adding an event-counter collector inside kbeads is a few lines of `promauto`; vmagent will scrape it on the existing endpoint. R1's recommendation (single-node vmsingle, vmagent push) is the obvious match. No new ports, no new ServiceMonitors.

## 4. Recommendation

### 4.1 Counting strategy: emit-at-source, inside kbeads

Recommend **option A** from the bead description: kbeads itself increments `gardenio_beads_events_total{...}` at event-emission time (i.e., right next to `store.RecordEvent()` and `eventbus.Bus.Dispatch()`).

Reasoning:

1. **No second consumer to maintain.** A separate aggregator-from-NATS would need its own deployment, JetStream durable consumer, postgres for dedup state, and operational ownership — all to count events that are already counted at the source.
2. **Idempotency for free.** `store.RecordEvent` runs inside the same transaction that mutates the bead; if the txn rolls back, the metric increment never happens (we just need to wire `prometheus.Counter.Inc()` *after* the txn commits, not inside it). NATS at-least-once delivery never enters the picture.
3. **Already where Prometheus lives.** kbeads already exposes `/metrics` on 9091; vmagent already scrapes it. We add counters to the existing registry.
4. **Cheap.** Counter increments are nanoseconds; no measurable hot-path impact even at peak event rate.
5. **Doesn't couple kbeads to a chart story.** kbeads exposes counters; what charts the operator builds with them is downstream.

The only real argument *against* (A) is "doesn't couple kbeads to the metric library." kbeads is already coupled (`prometheus/client_golang` is imported for the embedding histograms). That ship has sailed.

**Caveat:** for events not minted in kbeads (e.g., `coop.events.messages.*` from coop, hook events from agent shells) we have two sub-options:

- A1: count only the events kbeads *sees* — i.e., kbeads has a JetStream consumer of `agents.>`, `decisions.>`, `mutations.>`, `hooks.>` and increments a counter per delivery.
- A2: each emitter exports its own counters, scraped independently.

Recommend **A2 for hook/coop events, A1 for agent/decision/mutation events**:

- coop already runs a process per agent; making it expose its own `/metrics` is fine and keeps coop self-contained. Hook events are ephemeral and high-volume; coop is the natural counter site. (See R2 for the coop instrumentation plan.)
- Agent/decision/mutation/gate events are dispatched by kbeads's eventbus already, so kbeads can count them at dispatch site without a JetStream consumer at all. No new code path; one `Inc()` next to `bus.Dispatch()`.

### 4.2 v1 metric: `gardenio_beads_events_total`

```
# HELP gardenio_beads_events_total Counter of beads-domain lifecycle events.
# TYPE gardenio_beads_events_total counter
gardenio_beads_events_total{event="bead.created",      project, kind, role}
gardenio_beads_events_total{event="bead.claimed",      project, kind, role}
gardenio_beads_events_total{event="bead.released",     project, kind, role}
gardenio_beads_events_total{event="bead.status_changed",project, kind, role, from, to}
gardenio_beads_events_total{event="bead.closed",       project, kind, role}
gardenio_beads_events_total{event="comment.added",     project, kind, role}
gardenio_beads_events_total{event="dependency.added",  project, kind}
gardenio_beads_events_total{event="dependency.removed",project, kind}
gardenio_beads_events_total{event="label.added",       project, kind}
gardenio_beads_events_total{event="label.removed",     project, kind}
gardenio_beads_events_total{event="advice.created",    project}
gardenio_beads_events_total{event="advice.updated",    project}
gardenio_beads_events_total{event="advice.deleted",    project}
gardenio_beads_events_total{event="jack.on",           project, kind}
gardenio_beads_events_total{event="jack.off",          project, kind}
gardenio_beads_events_total{event="jack.extended",     project, kind}
gardenio_beads_events_total{event="jack.expired",      project, kind}
```

Decision/agent/gate/mail are a **separate** subsystem counter — these are platform events, not bead lifecycle events. Naming aligns to R2's `gardenio_<subsystem>_<thing>_<unit>` rule:

```
gardenio_decisions_events_total{event="created"|"responded"|"escalated"|"expired", project, urgency}
gardenio_agents_lifecycle_total {event="started"|"stopped"|"crashed"|"idle", project, role}
gardenio_agents_heartbeats_total{project, role}                 # split out — high volume
gardenio_gate_events_total      {event="satisfied"|"cleared", project, mechanism}
gardenio_mail_events_total      {event="sent"|"read"}
gardenio_mutations_events_total {event="create"|"update"|"delete"|"comment"|"status", project, kind}
```

Rationale:

- `event` carries the verb. The bead description suggested `type` as the label name; **R2 uses `event` for "what happened" because `type` collides with the bead `type` column**. (Open question §7, but I'm picking `event` here so the doc is concrete; will defer to R2.)
- `project` is mandatory. Pulled from the bead row's `project` field (already exists on most beads via labels in canonical form). If absent, label value is `""` (Prometheus convention) or `"unknown"`.
- `kind` is the bead-kind field (issue, epic, decision, agent, schedule, trigger, advice…) — already present on every bead. Cardinality is bounded (~10 values today, low-double-digits ceiling).
- `role` is the agent role for the actor that performed the event, when known. Empty string when the actor is a human or a system.
- **No** `bead_id`, **no** `agent_id`. R2 calls out cardinality budget; we honor it.
- For `bead.status_changed` only, we add `from` and `to` labels (each ~5 values: open, in_progress, completed, blocked, abandoned). 5×5=25 label combos × kinds × projects — bounded. The gardener should review before this lands; if it scares anyone, fold to a single `transition` label like `"open->in_progress"` (still bounded).

### 4.3 Latency histograms (deferred)

The bead description asks whether to include `time-to-claim`, `time-to-completion`, `decision-resolution-latency` histograms in v1.

**Recommend deferring to v2.** Rationale:

- Latency histograms require *correlating two events* (e.g., `bead.created` → `bead.claimed`). That means either (a) emit at the second event with the elapsed delta as the histogram observation, or (b) compute from the postgres `events` table on demand. Both are tractable but neither is a one-line counter.
- The operator brief is "spend per agent per hour" + "beads events by type" — counters cover that.
- Running counters first lets us see in operations whether we even *need* histograms (e.g., maybe a queue length gauge is enough for "how stale is the open-bead pool?"). Don't pre-build observability we haven't motivated.

Stub for v2:

```
gardenio_beads_time_to_claim_seconds_bucket   {project, kind}
gardenio_beads_time_to_completion_seconds_bucket{project, kind, role}
gardenio_decisions_resolution_seconds_bucket  {project, urgency}
```

Histogram observation site: emit at the `claimed` / `completed` / `responded` event, computing the delta from the bead row's `created_at` / `claimed_at` / `created_at` of the decision (whichever applies). Use the standard Prometheus histogram buckets initially; tune later.

### 4.4 Ingestion path

vmagent (deployed as part of the gardenio helm chart, currently `enabled: false`) discovers kbeads + controller + slack-bridge pods via the `prometheus.io/scrape="true"` annotation, scrapes `/metrics` every 15s, and remote-writes to vmsingle. **No change** to the ingestion path is needed for v1 — adding new counters to the existing `/metrics` registry just means more series at the same scrape endpoint.

Coop (Rust) needs a `/metrics` endpoint to be added — that's R2's lane and out of scope here.

### 4.5 Backfill

`events` table has every bead-domain event since the postgres install with `topic`, `created_at`, `payload`, `actor`, `bead_id`. To populate VictoriaMetrics historical data:

1. **Replay job** (one-shot Go binary or a `kd events backfill` subcommand): scan `events` ordered by `created_at`, project each row to the metric schema in §4.2, and push as VM samples via the `vminsert` Prometheus remote_write endpoint or via the `/api/v1/import/prometheus` import API.
2. For each event, the timestamp in VM is the `created_at` of the event row, **not** ingestion time — VictoriaMetrics accepts arbitrary backfill timestamps for samples on existing series.
3. Counter behavior: counters are monotonic. The backfill must increment counters in order, so the rate over the historical window matches reality. Easiest implementation: maintain in-memory per-label-set counter state in the backfill tool, push a sample per event with the running total.
4. The eventbus events (`agents.*`, `decisions.*`, `mutations.*`, `gate.*`, etc.) **are not in the postgres `events` table.** They live in JetStream only. Backfill horizon for those is bounded by JS retention (today: probably hours, not 90 days). This is a real gap. Two options:
   - (a) extend kbeads to persist eventbus events to a parallel `eventbus_events` table going forward — gives us forward backfill for the next horizon-window, but doesn't recover history.
   - (b) accept that decisions/agents/mutations history starts from cutover. For v1, counters become useful from Day 1 once vmagent is enabled; we don't need 90 days of pre-history for "decisions per hour" to work.

Recommend **(b) for v1 backfill, (a) as a follow-up.**

The backfill **does not** need to run before vmsingle goes live — it can be retroactive. Recommend turning on metric emission first, getting steady state, then running backfill to populate pre-history.

## 5. Duplicate / replay safety

Counting at emission means each event is counted exactly once at the point of dispatch. NATS at-least-once redelivery is not in the picture because we never count from a NATS subscription.

Two edge cases to call out:

- **Counter resets on kbeads restart.** Standard Prometheus counter behavior — vmagent + VictoriaMetrics handle this correctly via the `rate()` reset detection. No action needed.
- **Same event dispatched twice in code.** Defensive; caller bug; rare; identical to any other counter instrumentation. Code review is the safety net.
- **Backfill double-count.** If the replay job runs while live emission is also happening, we'd double-count the live window. Mitigation: backfill job has a strict `--upper-bound` flag and we explicitly stop at `now() - 1h`, leaving a gap (filled by live emission). Document in the backfill runbook.

## 6. Concrete v1 deliverable list

For the gardener's planning epic, the implementation work breaks down into:

1. Add an event-counter package `kbeads/internal/metrics/events.go` exposing typed `IncBeadEvent(...)`, `IncDecisionEvent(...)`, `IncAgentEvent(...)`, `IncMutationEvent(...)`, `IncGateEvent(...)`, `IncJackEvent(...)` helpers wrapping `promauto.NewCounterVec`. ~150 LOC.
2. Wire `IncBeadEvent` calls inside `store.RecordEvent` (or just after the txn commit point) — one line per topic. The topic→event-label mapping is direct.
3. Decompose `beads.bead.updated` into derived events for the metric: claim, release, status_changed, owner_changed (read `Changes` map and emit one metric increment per recognized transition). Keep the NATS subject as-is for back-compat.
4. Wire `IncDecisionEvent` / `IncAgentEvent` / `IncGateEvent` / `IncMutationEvent` calls inside `eventbus.Bus.Dispatch()` — one switch on event-type group.
5. Smoke test: spin up kbeads, do a few bead mutations, scrape `/metrics`, confirm counters move.
6. Helm: enable `vmsingle.enabled` + `vmagent.enabled` in gardenio-rwx values overlay (gated on operator decision per umbrella epic).
7. Backfill tool: `kd events backfill --remote-write http://vmsingle:8429/api/v1/write --upper-bound '-1h'`.
8. Recording rules / pre-canned PromQL: not in this doc — that's the chart-backend (R5) consumer.

Estimated effort: 1–2 day-equivalents for the kbeads side (steps 1–4), 0.5 day for backfill, 1 day for helm rollout + smoke + dashboards.

## 7. Open questions for gardener / R2

These are deliberately surfaced for the synthesis pass — not pre-decided here.

1. **Label name for the verb: `event` or `type`?** R2 should pick. I used `event` to avoid colliding with bead-`type`. R2's catalog will be the source of truth.
2. **Do we want fine-grained subjects (`beads.bead.claimed`) or stay with `beads.bead.updated{Changes}`?** Adding new subjects is non-breaking but is real surface. Recommend NOT adding new subjects — derive from `Changes` at the metric site. This keeps the NATS contract stable.
3. **Versioning the event payload.** Today none of the structs have a `Version` / `SchemaVersion` field. As payloads evolve, downstream consumers (chart backend, future metric exporters, audit replay) will break silently. Recommend a separate gardener task to add `SchemaVersion int` to the wrapper. **Not blocking v1 metrics** because v1 only reads `topic` + a handful of well-known fields.
4. **Eventbus persistence to postgres.** Currently lost on JetStream retention horizon. Decide whether to add a `eventbus_events` table in kbeads. Recommend adding in a follow-up after v1 metrics are live; gives us auditability + future replay.
5. **`status_changed` cardinality with `from`/`to` labels.** Fine today; could explode if statuses proliferate. Cap at 10 status values total across the platform; treat new-status proposals as a label-cardinality review.
6. **Event vs metric ownership boundary with R2.** R2 owns the catalog; this doc proposes a slice of it. If R2 disagrees on naming, R2 wins.
7. **Coop-side hook event counting.** Out of this research's scope; flagged for R2 since coop is the emitter.
8. **Backfill ordering for counters.** Need to validate empirically that VM accepts out-of-order timestamped samples on a counter series and computes `rate()` correctly. Worth a 30-minute spike before committing to the backfill design.
9. **Where do `beads.session.*` events count?** They're agent-shell hook beats, not bead lifecycle. Recommend folding under `gardenio_agents_session_total{event}` rather than `gardenio_beads_events_total`. Defer to R2.

## 8. References

- kbeads bead-event types: `kbeads/internal/events/events.go`
- eventbus types: `kbeads/internal/eventbus/types.go`, `bus.go`
- events table: `kbeads/internal/store/postgres/migrations/001_initial.up.sql`
- kbeads Prometheus metrics: `kbeads/internal/server/http_metrics.go`, `kbeads/internal/metrics/embedding.go`
- controller Prometheus metrics: `gardenio/controller/internal/metrics/metrics.go`
- Helm chart vmagent / vmsingle: `gardenio/helm/gardenio/values.yaml` lines 960–1041
- Sibling research: `gardenio/docs/research/timeseries-victoriametrics-ops.md` (R1, in flight), `gardenio/docs/research/timeseries-metrics-catalog.md` (R2, in flight)
