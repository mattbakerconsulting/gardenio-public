# Data-Source Inventory: Activity-Feed Signals

**Task:** ka-0000002035 · **Epic:** ka-0000003263 (Research: new beads-viewer activity-feed front page) · **Operator:** Matt Baker

> Deliverable for child task #2 of the activity-feed research epic: enumerate every
> realtime signal a front-page activity feed could surface, map each to its source,
> and note what is already exposed to the viewer vs what is missing.

## Framing: three layers

A front-page activity feed draws on three distinct layers, and the inventory is
organized around that distinction:

1. **The spine — raw kbeads eventbus topics.** Every attributable bead mutation flows
   through one chokepoint, `BeadsServer.recordAndPublish`
   (`kbeads/internal/server/server.go:198`), which (a) writes a durable row to the
   `events` table (`RecordEvent`, `server.go:229`), (b) publishes on the legacy
   in-process topic (e.g. `beads.bead.created`), (c) mirrors mutation-class topics onto
   NATS JetStream subjects (`mutations.*`), and (d) fans out to the in-process SSE hub.
   Non-bead families (decisions, agents, mail, gate, jack, config, hooks, coop session
   messages) are published directly onto their own JetStream streams via the `eventbus`
   package (`kbeads/internal/eventbus/streams.go`, `types.go`).

2. **The surfacing layer — viewer SSE endpoints.** The viewer never reads NATS for bead
   lifecycle; it consumes the kbeads SSE hub at `GET /v1/events/stream`
   (`kbeads/internal/server/http.go:174` → `handleEventStream`) through `beadsapi`
   (`gardenio/controller/internal/beadsapi/client_events.go:72`, topic glob
   `beads.bead.>`). It subscribes to NATS *directly* only for the thinking/telepathy
   feed (`coop.events.messages.*.thinking`,
   `gardenio/controller/cmd/beads-viewer/thinking_hub.go`). A raw JetStream SSE
   multiplexer also exists at `GET /v1/bus/events`
   (`kbeads/internal/server/http_bus_sse.go`) but the viewer does **not** yet consume it.

3. **The backfill layer — a durable query for first paint.** Live SSE only delivers
   events *after* connect (`nats.DeliverNew()`, `http_bus_sse.go`). First paint must
   come from the durable `events` table via `GET /v1/events`
   (`kbeads/internal/server/http_events_global.go` → `handleListGlobalEvents`,
   `http.go:109`), with `GET /v1/events/histogram` (`http.go:132`) for an activity
   heatmap/sparkline.

> **Auth note:** `/v1/events` and `/v1/events/histogram` are `RoleViewer`. The live SSE
> endpoints `/v1/events/stream` and `/v1/bus/events` are `RoleOperator`
> (`kbeads/internal/server/http.go:174-175`). A viewer-side feed multiplexer must hold
> an operator-scoped token to open the live streams — same as today's `/agents/events`.

## Master signal-source table

