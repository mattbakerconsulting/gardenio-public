# Realtime delivery options for the beads-viewer activity feed

**Task:** ka-0000002760 · **Epic:** ka-0000003263 (Research: new beads-viewer activity-feed front page)
**Author:** gardenio-realtime-delivery-options-yk8 (crew) · **Operator:** Matt Baker
**Status:** research recommendation — no code shipped. New feed lands behind `/feed`, promoted to `/` only on Matt's go-ahead.

---

## TL;DR — recommendation

**Ship the feed as REST seed + SSE-live, reusing the existing daemon hub — but route the
live stream through a viewer-side fan-out hub modelled on the existing `ThinkingHub`, and
use the already-built `GET /v1/events` durable endpoint for the initial paint and
gap-fill.** This is a **hybrid of Option A (SSE) and Option B (polling)** — not NATS-direct
(Option C).

Concretely:

1. **Initial paint + gap-fill backfill → `GET /v1/events?since=&limit=&cursor=`** — a
   fleet-wide, durable, cursor-paginated, Postgres-backed events endpoint that **already
   exists and is route-registered + tested** (`kbeads/internal/server/http_events_global.go:30`).
   This is the single most important finding: the backfill primitive everyone assumed we'd
   have to build is already in production.
2. **Live deltas → SSE**, reusing the proven client (`agents_stream.js`: backoff + auth
   probe + swap-by-`data-bead-id`) and the daemon's in-memory hub (`/v1/events/stream`).
3. **Scale fix → a viewer-side fan-out hub.** Today each browser opens its own upstream
   daemon SSE connection (N viewers ⇒ N daemon connections, each re-filtering the whole
   `beads.bead.>` firehose). The viewer **already solves exactly this** for the telepathy
   feed with `ThinkingHub` (one upstream subscription → fan-out to N browsers + a backfill
   ring). Apply the same pattern to the bead-event feed.

**Do not** make the viewer a direct NATS/JetStream subscriber (Option C) for v1: the
JetStream mutation stream drops 3 of the signal classes a feed wants, has no project/type
scoping in its subjects, and adds a new hard failure domain to the feed path — all for a
durability win that the Postgres `/v1/events` endpoint already delivers.

