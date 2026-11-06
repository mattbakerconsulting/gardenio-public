# Activity-feed front page — synthesis & recommendation for Matt

> **Task:** [ka-0000004348](https://beads.gardenio.e2e.example.com/ka-0000004348) ·
> **Epic:** [ka-0000003263](https://beads.gardenio.e2e.example.com/ka-0000003263) ·
> **Operator:** Matt Baker (he/him), requested via Slack (thread bead ka-0000002945)
>
> This is the **final synthesis deliverable** the operator asked for: all five
> research outputs collected, a single clear recommendation, and a promotion
> proposal — ready for Matt to read and pick a direction. **Nothing here promotes
> `/`.** The prototype already landed behind a distinct `/feed` route; promotion is
> a small, explicitly-gated follow-up.

---

## TL;DR (read this if nothing else)

The fleet already has everything needed to build a great activity-feed front page —
the research found that the two hardest pieces (a **durable, fleet-wide events
endpoint** and a **viewer-side fan-out hub with backfill**) **already exist in the
codebase** and just aren't wired into a feed. A **working prototype is already
merged behind `/feed`** (it does not touch `/`).

**Recommendation — one sentence:** Ship **Concept A (unified chronological feed)**
as the `/feed` default with a **Concept B (lanes) view toggle**, delivered as
**REST-seed + SSE-live** using the existing `GET /v1/events` durable endpoint plus a
`ThinkingHub`-style viewer fan-out hub; **defer** direct JetStream and Concept C
(project swimlanes).

**Where it stands:** Phase-1 prototype is live behind `/feed` (MR !4295). What
remains before promoting `/feed` to `/` is the Phase-2 scale fix (fan-out hub) +
reconnect backfill + richer signal families — all scoped below. **Matt's decision:
pick the default arrangement and approve (or hold) promotion.**

---

## 1. What was asked

The current front page (`handleIndex` → `index.html`) is a *useful but static
snapshot*: a stat banner + the 20 most-recently-**updated** in-progress beads + a
right rail, rendered once with **zero realtime behaviour**. Matt wanted a **living
activity-feed front page** — "what is the whole fleet doing right now" — surfacing
recently in-progress / created / accomplished work as it happens, landed behind a
distinct route (`/feed`) and promoted to `/` **only on his explicit go-ahead**.

The epic fanned out into five research tasks (audit, data-source inventory,
realtime delivery, IA/UX, prototype). All five are **complete**. This doc folds
them together.

---

## 2. The five research outputs (all complete)

| # | Task | Output | Bundle / MR |
|---|---|---|---|
| 1 | **Audit** current front page + adjacent pages ([ka-0000008172](https://beads.gardenio.e2e.example.com/ka-0000008172)) | `beads-viewer-frontpage-audit.md` | [/bundle/ka-0000006864](https://beads-viewer.e2e.example.com/bundle/ka-0000006864) · MR !4289 |
| 2 | **Data-source inventory** of every feed signal ([ka-0000002035](https://beads.gardenio.e2e.example.com/ka-0000002035)) | Signal inventory (25+ signals → sources) | [/bundle/ka-0000004916](https://beads-viewer.e2e.example.com/bundle/ka-0000004916) · MR !4280 |
| 3 | **Realtime delivery** options ([ka-0000002760](https://beads.gardenio.e2e.example.com/ka-0000002760)) | `research-feed-realtime-delivery.md` | [/bundle/ka-0000006084](https://beads-viewer.e2e.example.com/bundle/ka-0000006084) · MR !4283 |
| 4 | **IA + UX** proposals (3 concepts) ([ka-0000002322](https://beads.gardenio.e2e.example.com/ka-0000002322)) | `beads-viewer-activity-feed-ux-proposals.md` + mockups | [/bundle/ka-0000003010](https://beads-viewer.e2e.example.com/bundle/ka-0000003010) · MR !4282 |
| 5 | **Prototype** behind `/feed` ([ka-0000003020](https://beads.gardenio.e2e.example.com/ka-0000003020)) | `beads-viewer-feed-prototype.md` + live page | [/bundle/ka-0000007769](https://beads-viewer.e2e.example.com/bundle/ka-0000007769) · MR !4295 |

### 2.1 Audit — what we have, what's missing

- Front page and `/active` are **fully static snapshots**, not feeds. The "Recent
  activity" panel is mislabelled: it's the 20 most-recently-*updated* in-progress
  beads keyed on `updated_at`, with no events, actors, or closures — a
  current-state list, not an event log.
- **Three independent realtime mechanisms already exist** and the front page uses
  none of them: `/agents/events` (bead-lifecycle SSE), per-agent `/agents/{id}/events`,
  and `/telepathy/events` (the NATS thinking feed with a ring buffer + JetStream
  backfill).
- **Reuse foundation:** the `/telepathy` `ThinkingHub` is the only mechanism with a
  real backfill story; `computeIndexLiveAgents` is the strongest existing "feed row"
  builder; `ThinkingEvent` is nearly the right unified event schema.
- **Headline gap:** the rich "what just happened" signals exist as data but are
  never assembled into one chronological, realtime stream. Recommended an
  **`ActivityHub` modeled on `ThinkingHub`**, fed by `beads.bead.>` lifecycle events.

### 2.2 Data-source inventory — 25+ signals mapped

- Mapped 25+ signals (created/claimed/closed, status transitions, comments,
  squawks, MR open/merge, decisions, agent spawn/stop, pipeline) to their kbeads
  topics / viewer SSE / durable backfill sources. **Independently cross-validated**
  by a second agent (agent `ahg`) against `kbeads/internal/events` + eventbus +
  viewer SSE handlers — no factual deltas.
- **Key findings:**
  - There is **no dedicated `claimed` topic** — derive it from
    `BeadUpdated.changes.status → in_progress`.
  - The **load-bearing constraint:** both live SSE endpoints (`/v1/events/stream`
    and `/v1/bus/events`) are gated `RoleOperator`. A viewer `/feed` multiplexer
    needs an operator-scoped token exactly like today's `/agents/events` already
    uses (it already calls `s.daemon.EventStream`).
  - Day-one prototype = **purely a new viewer-side consumer** reusing existing
    operator-token `EventStream` plumbing; **zero new daemon endpoints** for the
    bead-lifecycle lane. The remaining gap is a unified multi-family
    `/feed/events` multiplexer for non-lifecycle families (decisions/mail/gate/
    pipeline) — that's the follow-up, not the MVP.

### 2.3 Realtime delivery — REST-seed + SSE-live hybrid

- **The decisive finding:** the durable, fleet-wide events endpoint everyone
  assumed we'd have to build **already exists and is route-registered + tested** —
  `GET /v1/events?since=&until=&limit=&cursor=` (Postgres-backed, cursor-paginated,
  `RoleViewer`). This is the backfill/seed tier for free.
- **Recommendation: hybrid of SSE + polling, NOT NATS-direct.**
  1. **Initial paint + gap-fill** → `GET /v1/events` (durable, survives restart).
  2. **Live deltas** → SSE, reusing the proven `agents_stream.js` client.
  3. **Scale fix** → route the live stream through a **viewer-side fan-out hub**
     modelled on `ThinkingHub`. Today each browser opens its own upstream daemon
     SSE connection (N viewers ⇒ N connections re-filtering the whole firehose);
     the hub collapses that to **one upstream → many browsers + instant warm paint**.
- **Defer direct JetStream (Option C):** it drops 3 of the signal classes (only 5
  mutation topics reach the bus), has flat subjects (no project scoping), and adds
  a new hard failure domain — all for a durability win `/v1/events` already
  delivers. Every code claim was adversarially verified.

### 2.4 IA + UX — three concepts, recommend A + B toggle

Three full-page wireframes (PNGs committed under `feed-mockups/`, SVG sources
alongside per advice ka-0000000334):

- **Concept A — unified chronological feed:** one newest-first stream, typed
  color-coded cards, "needs you" right rail. *Lowest cognitive load, cheapest
  realtime (1 stream), reuses the most code.* Trade-off: a busy fleet is a firehose
  (mitigated by the pinned rail + type-mute filters).
- **Concept B — now / recently-changed / just-done lanes:** kanban-style spatial
  stability; completion feels visible. Trade-off: loses strict global chronology.
- **Concept C — project-grouped swimlanes:** portfolio view, "which projects are
  hot/idle/blocked" at a glance. Trade-off: highest data cost (N×aggregation),
  sacrifices cross-project chronology.

**Recommendation:** ship **A as the default**, offer **B as a `?view=` toggle**
(pure client-side re-bucketing of the *same* stream — cheap once A exists), and
treat **C as a separate "Projects" mode** or fold it into `/active` / per-project
pages. One `?view=` param + one SSE subscription keeps the build small.

### 2.5 Prototype — already live behind `/feed`

A working, opt-in `/feed` page is **already merged (MR !4295)** and verified
against the in-cluster daemon:

- **Concept A** (chronological stream) is the default; **Concept B** (lanes) is a
  `?view=lanes` toggle — client-side re-bucketing, never re-subscribes.
- A **"needs you" rail** pins open decisions, hot ready queue, and live agents.
- First paint reuses `computeIndexRecent` + `computeIndexRail`; live deltas via
  `EventSource` on `/feed/events` → `beadAPI.EventStream("beads.bead.>")` — the
  same idiom as `agent_events.go` / `telepathy.go`, no htmx-sse extension needed.
- Cards are classified into the proposal's color taxonomy (decision=amber,
  mr-merged=purple, claimed=blue, blocked=red, created/updated=grey, closed=green).
- Files: `feed.go` (handler + SSE + card renderer), `feed.html`, `feed.js`,
  `feed_test.go`. **`/` is untouched.**

---

## 3. The synthesis: one coherent design

The five outputs converge cleanly — they are not competing options, they compose
into a single design:

```
                    ┌─────────────────────────────────────────────┐
   First paint /    │  GET /v1/events?since=&cursor=  (durable,    │
   gap-fill backfill│  Postgres-backed, ALREADY EXISTS)           │  ← realtime task
                    └─────────────────────────────────────────────┘
                                      │ seed cards
                                      ▼
   Live deltas      ┌─────────────────────────────────────────────┐
   (Phase 2 fan-out)│  BeadFeedHub  (one upstream EventStream      │  ← audit + realtime
                    │  "beads.bead.>", fan-out to N browsers,      │     (ThinkingHub pattern)
                    │  bounded ring, instant warm paint)          │
                    └─────────────────────────────────────────────┘
                                      │ pre-rendered HTML cards
                                      ▼
   Presentation     ┌─────────────────────────────────────────────┐
                    │  /feed  ·  Concept A default + ?view=lanes   │  ← IA/UX + prototype
                    │  typed color cards + "needs you" rail        │
                    └─────────────────────────────────────────────┘
```

- **Audit** says: build an `ActivityHub` like `ThinkingHub`, sourced from
  bead-lifecycle events; collapse the three static "what's happening" lists into
  one event-sourced surface.
- **Data-source inventory** says: the bead-lifecycle stream already covers
  create/claim/close/decision/MR-bead — day-one needs no new daemon endpoint; the
  multi-family multiplexer is a follow-up.
- **Realtime** says: seed from the durable `/v1/events`, live via SSE, scale via
  the fan-out hub; defer JetStream.
- **IA/UX** says: Concept A default + B toggle; defer C.
- **Prototype** says: all of the above already works end-to-end behind `/feed` at
  Phase-1 fidelity (per-connection SSE, lifecycle stream only).

---

## 4. Recommendation

**Adopt the unified design above. Concretely:**

1. **Default arrangement: Concept A** (unified chronological feed) at `/feed`, with
   **Concept B as a `?view=lanes` toggle** (already prototyped).
2. **Delivery: REST-seed + SSE-live hybrid.** Seed/backfill from the existing
   `GET /v1/events`; live deltas via SSE. **Defer direct JetStream.**
3. **Defer Concept C** (project swimlanes) to a separate "Projects" mode or fold
   into `/active`.
4. **Keep `/` untouched** until Matt explicitly approves promotion.

This is the lowest-risk, lowest-cost, highest-fidelity answer to the brief, and it
reuses three proven in-repo mechanisms (`/v1/events`, `agents_stream.js`,
`ThinkingHub`) plus the already-merged prototype — adding **no new hard
infrastructure dependency** before promotion.

---

## 5. Promotion proposal (phased)

The prototype is **Phase 1**. Two phases remain before `/feed` is promotion-ready,
then promotion itself is a small, explicitly-gated step.

| Phase | Scope | Status |
|---|---|---|
| **1 — MVP (shipped)** | `/feed` route, Concept A default + B toggle, "needs you" rail, live SSE on `beads.bead.>` via per-connection `EventStream`, REST seed from `computeIndexRecent`. `/` untouched. | ✅ **Merged (MR !4295)** |
| **2 — Scale & durability (pre-promotion)** | Viewer-side `BeadFeedHub` (one upstream → N browsers, bounded ring, instant warm paint); seed/gap-fill from `GET /v1/events?since=` rather than `computeIndexRecent`; reconnect backfill (lean on `?since=`, skip `Last-Event-ID`). | ✅ **Merged (ka-0000000643)** |
| **3 — Richer signals (pre/post-promotion)** | Multi-family `/feed/events` multiplexer for decisions / mail / gate / pipeline / squawk-text / agent spawn-stop, plus typed MR-merge + pipeline events from the GitLab bridge. | ✅ **Built (ka-0000006994)** — see §9 |
| **4 — Promotion** | On Matt's explicit go-ahead: make `/feed` the default landing experience (or A/B behind a flag). Small diff: swap the `/` handler or add a redirect/flag. | 🔒 **Gated on Matt** |

**Suggested build epic** (only on Matt's pick): one epic with Phase-2 and Phase-3
as child tasks, each independently shippable behind `/feed`, with Phase-4 promotion
as a final gated decision bead (not a plain task — a real `type=decision` gate).

---

## 6. Open questions for Matt (the decision)

These are the choices the research surfaced that need a human pick:

1. **Default view** — Concept A (stream), B (lanes), or C (projects)? *Research
   recommends A default + B toggle.*
2. **Event scope** — all projects by default, or scoped to the viewer's project(s)?
3. **Noise floor** — are `spawned` / `squawk` / routine `claimed` events on by
   default, or opt-in via Filters?
4. **Backfill depth** — how far back does the feed load on first paint, and do we
   backfill missed events on reconnect? *Research recommends `?since=<last-ts>`
   gap-fill.*
5. **Promotion** — promote `/feed` to `/` once Phase 2 lands, A/B it behind a flag,
   or hold at `/feed` indefinitely?

---

## 7. How to look at it right now

- **Read the bundles** (linked in §2) — each research task's full doc + mockups.
- **See the prototype live:** open `/feed` (and `/feed?view=lanes`) on a
  beads-viewer that has the !4295 build. `/` is unchanged.
- **Mockups:** `gardenio/docs/feed-mockups/` — Concept A / B / C PNGs (SVG sources
  alongside).

---

## 8. Decision resolved — build authorized (2026-06-14)

Matt resolved the synthesis decision [ka-0000000205](https://beads.gardenio.e2e.example.com/ka-0000000205)
by choosing **approve-a**: approve **Concept A default + Concept B `?view=` toggle**
and authorize a **build epic** for the remaining phases, with promotion of `/feed`
to `/` as a final **gated** step.

**Build epic filed:** [ka-0000004907](https://beads.gardenio.e2e.example.com/ka-0000004907)
— *EPIC: Build activity-feed front page (/feed → promotion)*.

| Child | Bead | Notes |
|---|---|---|
| Phase 2 — scale & durability | [ka-0000000643](https://beads.gardenio.e2e.example.com/ka-0000000643) | `BeadFeedHub` fan-out + durable `/v1/events` seed/backfill |
| Phase 3 — richer signals | [ka-0000006994](https://beads.gardenio.e2e.example.com/ka-0000006994) | multi-family `/feed/events` multiplexer + GitLab-bridge MR/pipeline events |
| Per-viewer scoping | [ka-0000003403](https://beads.gardenio.e2e.example.com/ka-0000003403) | default user+project scoping (re-parented under the build epic) |
| Phase 4 — promotion **GATE** | [ka-0000004619](https://beads.gardenio.e2e.example.com/ka-0000004619) | `type=decision`, blocked-by Phase 2; promotes `/feed`→`/` only on Matt's go-ahead |

`/` remains untouched. Promotion stays gated behind the Phase 4 decision.

---

## 9. Phase 3 — multi-family multiplexer (2026-06-15, ka-0000006994)

The feed now surfaces **every** signal family the data-source inventory
([/bundle/ka-0000004916](https://beads-viewer.e2e.example.com/bundle/ka-0000004916))
mapped — not just the bead-lifecycle lane. The decisive finding the inventory
made explicit: **every family is already a typed bead flowing through the one
`beads.bead.>` lifecycle stream.** Squawks are `type=message`+`squawk` beads,
mail is `type=mail`, agent spawn/stop and gate transitions are `type=agent`
create/close/update, and MR open/merge + pipeline status are
`type=merge-request` fields the GitLab bridge writes (`gitlab_sync.go`:
`mr_state`, `mr_pipeline_status`). The prototype was *dropping* most of these
via the shared `recentActivityNonActionableTypes` type filter — so the
multiplexer needs **no new daemon endpoint and no new upstream subscription**
(consistent with the realtime research, §2.3). It rides the same Phase-2
`BeadFeedHub` single upstream stream; only the **classifier** broadened.

### 9.1 Family → source → kind mapping

| Family | Bead source (all on `beads.bead.>`) | Detection | Feed kind · color |
|---|---|---|---|
| Decision | `type=decision` create/close | bead type | `decision` · amber ("needs you") |
| Claim | work bead → `in_progress` | status transition | `claimed` · blue |
| Blocked | work bead → `blocked` | status transition | `blocked` · red |
| Created / Closed | work bead create / close | lifecycle topic | `created` grey · `closed` green |
| MR opened | `type=merge-request` create | bead type + create | `mr-opened` · purple (light) |
| MR merged | `type=merge-request`, `mr_state=merged` | field | `mr-merged` · purple |
| Pipeline | `type=merge-request`, `mr_pipeline_status` terminal | field (`success`/`failed`/`canceled`) | `pipeline` · green ok / red failed |
| Squawk | `type=message` + `squawk` label, on create | type + label | `squawk` · teal |
| Mail | `type=mail`, on create | bead type | `mail` · teal |
| Agent spawned | `type=agent` create | bead type + create | `spawned` · grey |
| Agent stopped | `type=agent` close or `agent_state∈{stopping,stopped,terminated}` | type + close/field | `stopped` · grey |
| Gate transition | `type=agent`, `gate_satisfied_by` set on update | field | `gate` · amber |

### 9.2 Noise control

Routine per-family churn is dropped at the classifier (zero kind), not just
muted client-side: an agent **heartbeat** update (still `working`, no gate) and
a **running/pending** pipeline produce no card. The type-mute chips cover every
new family; per the noise-floor question (§6.3), **`squawk` and `spawned` ship
muted-by-default** (chips render `aria-pressed="false"`, `feed.js` seeds
`mutedTypes` from that state) — the operator un-mutes when they want the
firehose. The remaining pure-meta types (`schedule`, `advice`, `rating`,
`generic`, `config`) stay filtered entirely.

### 9.3 Files touched (all behind `/feed`; `/` untouched)

- `feed.go` — `classifyFeedEvent` / `feedKindForBead` broadened into per-family
  dispatch (`feedMRKind`, `feedAgentKind`, `feedWorkKind`); new kinds + glyphs /
  labels / actors; pipeline ok/failed card modifier.
- `feed.html` — mute chips for the new families; taxonomy CSS.
- `feed.js` — `EVENT_NAMES` binds the new SSE event names; `mutedTypes` seeded
  from server-rendered chip state.
- `feed_phase3_test.go` — family classification, noise-drop, render, and actor
  coverage.

---

*Synthesis by gardenio-synthesis-assemble-bundle-zgp (crew). Operator: Matt Baker.
No code shipped by this task — it collects the five research deliverables into one
recommendation. Promotion of `/` remains gated on Matt's explicit approval.*