| Signal | Source system | Topic / Endpoint | Payload key fields | Already exposed to viewer? | File ref |
|---|---|---|---|---|---|
| Bead created | kbeads mutation chokepoint | `beads.bead.created` (legacy) + `mutations.MutationCreate` (NATS) | `bead{id,title,type,status,assignee,created_by}` | **YES** via hub `/v1/events/stream` + durable `/v1/events` | `events.go:11`; `server.go` (recordAndPublish); viewer `bead_activity.go:140` |
| Bead updated | kbeads | `beads.bead.updated` + `mutations.MutationUpdate` | `bead{...}`, `changes{field→newValue}` | **YES** via `/v1/events/stream` | `events.go:12`; `beads.go` (UpdateBead); viewer `bead_activity.go:142`, `updatedBody:295` |
| Bead closed | kbeads | `beads.bead.closed` + `mutations.MutationStatus` | `bead`, `closed_by`, `decision_answer?` | **YES** via `/v1/events/stream` | `events.go:13`; `sse.go` (BeadClosed); viewer `bead_activity.go:144` |
| Bead deleted | kbeads | `beads.bead.deleted` + `mutations.MutationDelete` | `bead_id`, `fleet` | **YES** via `/v1/events/stream` | `events.go:14`; viewer `bead_activity.go:146` |
| Bead transferred (fleet) | kbeads | `beads.bead.transferred` | transfer payload | **YES** via `/v1/events/stream` | `events.go:36` |
| Status transition: **claim** (`status→in_progress`) | kbeads (derived) | `beads.bead.updated`, inspect `changes.status` | `changes.status="in_progress"` (+`changes.assignee`) | **PARTIAL** — derivable from BeadUpdated; no dedicated topic | `beads_invariants.go:21` (invariant); viewer `bead_activity.go:295` (`updatedBody`) renders generically |
| Status transition: **reopen** (`status→open`) | kbeads (derived) | `beads.bead.updated`, inspect `changes.status` | `changes.status="open"`, `changes.closed_at=∅` | **PARTIAL** — derivable from BeadUpdated | `beads.go` (ClosedAt cleared on reopen) |
| Dependency added / removed | kbeads | `beads.dependency.added` / `.removed` | `dependency{bead_id,...}`, `fleet` | **PARTIAL** — on hub glob + durable table, **not** mirrored to NATS | `events.go:15-16` |
| Label added / removed | kbeads | `beads.label.added` / `.removed` | `bead_id`, `label` | **PARTIAL** — hub + durable only (no NATS mirror) | `events.go:17-18` |
| Comment added | kbeads | `beads.comment.added` + `mutations.MutationComment` | `comment{bead_id,author,body}`, `fleet` | **YES** via `/v1/events/stream` | `events.go:19`; viewer `bead_activity.go` |
| Comment deleted | kbeads | `beads.comment.deleted` | `bead_id`, comment ref | **PARTIAL** — hub + durable, no NATS mirror | `events.go:20` |
| Follower added / removed | kbeads | `beads.follower.added` / `.removed` | `bead_id`, follower | **MISSING** from feed (on hub glob but no viewer rendering) | `events.go:21-22` |
| Advice created / updated / deleted | kbeads | `beads.advice.*` (legacy) / `advice.*` (eventbus) | advice payload | **MISSING** — not on `beads.bead.>` glob; no viewer endpoint | `events.go:25-27`; `eventbus/types.go:30-32` |
| Session end / pre-commit / pre-push / handoff | kbeads (agent-emitted) | `beads.session.{end,pre_commit,pre_push,handoff}` | session payload | **MISSING** from feed (hooks-handler consumed, not surfaced) | `events.go:30-33` |
| Session lifecycle (hooks) | coop → kbeads eventbus | JetStream `HOOK_EVENTS` / `hooks.*` (`SessionStart`,`Stop`,`SessionEnd`,`PreToolUse`…) | `session_id`, `tool_name`, `actor`, `cwd` | **PARTIAL** — only via raw `/v1/bus/events`; viewer doesn't consume | `eventbus/types.go:15-26`; `streams.go` |
| Mail sent | kbeads (type=mail bead create) + eventbus | `beads.mail.sent` (legacy) + `mail.MailSent` (`MAIL_EVENTS`) | `from`, `to`, `subject`, `sent_at` | **PARTIAL** — bead-create rides hub; dedicated stream only on `/v1/bus/events` | `events.go:43`; `eventbus/types.go:47` |
| Mail read | kbeads eventbus | `mail.MailRead` (`MAIL_EVENTS`) | `message_id`, `from`, `to` | **PARTIAL** — only `/v1/bus/events` | `eventbus/types.go:48` |
| Jack on / off / expired / extended | kbeads eventbus | `beads.jack.*` (legacy) + `jack.*` (`JACK_EVENTS`) | `jack_id`, `target`, `agent`, `reason`, `ttl`, `expires_at` | **PARTIAL** — only `/v1/bus/events` | `events.go:46-49`; `eventbus/types.go:64-67` |
| Agent spawn / stop / crash / idle / heartbeat | controller → kbeads eventbus | `agents.*` (`AGENT_EVENTS`): `AgentStarted/Stopped/Crashed/Idle/Heartbeat` | `agent_id`, `agent_name`, `role`, `session_id`, `reason`, `uptime_sec` | **PARTIAL** — viewer reconstructs lifecycle from **type=agent bead** events on `beads.bead.>` (`/agents/events`), **not** from `AGENT_EVENTS` | `eventbus/types.go:39-44`; viewer `agents_events.go:19` |
| Squawk (agent broadcast) | kbeads (type=message bead) | `beads.bead.created` (a type=message bead) | bead title/body, source agent | **PARTIAL** — surfaced as bead-create + batched rail join; no dedicated topic | viewer `index_rail.go`, `bead_chat.go` |
| Decision create | kbeads eventbus | `decisions.<scope>.DecisionCreated` (`DECISION_EVENTS`) | `decision_id`, `question`, `urgency`, `requested_by`, `option_count` | **PARTIAL** — decision *bead* rides `beads.bead.created`; scoped decision stream only on `/v1/bus/events` | `eventbus/types.go:34` |
| Decision resolve / respond | kbeads eventbus | `decisions.<scope>.DecisionResponded` | `chosen_index`, `chosen_label`, `resolved_by`, `rationale` | **PARTIAL** — also visible via `BeadClosed.decision_answer` on hub; dedicated stream only on `/v1/bus/events` | `eventbus/types.go:35`; `sse.go` (`DecisionAnswer`) |
| Decision escalate / expire | kbeads eventbus | `decisions.*.DecisionEscalated` / `.DecisionExpired` | `escalated_by_role`, `outcome` / expiry | **PARTIAL** — only `/v1/bus/events` | `eventbus/types.go:36-37` |
| Gate satisfied / cleared | kbeads eventbus | `gate.*` (`GATE_EVENTS`) | `gate_id`, `agent`, `mechanism`, `actor` | **PARTIAL** — only `/v1/bus/events` | `eventbus/types.go:62-63` |
| Config set / unset | kbeads eventbus | `config.*` (`CONFIG_EVENTS`) | `key`, `value`, `actor` | **PARTIAL** — only `/v1/bus/events` | `eventbus/types.go:57-58` |
| MR open / merge | GitLab bridge → kbeads (type=merge-request bead) | `beads.bead.created` / `.updated` on a **type=merge-request** bead | `Fields{mr_state}` (`opened`/`merged`/`closed`), `mr_iid`, `mr_title`, `pipeline_status`; `produces` edge | **PARTIAL** — rides bead lifecycle hub; no MR-specific topic | viewer `agents.go:572,1083-1134` (`MRBeadByURL`, `wrapUpMRViewFromBead`) |
| Pipeline status | GitLab bridge (poll) → bead Fields | **NOT an event** — `bead.Fields["pipeline_status"]` / `mr_pipeline_status`, fetched via GitLab API on demand | pipeline status, id | **MISSING** as a stream — pulled on demand, never published | viewer `coverage.go` (`GetPipelineByPath`); `agents.go:233` |
| Assistant text / thinking / usage | coop NATS forwarder → kbeads `MESSAGE_EVENTS` | `coop.events.messages.<actor>.{assistant_text,thinking,usage}` | `agent`, `text`, `full_length`, `session_id` | **YES** (thinking) via viewer `/telepathy/events` (direct NATS sub); `assistant_text`/`usage` **MISSING** from feed | `eventbus/types.go:71-73`; viewer `thinking_hub.go`, `telepathy.go` |