Every claim below was adversarially verified against the code (verdicts in
[§7](#7-verification-ledger)).

---

## 1. What we're delivering against

The new front page is a **fleet-wide, cross-project living feed** — "what is the whole
fleet doing right now" — surfacing bead created/claimed/closed, status transitions,
comments, MR/decision/squawk activity. That's a different shape from the existing
**per-agent** lane (`/agents/{id}/events`), which filters to one agent's interest-set.

Four axes the epic asked us to compare: **reconnection**, **backfill of missed events**,
**per-viewer scoping**, and **cost/complexity**.

The grounding infra we build on:

| Layer | File | Role |
|---|---|---|
| Daemon in-memory hub | `kbeads/internal/server/sse.go` | `GET /v1/events/stream?topics=beads.bead.>`; 1000-event ring + `Last-Event-ID` replay; **wiped on restart**; drops slow clients |
| Daemon durable events | `kbeads/internal/server/http_events_global.go` | `GET /v1/events?since=&until=&limit=&cursor=`; Postgres-backed, cursor-paginated, fleet-wide |
| Daemon JetStream bus | `kbeads/internal/server/http_bus_sse.go` | `GET /v1/bus/events?stream=&since=&subject_contains=`; durable replay; **only mutation-class topics**, flat subjects |
| Viewer SSE client (Go) | `beadsapi/client_events.go` | `EventStream(ctx, topics)` — proxies daemon SSE; **discards `id:`** |
| Viewer agent-feed handlers | `agents_events.go`, `agent_events.go` | re-render daemon events as HTML rows; **emit no `id:`** |
| Viewer client (JS) | `templates/static/agents_stream.js` | EventSource + 1s→30s backoff + auth-expiry probe + swap-by-`data-bead-id` |
| Viewer fan-out precedent | `thinking_hub.go` + `telepathy.go` | **one** upstream NATS sub → fan-out to N browsers + JetStream backfill ring |

---

## 2. Option A — SSE on the existing in-memory hub

**Reuse the path the viewer already uses:** `daemon.EventStream("beads.bead.>")` → filter
in Go → write HTML rows → `agents_stream.js` swaps them in.

**Reconnection — partially broken today.** The browser `EventSource` will auto-resume with
a `Last-Event-ID` header *if* the stream emits `id:` lines. The daemon does
(`sse.go:287` writes `id:%d`, and `sse.go:252-263` honours `Last-Event-ID` via the ring).
**But the chain is severed twice on the viewer hop:**

- The viewer's beadsapi client only parses `event:` and `data:` lines and **silently drops
  `id:`** (`client_events.go:133-137`). ✅ verified.
- The viewer's own SSE writers emit only `event:`/`data:`, **no `id:`**
  (`agents_events.go:154`, `agent_events.go:170`). ✅ verified.

So today, a browser reconnecting to a viewer SSE feed **cannot** resume from a sequence id —
it just reopens and relies on a fresh page seed. Fixable (forward the daemon's `id:`
through the viewer), but it's net-new work, and even fixed it only buys ≤1000 events of
replay before the gap must be filled from Postgres anyway.

**Backfill.** Initial paint comes from a REST seed (the house pattern — see §6). Live gaps
≤1000 fleet-events are coverable by the ring *if* `Last-Event-ID` were wired; beyond that,
or across any daemon restart (ring is in-memory, wiped — `sse.go:35-45`, ✅ verified), you
must re-seed from the durable store. At a fleet peak of ~100 events/s the ring is ~10s of
coverage — far too small to be the real backfill tier.

**Per-viewer scoping.** Two-layer filter: daemon-side topic/fleet match (`sse.go:82-90`)
then viewer-side type/project/schedule match (`agents_events.go:107-156`). Cheap per event,
but applied **per connection** — see the scale problem below.

**Cost/complexity — lowest.** A `/feed` handler mirrors `handleIndex` + `handleAgentsEvents`
(~150–200 LoC), no new deps, no new query paths. The catch is the **scale model** (§5): on
its own, Option A opens one upstream daemon SSE connection per browser.

---

## 3. Option B — REST polling

Client GETs a feed endpoint every N seconds; server returns recent events from the durable
Postgres events table.

**The decisive finding: the fleet-wide endpoint already exists.**
`GET /v1/events?since=<RFC3339>&until=&limit=1..1000&cursor=<id>`
(`http_events_global.go:30`, route-registered at `http.go:109` behind `RoleViewer`,
covered by `http_events_global_test.go`). It returns all events across all beads in a time
window, `created_at ASC, id ASC`, with `has_more` + `next_cursor` cursor pagination, 24h
default window, default limit 100 / max 1000. Backed by a single indexed query
(`queries.go:1360` — `WHERE created_at BETWEEN $1 AND $2 ... (cursor) ORDER BY created_at, id`),
**no NATS dependency**. ✅ all verified. (There's also a `/v1/events/histogram` companion.)

This means polling needs **zero new daemon infrastructure** — only a thin beadsapi wrapper
and a `/feed` poll loop.

**Reconnection — trivially robust.** Stateless GETs. No backoff, no auth-probe, no
`Last-Event-ID` plumbing. A dropped poll just retries on the next tick. This is a real
operational advantage over SSE's connection-state machinery.

**Backfill — natural.** "Give me everything since my last cursor/timestamp" *is* the poll
query. Survives daemon restart (Postgres is durable). No ring-size ceiling.

**Per-viewer scoping — natural in SQL.** `since` + (proposed) `topics`/`bead_id`/project
filters resolve in the indexed query, not in per-connection memory.

**Cost — DB load scales with viewers.** K viewers polling every P seconds = **K/P
queries/s** on Postgres. K=10,P=5s ≈ 2 q/s; K=50,P=5s ≈ 10 q/s; K=100,P=3s ≈ 33 q/s.
Each query is a fast indexed range scan, and the cursor keeps payloads small, but it is
linear and never zero. ETag/`If-Modified-Since` caching keyed on `since+limit` would shave
the idle case (not implemented today). **Latency = poll interval**: 3–5s feels acceptable
for a "recent activity" pane but is visibly not "live" for the instant-transition feel the
epic describes.

---

## 4. Option C — NATS/JetStream fanout

Viewer subscribes to the durable JetStream bus instead of the in-memory hub.

**Durability is real:** `GET /v1/bus/events?since=<RFC3339>` replays via
`nats.StartTime` (`http_bus_sse.go:69-72`), file-backed, surviving restart, bounded by
`MaxMsgs 10000` / `MaxBytes 100MB` per stream (`streams.go:199-200`) — roughly 2k–20k
mutations at typical bead sizes. ✅ verified.

**But three gaps make it the wrong v1 choice:**

1. **Signal coverage gap.** Only **5** topics reach JetStream —
   created/updated/closed/deleted/comment — because `recordAndPublish` only `PublishRaw`s
   mutation-class topics (`server.go:261-268`, `topicToMutationType` `server.go:273-287`).
   **Dependency, label, and advice events never hit the bus.** ✅ verified. A feed that wants
   "X blocked Y", "labeled needs-intake", etc. can't get them from JetStream. (The payload
   that *does* flow is the full rich `BeadCreated{Bead}` — same fidelity as the hub —
   ✅ verified, so that part isn't the problem.)
2. **No scoping in subjects.** The subject is flat `mutations.<EventType>`
   (`server.go:264`, `streams.go:26`) — **no project/type/actor hierarchy**. ✅ verified.
   Project filtering would fall back to in-memory filtering of the full replayed stream
   (negating the durability/efficiency win) or parsing JSON per message via
   `subject_contains`.
3. **New failure domain + new client.** The viewer has **no NATS *event* client today** —
   `cfg.natsURL` is used only to build `ThinkingHub` for telepathy (`main.go:205`),
   ✅ verified. Wiring the bead-event feed directly to NATS adds credential plumbing
   (`natsAgentToken`/`natsInfraToken`), embedded-NATS test infra, and makes NATS a hard
   dependency of the feed path (today the hub is in-process and fail-silent).

JetStream's durability advantage is genuine — but `GET /v1/events` (Option B) already
delivers durable replay from Postgres without any of these costs.

---

## 5. The scale problem all SSE options share (and its in-repo fix)

**Verified key risk:** each browser hitting `handleAgentsEvents` calls
`s.daemon.EventStream(r.Context(), "beads.bead.>")` **once per request**
(`agents_events.go:71`, ✅ verified). There is **no viewer-side pooling**. So:

> **N concurrent browsers ⇒ N upstream daemon SSE connections**, and the daemon hub fans
> every `beads.bead.>` event to all N (`sse.go:82-90`, O(N) per event), with each viewer
> connection **re-filtering the same global firehose independently**. Slow clients are
> silently dropped on a full 64-deep channel (`sse.go:84-89`, ✅ verified) — a dropped
> event is a **missing row** until the next full render.

For a front page (far more concurrent eyeballs than `/agents`), that's the real cost
driver — not storage.

**The fix already exists in this codebase.** `ThinkingHub` (`thinking_hub.go`) opens **one**
upstream NATS subscription in the viewer process, maintains a bounded ring (200 events,
`thinkingRingCapacity`), **backfills the ring from JetStream on startup so a fresh viewer
pod paints immediately** (`backfillFromJetStream`, using `DeliverLastPerSubject` +
`OrderedConsumer`), and fans events out to every connected browser. New browsers get the
ring as instant backfill; there's exactly one upstream consumer regardless of viewer count.

Applying that pattern to the bead-event feed collapses N upstream connections to **one**,
moves filtering to a single in-process pass, and gives every new browser an instant warm
paint — turning the SSE option from "O(N) upstream load" into "O(1) upstream, O(N) local
fan-out," which is cheap.

---

## 6. The house pattern: REST seed + SSE live

The established idiom (`handleIndex` seeds 20 recent beads via `ListBeadsFiltered`, then the
template attaches `agents_stream.js` to an SSE endpoint for live deltas) is **reusable by
every option** and is the backbone of the recommendation:

- **Seed (initial paint, and re-paint after any long gap):** `GET /v1/events?since=&limit=`
  — durable, cursor-paginated, fleet-wide. Fast first render, survives restarts.
- **Live (deltas):** SSE through the viewer fan-out hub (§5), client = `agents_stream.js`.
- **Gap-fill (short reconnect):** the hub's in-memory ring; **(long reconnect / restart):**
  re-seed from `/v1/events?since=<last-seen-ts>` — the same endpoint as the seed.

This is why the recommendation is a **hybrid**: polling's durable endpoint is the
backfill/seed tier; SSE is the live tier; the fan-out hub makes SSE scale. We get
SSE's instant feel *and* polling's durability, without Option C's costs.

---

## 7. Scorecard

| Axis | A: hub SSE | B: REST polling | C: JetStream | **Recommended hybrid** |
|---|---|---|---|---|
| **Reconnection** | Broken end-to-end today (no `id:` through viewer); fixable | Trivial (stateless) | Durable `since=` replay | Trivial seed re-fetch + SSE auto-reconnect |
| **Backfill** | Ring 1000, wiped on restart | **Durable, unbounded by time** | Durable (10k msgs / 100MB) | **Durable via `/v1/events`** |
| **Scoping** | In-mem per-conn | **SQL, expressive** | Flat subjects, weak | SQL seed + in-mem live filter (one pass) |
| **Signal coverage** | All `beads.bead.>` | All events table | **Only 5 mutation topics** | All (hub + events table) |
| **Live latency** | Instant | Poll interval (3–5s) | Instant | **Instant** |
| **Scale (N viewers)** | **O(N) upstream conns** | K/P DB q/s | O(N) NATS subs | **O(1) upstream via fan-out hub** |
| **New failure domain** | None (in-proc) | None | **NATS hard dep** | None (NATS optional, like telepathy) |
| **Net-new code** | Low | Low | **High** | Low–medium (fan-out hub) |

---

## 8. Recommendation & phased build

**Adopt the hybrid (A+B with the `ThinkingHub` fan-out pattern). Defer C.**

- **Phase 1 (MVP, smallest diff):** `/feed` = REST seed from `GET /v1/events` + the
  existing per-connection SSE path (Option A as-is). Ships the feed; accepts the O(N)
  upstream cost while concurrency is low. Reuses `agents_stream.js` wholesale; new code is
  card markup + one handler + a `GetFleetRecentEvents` beadsapi wrapper.
- **Phase 2 (scale, before promotion to `/`):** introduce a viewer-side `BeadFeedHub`
  modelled on `ThinkingHub` — one upstream `EventStream("beads.bead.>")` (or NATS sub),
  fan-out to N browsers, in-memory backfill ring seeded from `/v1/events`. Collapses
  upstream load to O(1) and gives instant warm paint.
- **Reconnect polish:** either forward the daemon's `id:` through the viewer SSE writers to
  light up `Last-Event-ID`, or (simpler, recommended) lean on `/v1/events?since=<last-ts>`
  for gap-fill on reconnect and skip `Last-Event-ID` entirely.
- **Revisit C only if** the feed later needs cross-pod durable replay the viewer ring can't
  give — and only after the dependency/label/advice topics are added to the JetStream
  publish path and project scoping is encoded in subjects.

This keeps the front page **instant and durable**, reuses three proven in-repo mechanisms
(`/v1/events`, `agents_stream.js`, `ThinkingHub`), and adds no new hard infrastructure
dependency before Matt approves promotion.

---

## 7b. Verification ledger

All code claims in this doc were checked by an adversarial verifier against the repo.
Headline verdicts (full detail in task workflow `wf_afdf9290-b4d`):

- ✅ `GET /v1/events` fleet-wide durable endpoint exists, route-registered, tested — CONFIRMED
- ✅ Viewer SSE re-stream drops `id:`; beadsapi client ignores `id:` ⇒ Last-Event-ID broken end-to-end — CONFIRMED
- ✅ In-memory ring = 1000 events, wiped on daemon restart; slow clients dropped (select/default) — CONFIRMED
- ✅ N browsers ⇒ N upstream daemon `EventStream` connections, unpooled — CONFIRMED
- ✅ JetStream carries only 5 mutation topics (no dependency/label/advice); flat `mutations.<EventType>` subjects; viewer has no NATS event client today — CONFIRMED
- ✅ JetStream payload is the full rich bead, same fidelity as the hub — CONFIRMED
- ◐ "REST seed + SSE live" house pattern — CONFIRMED for `/agents`; `/feed`/`/` integration is net-new (the `/feed` route does not exist yet) — PARTIAL
