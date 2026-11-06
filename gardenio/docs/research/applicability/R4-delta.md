# R4 Applicability Delta — beads-events counters vs current kbeads event path

**Bead:** ka-0000001015 · **Parent (E0):** ka-0000001798 · **Umbrella:** ka-0000007704
**Source research:** ka-0000002387 — `gardenio/docs/research/timeseries-beads-events-metrics.md` (R4, written 2026-04-25)
**Re-verified against:** `main` @ `755d74f69` (release 2026.149.12), 2026-05-29
**Verdict:** **STILL APPLICABLE.** The architecture the R4 doc described holds. The two emission chokepoints it pinned still exist and are still the correct increment sites. One material drift (migration-026 fail-closed audit constraint) tightens — not invalidates — the backfill story and is captured below as a hard caveat for the implementer.

---

## 1. Claims still holding (re-verified line by line)

### 1.1 Bead-domain event surface — `kbeads/internal/events/events.go`

All R4 §2.1 subjects are present and unchanged in shape. Re-verified topic constants:

| R4 claim | Status | Evidence |
| --- | --- | --- |
| `beads.bead.{created,updated,closed,deleted,transferred}` | ✅ present | `events.go:11-14,36` |
| `beads.dependency.{added,removed}` | ✅ present | `events.go:15-16` |
| `beads.label.{added,removed}` | ✅ present | `events.go:17-18` |
| `beads.comment.added` | ✅ present | `events.go:19` |
| `beads.advice.{created,updated,deleted}` | ✅ present | `events.go:25-27` |
| `beads.jack.{on,off,extended,expired}` | ✅ present | `events.go:46-49` |
| `beads.session.{end,pre_commit,pre_push,handoff}` | ✅ present | `events.go:30-33` |
| `BeadUpdated.Changes` is `map[string]any` (status/claim squashed into `updated`) | ✅ holds | `events.go:58-61` |
| No `Version`/`SchemaVersion` field on wrapper or payloads | ✅ still true | `model/event.go:9-20` — only `ID, Topic, BeadID, Actor, Payload, CreatedAt, OperatedBy, OperatedByID` |

### 1.2 Eventbus surface — `kbeads/internal/eventbus/types.go`, `bus.go`

R4 §2.2's event-type families are intact and unchanged:

| Family | Event types | Evidence |
| --- | --- | --- |
| `agents.*` | `AgentStarted/Stopped/Crashed/Idle/Heartbeat` | `types.go:40-44` |
| `decisions.*` | `DecisionCreated/Responded/Escalated/Expired` | `types.go:34-37` |
| `mutations.*` | `MutationCreate/Update/Delete/Comment/Status` | `types.go:51-55` |
| `gate.*` | `GateSatisfied/Cleared` | `types.go:62-63` |
| `jack.*` | `JackOn/Off/Expired/Extend` | `types.go:66-69` |
| `mail.*` | `MailSent/Read` | `types.go:47-48` |

`MutationEventPayload` still carries `OldStatus`/`NewStatus`/`IssueType`/`Labels`/`AgentState` (`types.go:185-196`), so the status-transition labels R4 §4.2 proposed (`from`/`to`) remain derivable directly from the eventbus payload without re-reading the bead row. **This is actually a cleaner path than R4 assumed** — see §3.

### 1.3 Increment site — the post-commit chokepoint **still holds**

R4 §6 step 2 said "wire `IncBeadEvent` calls inside `store.RecordEvent` (or just after the txn commit point) — one line per topic." Re-verification confirms the **better** site is one layer up, and it is a single, durable chokepoint:

- **`BeadsServer.recordAndPublish(ctx, topic, beadID, actor, event)`** — `kbeads/internal/server/server.go:185`. Every bead-domain mutation funnels through here. It (a) marshals the payload, (b) calls `store.RecordEvent`, (c) `publisher.Publish`, (d) `broadcastEvent` (SSE), (e) republishes to the JetStream mutation stream. **This is the single correct increment site for `gardenio_beads_events_total`** — one `IncBeadEvent(topic, …)` call here covers every bead event with no per-call-site bookkeeping. `topicToMutationType` (server.go:233) already demonstrates the topic→family switch pattern the counter would reuse.
- **`eventbus.Bus.Dispatch(ctx, event)`** — `kbeads/internal/eventbus/bus.go:78`. Single chokepoint for all eventbus families (agents/decisions/mutations/gate/jack/mail). One switch-on-`event.Type` here covers R4 §6 step 4. Confirmed `Dispatch` is the only fan-out point; `publishToJetStream` (bus.go:115) is downstream of it.