## Status transitions — deriving claim / close / reopen

There is **no dedicated "claimed" topic.** A consumer reconstructs the agent-workflow
lifecycle from two distinct signals:

- **Close** is explicit and first-class: its own topic `beads.bead.closed`
  (`BeadClosed{bead, closed_by, decision_answer?}`) and its own NATS mirror
  `mutations.MutationStatus`. A consumer keys on the topic string alone.

- **Claim, reopen, reassign, priority-bump, etc.** all arrive on the single
  `beads.bead.updated` topic, carrying a `changes` map of `field → new value` built in
  `UpdateBead`. The consumer must inspect `changes`:
  - **Claim** = `changes["status"] == "in_progress"` (typically with
    `changes["assignee"]`). The invariant at `beads_invariants.go:21` guarantees a
    transition into `in_progress` always carries a non-empty assignee, so
    `changes.status=="in_progress"` is a reliable claim marker.
  - **Reopen** = `changes["status"] == "open"` on a previously-closed bead; the server
    simultaneously clears `closed_at`, so `closed_at→∅` corroborates.

The viewer already does this client-side derivation in `updatedBody`
(`bead_activity.go:295`), iterating `changes` and rendering `field:value` pairs — but
**generically**, not as semantic "claimed"/"reopened" events.

**Implication for the feed:** close is detectable by topic; claim/reopen/reassign
require parsing `BeadUpdated.Changes`. A naive feed listening only for distinct topics
shows "updated bead" for a claim and misses the single most important agent-workflow
signal. The feed must special-case `changes.status` transitions to label them
**claimed / reopened / done**.