Both chokepoints from R4 are real, single, and unchanged. **No structural drift in the emission path.**

### 1.4 Counter does not yet exist — v1 is greenfield

`grep -rn "gardenio_beads_events_total\|IncBeadEvent\|beads_events_total" kbeads/ gardenio/ --include=*.go` → **zero hits.** No partial implementation has landed. The current metrics registry (`kbeads/internal/server/http_metrics.go`) exposes only the `fleetCollector` gauges (`gardenio_fleet_total_cost_usd`, `gardenio_fleet_active_agents`, `gardenio_fleet_sessions_total`, `gardenio_fleet_beads_total`) — these are **store-query gauges**, not event counters, and do not overlap R4's proposed counter family. R4's deliverable is still net-new.

### 1.5 Backfill source — events table + cursor query **still holds**

R4 §4.5 assumed the `events` table (`topic, bead_id, actor, payload, created_at`) is the backfill source. Confirmed:

- Table schema unchanged at the core columns (`migrations/001_initial.up.sql:64-74`).
- The exact cursor-paginated replay query a backfill tool needs **already exists**: `queryGetEventsByTimeRange(since, until, cursor, limit)` (`queries.go:1313`) ordered `created_at ASC, id ASC`, exposed on the store interface as `GetEventsByTimeRange` (`store.go`) and over HTTP at `/v1/events` (`http_events_global.go`). A `kd events backfill` subcommand can wrap this verbatim — R4 §6 step 7 is cheaper than estimated.
- `EventHistogram` (store.go / `queryEventHistogram`) already buckets events by time — proof the replay-by-timestamp pattern is supported end to end.

### 1.6 Ingestion path unchanged

vmagent/vmsingle remain `enabled: false` in `gardenio/helm/gardenio/values.yaml` (vmsingle:2073, vmagent:2111). The R4 §4.4 claim — "no change to the ingestion path for v1; new counters just add series at the existing `/metrics` scrape endpoint" — still holds. The helm gating (R4 §6 step 6) is still a flip-the-flag operation pending the operator decision per the umbrella epic.

---

## 2. Drift since 2026-04-25

Four deltas. One is **material to the backfill design**; the rest are additive and non-breaking.

### 2.1 ⚠ MATERIAL — migration-026 fail-closed audit constraint breaks the "live count == events-table count" identity

This is the headline drift and the implementer must read it.

R4 §4.5 implicitly assumed every emitted event lands in the `events` table, so a backfill from that table reproduces the live counter stream. **That identity no longer holds.** Since R4 was written:

- **migration-026** (`026_operated_by_required_history.up.sql`, bead ka-0000005772) added `CHECK (operated_by_id IS NOT NULL) NOT VALID` on `events`. Existing rows are grandfathered; **new inserts must carry a resolved operator.**
- The server resolver went **fail-closed**: `recordAndPublish` (server.go:196-211) now calls `resolveAuditOperatorRequired`, and when the operator is unresolvable it **logs + skips `RecordEvent`** — but **still fires `Publish`, `broadcastEvent`, and the JetStream mutation republish.**

Consequence for the metric design:

1. If `IncBeadEvent` is placed **after** the resolver skip (i.e. gated on the same `ok`), the live counter will **undercount** exactly the unattributable events — and those same events are absent from the `events` table, so live + backfill stay consistent.
2. If `IncBeadEvent` is placed **before** the skip (unconditional, right at entry to `recordAndPublish`), the live counter counts events that **never reach the `events` table** → a backfill can never reconstruct them → permanent live-vs-backfill divergence on counter reset/replay.

**Recommendation (new, supersedes R4 §5):** place the bead-event counter increment **unconditionally at the top of `recordAndPublish`**, count the true emission volume, and treat the events-table backfill as **best-effort historical, not authoritative**. Document that backfill counts are a *lower bound* on pre-attribution history (events with unresolved operators are missing from the table). For prod this gap is tiny — attribution resolves for ~all real agent/human traffic — but the implementer must not assume table-count == counter-count and must not write a test asserting equality. The 313K-row prod events table predates the constraint, so historical backfill is unaffected by 026; only the go-forward skip matters.

### 2.2 Additive — new bead-domain event subjects (non-breaking)

Four topics were added to `events.go` since R4 inventoried it. They slot into the existing `gardenio_beads_events_total{event=…}` schema with no design change:

| New topic | Constant | Commit |
| --- | --- | --- |
| `beads.comment.deleted` | `TopicCommentDeleted` (events.go:20) | ka-0000002368 (`01bc5e22b`) |
| `beads.follower.added` | `TopicFollowerAdded` (events.go:21) | ka-0000004220 (`364577a58`) |
| `beads.follower.removed` | `TopicFollowerRemoved` (events.go:22) | ka-0000004220 |
| `beads.mail.sent` | `TopicMailSent` (events.go:43) | (mail topic on bead-domain side) |

Action: add `comment.deleted`, `follower.added`, `follower.removed` to the v1 event-label list (§4). They are low-volume, bounded-cardinality, and require no schema change.

### 2.3 Additive — `BeadClosed.DecisionAnswer` field

`BeadClosed` gained a `DecisionAnswer` field (events.go, ka-0000005470 `01b740e8d`) so `trigger:bead` watchers can match a chosen decision option. Irrelevant to counters (we count the close event, not its answer) but noted so the implementer isn't surprised by the extra payload field.

### 2.4 No drift — eventbus families, Dispatch signature, Event wrapper

`git log --since=2026-04-25 -- kbeads/internal/eventbus/types.go bus.go` shows **no changes to the event-type families or the `Event`/`Bus.Dispatch` shapes.** R4 §2.2 is verbatim-accurate today. The wrapper `eventbus.Event{Type, SessionID, Actor, PublishedAt, Raw}` is unchanged.

---

## 3. Shortest path now for the v1 deliverable

The path is **shorter** than R4 §6 estimated, because (a) the chokepoints are confirmed single-site, (b) the backfill query already exists, and (c) the eventbus mutation payload already carries `OldStatus`/`NewStatus` so status-transition labels need no bead-row re-read.

1. **`kbeads/internal/metrics/events.go`** (new, ~120 LOC): `promauto.NewCounterVec` for `gardenio_beads_events_total{event,project,kind,role}` plus the platform counters (`gardenio_decisions_events_total`, `gardenio_agents_lifecycle_total`, `gardenio_agents_heartbeats_total`, `gardenio_gate_events_total`, `gardenio_mail_events_total`, `gardenio_mutations_events_total`). Typed `IncBeadEvent/IncDecisionEvent/IncAgentEvent/IncGateEvent/IncMutationEvent/IncJackEvent` helpers. Register on the same default registry `http_metrics.go` already serves at `/metrics`.
2. **One call in `recordAndPublish`** (server.go:185), **unconditional, at function entry** (see §2.1). Map `topic`→`event` label via the same switch shape as `topicToMutationType`. Pull `project`/`kind` from the marshaled bead in the event payload; `role` from the actor when resolvable, else `""`.
3. **Derive sub-events from `BeadUpdated.Changes`** for claim/release/status_changed/owner_changed — read the `Changes` map at the increment site and emit one increment per recognized transition. (R4 §3 / OQ-2: do NOT add new NATS subjects; derive at the metric site. Still the right call — the NATS contract stays frozen.) For `status_changed`, take `from`/`to` from the `status` entry in `Changes`; cap status cardinality per R4 OQ-5.
4. **One switch in `Bus.Dispatch`** (bus.go:78): switch on `event.Type`'s family (`IsDecisionEvent`/`IsAgentEvent`/`IsMutationEvent`/`IsGateEvent`/`IsMailEvent`/jack) and call the matching `Inc*` helper. Status transitions for the `mutations.*` family read `OldStatus`/`NewStatus` straight off `MutationEventPayload` (§1.2) — no bead re-read.
5. **Smoke test** (R4 §6 step 5): boot kbeads, run a few mutations + a decision, scrape `/metrics`, assert the counters moved. **Do not** assert events-table-count == counter-count (§2.1).
6. **Backfill subcommand** `kd events backfill --remote-write … --upper-bound '-1h'`: wrap `GetEventsByTimeRange` (queries.go:1313), project each row to the §4 schema, maintain in-memory per-label running totals, push samples timestamped at `events.created_at` via VM's `/api/v1/import/prometheus`. Treat output as best-effort lower bound (§2.1). Eventbus families (agents/decisions/mutations/gate) are **not** in the events table → no backfill, counters useful from cutover (R4 §4.5 option (b), still recommended).
7. **Helm** (R4 §6 step 6): flip `vmsingle.enabled` + `vmagent.enabled` in the gardenio-rwx overlay — gated on the operator decision per the umbrella epic. Unchanged.

**Revised effort:** ~1 day for kbeads steps 1–5 (down from 1–2; the chokepoints are confirmed and the eventbus payload already carries transition fields), ~0.5 day backfill (query exists), ~1 day helm + smoke + dashboards. The §2.1 constraint adds *zero* effort if heeded up front and *significant rework* if discovered late via a failing equality test.