## Backfill vs live-stream

Live SSE (`/v1/events/stream` and `/v1/bus/events`) both default to
**new-events-only** (`nats.DeliverNew()`). On first paint the feed has nothing to show
until the next mutation occurs.

The durable backstop is the **`events` table**, written for every attributable
mutation inside `recordAndPublish` (`server.go:229`, `store.RecordEvent`). Queryable
two ways:

- `GET /v1/events` (`http_events_global.go`, `handleListGlobalEvents`) — global,
  cursor-paged, chronological events feed (RoleViewer). Canonical first-paint source;
  returns `model.Event{id, topic, bead_id, actor, payload, created_at, operated_by}`.
- `GET /v1/events/histogram` (`http_events_histogram.go`) — time-bucketed counts for
  an activity heatmap/sparkline.

**Recommended first-paint flow:** load recent N rows from `GET /v1/events` (seed the
heatmap from `/v1/events/histogram`), render, then open the SSE stream with
`Last-Event-ID` so the hub replays anything missed between snapshot and connect
(`eventsSince`). `/v1/bus/events` separately accepts `?since=<RFC3339>` for JetStream
`StartTime` replay.

**Caveat (from `kbeads/cmd/kd/events_backfill.go`):** the `events` table is a
best-effort lower bound. Events whose operator cannot be resolved are *counted* live
but *skipped* from the durable table (migration-026 fail-closed, `server.go:217`,
"skipping RecordEvent: operator unresolvable"). And the non-bead eventbus families —
agents, decisions, gate, jack, config, mail-read, hooks, coop messages — **never land
in the `events` table**; they live only on NATS JetStream. A feed backfilled purely
from `/v1/events` is bead-centric and will under-count those families.

> Note: `kd events backfill` (`events_backfill.go`) is a **Prometheus metrics**
> backfiller, not an event-replay API — it walks `/v1/events` to emit counters. Don't
> mistake it for a feed backfill endpoint.

## Gaps & recommendations

1. **No unified feed endpoint.** Bead lifecycle (hub SSE), thinking (direct NATS), and
   the rest of the bus (`/v1/bus/events`) are three disjoint streams; the viewer
   consumes only the first two. **Recommend:** add a viewer-side `GET /feed/events` SSE
   handler that multiplexes the kbeads `/v1/bus/events` JetStream endpoint across
   `mutations,decisions,agents,mail,jack,gate,messages` and normalizes every payload to
   one feed-row schema. The bus multiplexer (`http_bus_sse.go`) already supports
   `stream=` selection, so this is a *consumer*, not a new producer.