---

## 4. v1 event-type family list (pinned)

`gardenio_beads_events_total{event, project, kind, role}` — bead-domain (from `recordAndPublish`):

```
bead.created · bead.updated · bead.closed · bead.deleted · bead.transferred
bead.claimed · bead.released · bead.status_changed{from,to} · bead.owner_changed   # derived from Changes
dependency.added · dependency.removed
label.added · label.removed
comment.added · comment.deleted                                                     # comment.deleted NEW (§2.2)
follower.added · follower.removed                                                    # NEW (§2.2)
advice.created · advice.updated · advice.deleted
jack.on · jack.off · jack.extended · jack.expired
```

`beads.session.*` → fold under `gardenio_agents_session_total{event}` per R4 OQ-9 (deferred to R2's ownership — they're agent-shell beats, not bead lifecycle).

Platform counters (from `Bus.Dispatch`), separate subsystem per R2's `gardenio_<subsystem>_<thing>_total` rule:

```
gardenio_decisions_events_total {event=created|responded|escalated|expired, project, urgency}
gardenio_agents_lifecycle_total {event=started|stopped|crashed|idle, project, role}
gardenio_agents_heartbeats_total{project, role}                       # split out — high volume
gardenio_gate_events_total      {event=satisfied|cleared, project, mechanism}
gardenio_mail_events_total      {event=sent|read}
gardenio_mutations_events_total {event=create|update|delete|comment|status, project, kind}
```

**Open question still deferred to R2:** label name `event` vs `type` (R4 OQ-1/OQ-6). R4 picked `event` to avoid colliding with the bead `type` column; this delta keeps that choice but R2's catalog remains the source of truth.

---

## 5. Updated decision-point recommendation

**Recommendation: PROCEED to implementation, unchanged in scope from R4, with two binding amendments.**

1. **Counter placement is now a decision, not a detail.** Place `IncBeadEvent` **unconditionally at the top of `recordAndPublish`** (count true emission volume) and treat the events-table backfill as a best-effort *lower bound*, not an authoritative replay. This is forced by migration-026's fail-closed skip (§2.1) and was not visible when R4 was written. The implementer must not write any test asserting `events`-table-count == live-counter-count.
2. **Derive status transitions from the eventbus `MutationEventPayload`** (`OldStatus`/`NewStatus`), not from a bead re-read — a simplification the current code now affords (§1.2, §3 step 4).

Everything else in R4 — the two chokepoints, the counter schema, the no-new-NATS-subjects stance, the defer-histograms-to-v2 call, the backfill-after-cutover sequencing, the helm gating — **survives verification intact.** No re-research needed; the doc is implementation-ready against `main` @ `755d74f69`.

The only remaining gate is the operator's `gb decision` on enabling `vmsingle`/`vmagent` per the umbrella epic (ka-0000007704) — that is a platform-cost decision, not a code-readiness one, and is orthogonal to landing the counter package + backfill tool (which add series to an endpoint nobody scrapes until the flag flips, so they are safe to merge ahead of the helm decision).

---

## 6. Evidence index (file:line, verified 2026-05-29 @ 755d74f69)

- Bead-domain topics: `kbeads/internal/events/events.go:11-49`
- `BeadUpdated.Changes`: `kbeads/internal/events/events.go:58-61`
- Event model (no version field): `kbeads/internal/model/event.go:9-20`
- **Bead-event chokepoint:** `kbeads/internal/server/server.go:185` (`recordAndPublish`); resolver skip at `:196-211`; topic→family switch at `:233` (`topicToMutationType`)
- **Eventbus chokepoint:** `kbeads/internal/eventbus/bus.go:78` (`Dispatch`); JetStream fan-out `:115` (`publishToJetStream`)
- Eventbus families: `kbeads/internal/eventbus/types.go:34-69`; `MutationEventPayload` (`OldStatus`/`NewStatus`): `:185-196`
- events table: `kbeads/internal/store/postgres/migrations/001_initial.up.sql:64-74`
- **migration-026 fail-closed constraint:** `kbeads/internal/store/postgres/migrations/026_operated_by_required_history.up.sql`
- Backfill cursor query: `kbeads/internal/store/postgres/queries.go:1313` (`queryGetEventsByTimeRange`)
- Current metrics registry (no event counters yet): `kbeads/internal/server/http_metrics.go:44-67`
- Helm vm gating: `gardenio/helm/gardenio/values.yaml:2073` (vmsingle), `:2111` (vmagent)