2. **Agent spawn/stop is reconstructed, not consumed.** The viewer infers agent
   lifecycle from type=agent bead events (`agents_events.go:19`) and ignores the
   purpose-built `AGENT_EVENTS` stream (`AgentStarted/Stopped/Crashed/Idle/Heartbeat`,
   with `uptime_sec`, `reason`). **Recommend:** have `/feed/events` subscribe to
   `agents.*` for real spawn/stop/crash signals with reasons and uptime.

3. **Decisions are double-sourced and inconsistent.** The decision *bead* rides
   `beads.bead.*` (visible), but the rich scoped `decisions.*` stream (urgency,
   chosen_label, escalation outcome) is only on `/v1/bus/events`. **Recommend:** source
   decision create/resolve/escalate/expire from `decisions.>` for structured
   `DecisionEventPayload` instead of inferring from `BeadClosed.decision_answer`.

4. **MR merge has no event; pipeline has no stream at all.** MR state lives in a
   `type=merge-request` bead's `Fields` and only rides generic `beads.bead.updated`;
   pipeline status is pulled on demand (`coverage.go`, `GetPipelineByPath`) and never
   published. **Recommend:** in the GitLab bridge, emit a typed status event when MR
   state flips to `merged`, and publish a pipeline event when `pipeline_status`
   changes — otherwise the feed shows MRs opening but learns of merges/pipeline results
   only by polling.

5. **Squawk, advice, session-lifecycle, mail-read, gate, config, jack have no viewer
   rendering.** Squawk/advice are MISSING from the feed entirely (advice isn't even on
   the `beads.bead.>` glob); mail-read/gate/config/jack are PARTIAL (NATS-only).
   **Recommend:** add per-family row renderers in the feed consumer (glyph + summary)
   keyed off the normalized `{stream,type}` from `busSSEEvent`; `jack.*`/`gate.*`
   payloads already carry display-ready `agent`/`reason`/`target` fields.

6. **First-paint backfill omits non-bead families.** `GET /v1/events` covers only
   bead-domain topics that reach the durable table. **Recommend:** for first paint of
   non-bead families, use `/v1/bus/events?since=<now-15m>` (JetStream `StartTime`
   replay) — JetStream retains those streams while the SQL `events` table does not.

## Key file references for the bundle

- `kbeads/internal/events/events.go` — legacy in-process topic constants + payload structs
- `kbeads/internal/eventbus/types.go`, `streams.go` — JetStream subjects/streams + payloads (agents, decisions, mail, jack, gate, config, hooks, messages)
- `kbeads/internal/server/server.go:198` — `recordAndPublish` (the mutation chokepoint)
- `kbeads/internal/server/sse.go` — bead-lifecycle SSE hub (`/v1/events/stream`)
- `kbeads/internal/server/http_bus_sse.go` — JetStream SSE multiplexer (`/v1/bus/events`)
- `kbeads/internal/server/http_events_global.go` — durable backfill query (`/v1/events`)
- `kbeads/internal/server/http_events_histogram.go` — histogram (`/v1/events/histogram`)
- `kbeads/internal/server/http.go:109,132,174,175` — route table + auth roles
- `kbeads/cmd/kd/events_backfill.go` — durable-table caveats (metrics backfiller)
- `gardenio/controller/internal/beadsapi/client_events.go` — viewer's SSE client
- `gardenio/controller/cmd/beads-viewer/bead_activity.go` — existing change-derivation (`updatedBody`)
- `gardenio/controller/cmd/beads-viewer/agents_events.go`, `agent_events.go` — existing `/agents/events` consumers (`beads.bead.>`)
- `gardenio/controller/cmd/beads-viewer/thinking_hub.go`, `telepathy.go` — direct-NATS telepathy consumer
- `gardenio/controller/cmd/beads-viewer/agents.go`, `coverage.go` — MR/pipeline projection from bead Fields
