# Audit: current front page + adjacent pages

> **Task:** ka-0000008172 · **Epic:** ka-0000003263 (Research: new beads-viewer activity-feed front page) · **Operator:** Matt Baker
> All file:line references read from `gardenio/controller/cmd/beads-viewer/` at `main` (60e1fa8). Spot-checked for accuracy.

## 1. Summary

The current front page (`GET /{$}` → `handleIndex`, `server.go:1876`) is a **fully static, server-rendered dashboard**: on each request it fans out four parallel best-effort daemon queries — a stats banner, a "Recent activity" list, a multi-panel right rail (Hot ready / Live agents / Open decisions / Hot epics / MR status), and per-project counts — then renders `index.html` once with no SSE or polling. It is a good *snapshot* but has **zero realtime behaviour**: nothing updates after first paint, and the "Recent activity" panel is not an activity feed at all — it is the 20 most-recently-*updated* in-progress beads (`computeIndexRecent`, `server.go:1797`), filtered to actionable types, with no events, no actors, no closures, and no comments/squawks. The platform already has three independent realtime mechanisms the front page does not use — the bead-lifecycle SSE fan-out behind `/agents/events` (`agents_events.go:56`) and per-agent `/agents/{id}/events` (`agent_events.go:51`), and the NATS-backed thinking feed behind `/telepathy/events` (`telepathy.go:56`, `thinking_hub.go`). The headline gap for an activity feed is that **the richest "what just happened" signals (lifecycle transitions, squawks, thinking, comments, MR/decision state) exist as data but are never assembled into a single chronological, realtime stream**; today they are scattered across static lists (front page, `/active`, `/me`) and topic-siloed SSE pages.

## 2. Per-page inventory

### 2.1 Front page — `GET /{$}`

- **Route + handler:** `server.go:917` → `handleIndex` (`server.go:1876`).
- **Purpose:** Landing dashboard / fleet snapshot. Packs banner stats + projects + recent + right rail above the fold (ka-0000002715).
- **Data surfaced (sources):**
  - **Stats banner** — `computeIndexStats` (`server.go:1556`): totals by status (`daemon.GetStats`), type/field counts (`ListConfigs`), open decisions, **live agents** (status open/in_progress ∩ `agent_state ∈ {working,spawning,yielding}`, classified client-side), **ready-queue depth** (`NoOpenDeps`), **escalations pending** (`ListEscalations`), **P0 open** (priority-sorted scan). View struct `indexStatsView` (`server.go:1525`).
  - **Recent activity** — `computeIndexRecent` (`server.go:1797`): 20 most-recently-updated `in_progress` beads, `Sort:"-updated_at"`, over-fetched ×3 and client-filtered against `recentActivityNonActionableTypes` (`server.go:1782`: agent/decision/schedule/advice/rating/generic/mail/config). View struct `indexRecentItem` (`server.go:1704`).
  - **Right rail** — `computeIndexRail` (`index_rail.go:132`): five parallel panels — Hot ready (`computeIndexHot`, `index_rail.go:231`, `NoOpenDeps`, priority-sorted), **Live agents** (`computeIndexLiveAgents`, `index_rail.go:283` — enriched with role, task title via one `BatchGetBeads`, uptime, stale flag, MR/decision badges, pod phase, and **latest squawk** joined in one batched call `latestSquawksByAgent`, `index_rail.go:423`), Open decisions (`index_rail.go:534`), Hot epics (`index_rail.go:183`), MR status (`computeIndexMRStatus`). View structs `indexRail`/`indexLiveAgent`/`indexHotBead`/`indexOpenDecision`/`indexHotEpic` (`index_rail.go:29-124`).
  - **Projects** — `computeIndexProjects` (`server.go:1717`): per-project open/in-progress/blocked/deferred/decisions counts (5 parallel count-only queries each, `Kinds:["issue"]`). View struct `indexProjectView` (`server.go:1692`).
  - **Chrome** — `ViewerEmail` (OIDC) + `InboxCount` (`inboxCountForViewer`, `inbox.go:1156`).
- **What's useful:** The Live-agents panel is the closest thing to a feed today — it already carries actor (operator chip), task, state, uptime/stale, squawk preview, MR/decision deep-links, all with **O(1) round-trips**. The stats banner is a strong "fleet pulse" header. These are directly reusable.
- **What's noise (for a feed):** "Recent activity" is mislabelled — it is a *current-state* list keyed on `updated_at`, not an event log; the same bead never appears twice and a closure simply drops off. Per-project count grids are dashboard furniture, not feed signal. Hot ready / Hot epics are queue/backlog views, orthogonal to "what just happened".

### 2.2 `/active` — in-progress overview

- **Route + handler:** `server.go:919` → `handleActive` (`active.go:72`).
- **Purpose:** "What's happening right now" — all `in_progress` beads grouped by project, with a 4-count summary bar (Agents · Active · Decisions · Queued) and a decision banner. Mirrors iOS InProgressTab (`active.go:61`).
- **Data surfaced (sources):** `ListBeadsFiltered(Statuses:["in_progress"], Limit:500, ExcludeScheduled)` (`active.go:119`); decisions count (`active.go:145`); queued count via `countQueuedBeads` (count-only, `ExcludeTypes:["agent"]`, `active.go:235`). Default-on viewer scoping by operator (`effectiveOperatedBy`, `active.go:85`). Sorted by `UpdatedAt` desc (`active.go:171`). View structs `activeSummary` (`active.go:54`), `activeProjectGroup` (`active.go:48`); rows reuse `projectBeadView` via the shared `project_row` partial.
- **What's useful:** The summary bar (Agents/Active/Decisions/Queued) is a compact realtime-worthy counter set; the operator-scoping logic (`resolveDefaultOperatedBy`, `resolveOperatedByParam`) is reusable "my work" filtering. Recency-first sort is feed-aligned.
- **What's noise:** Project grouping fragments the chronology a feed wants. Like the front page it is **static** — no SSE/polling (confirmed: no `EventSource`/`hx-` in `active.html`), despite the "feels live" intent in the doc comment.

### 2.3 `/me` → `/u/{slug}` — per-user homepage

- **Route + handler:** `server.go:920` → `handleMe` (`user_home.go:230`) → `redirectMeToViewer` (308 to `/u/{slug}`, `user_home.go:238`).
- **Purpose:** Personalized homepage; resolves viewer operator id → slug and redirects to the canonical 8-section dashboard (`buildUserHomeSections`, `user_home.go:314`).
- **Data surfaced (sources):** Eight sections (`user_home.go:31-58`): pinned, active agents, open decisions, recent comments (lazy), open MRs, recent commits, recent merged (14d windows, `user_home.go:23`), forwarded mentions (lazy), inbox (lazy, chat-style merge of mail+mention squawks + slack replies — `sectionInbox`, `user_home.go:40`), their automations (schedules), their gardeners. View structs `userHomeSection`/`userHomeBeadRow` (`user_home.go:60-95`). Heavy sections are HTMX-lazy via `LazyURL` (`user_home.go:94`).
- **What's useful:** This is the **richest per-actor surface** and already enumerates feed-grade signals a fleet feed lacks elsewhere: comments, commits, MR opens/merges, forwarded mentions, and a **bidirectional inbox feed** (`sectionInbox`) merging agent→user and user→agent messages. The lazy-section pattern (`runUserHomeSection`, eager fan-out + `hx-trigger="load"`) is a reusable load model.
- **What's noise:** Strongly per-user and section-siloed (each section is its own list/query); no cross-section chronology, no realtime. The 14-day windows are reporting windows, not a live feed.

### 2.4 Adjacent realtime pages (for comparison)

- **`/agents` + `/agents/events`** — `server.go:963-964` → `handleAgents` (`agents.go:1196`), `handleAgentsEvents` (`agents_events.go:56`). Fleet agent table; the events endpoint is an **SSE stream of bead-lifecycle events filtered to `type=agent`** (topic `beads.bead.>`, `agents_events.go:19`), emitting pre-rendered `<tr>` rows keyed by `data-bead-id` (`renderAgentsIndexRow`, `agents_events.go:277`). Client wiring: `agents.html:179` `data-agents-stream=…/agents/events`. Optional `?project=` / `?schedule_id=` (gardener) scoping.
- **`/telepathy` + `/telepathy/events`** — `server.go:961-962` → `handleTelepathy` (`telepathy.go:37`), `handleTelepathyEvents` (`telepathy.go:56`). Unified cross-agent **thinking** feed over NATS (`coop.events.messages.*.thinking`), served via `ThinkingHub` (`thinking_hub.go:111`) with a 200-event in-memory ring + JetStream tail backfill. Pre-rendered per-event rows (`renderThinkingRow`, `telepathy.go:127`); client `telepathy.html:76` `data-events-url`.
- **Per-agent `/agents/{id}/events`** — `server.go:968` → `handleAgentEvents` (`agent_events.go:51`). SSE of lifecycle events filtered to one agent's interest set (the agent bead, its `task_id` parent, and reverse-dep descendants — `buildAgentEventFilter`, `agent_events.go:122`). Pre-rendered rows (`renderAgentEventRow`, `agent_events.go:194`).

## 3. Reusable building blocks

### Data / query helpers (Go funcs)
| Symbol (file:line) | What it gives a feed |
|---|---|
| `computeIndexStats` (`server.go:1556`) | Fleet pulse header: status totals, live agents, ready depth, escalations, P0 open — ready-made feed header counters. |
| `computeIndexRecent` (`server.go:1797`) | Recently-updated actionable beads + `recentActivityNonActionableTypes` (`server.go:1782`) noise filter — reuse the *filter*, replace the query with an event query. |
| `computeIndexLiveAgents` (`index_rail.go:283`) | Per-agent rows enriched with actor/task/state/uptime/stale/MR/decision/pod, O(1) round-trips — the strongest existing "feed row" builder. |
| `latestSquawksByAgent` / `squawkPreview` (`index_rail.go:423`, `:466`) | Batched newest-squawk-per-agent join — the "what the agent just said" signal without N+1. |
| `countQueuedBeads` (`active.go:235`) | Count-only daemon query idiom (`CountOnly`, `Limit:1`) — cheap counters for a feed header. |
| `resolveDefaultOperatedBy` / `resolveOperatedByParam` (used in `active.go:78,85`) | "My work" operator scoping — reuse for a personalized feed lens. |
| `buildAgentEventFilter` (`agent_events.go:122`) | Interest-set expansion (bead + parent task + reverse-deps) — reusable for "scope feed to this bead/epic". |
| `buildUserHomeSections` + `runUserHomeSection` (`user_home.go:314`) | Parallel section fan-out + lazy-section model — pattern for a multi-lane feed page. |
| `inboxCountForViewer` (`inbox.go:1156`) | Chrome unread/inbox badge — feed "unread since last visit" counter. |
| `formatAgo` (`server.go:1501`) / `fmtTimeAgo` (`active.go:270`) | Relative-time labels for feed timestamps. |
| `projectFromLabels`, `agentEventProject` (`agents_events.go:249`) | Project derivation from labels/fields — feed grouping/filtering. |

### SSE / realtime infra
| Symbol (file:line) | What it gives a feed |
|---|---|
| `Client.EventStream` (`beadsapi/client_events.go:72`) | The core daemon SSE consumer (topic glob, 4 MB buffer) — the bead-lifecycle event source for a feed. |
| `handleAgentsEvents` (`agents_events.go:56`) | Working fleet-wide lifecycle SSE handler w/ project/gardener scoping — closest template for a feed SSE endpoint. |
| `handleAgentEvents` (`agent_events.go:51`) | Scoped (interest-set) lifecycle SSE — per-entity feed. |
| `ThinkingHub` (`thinking_hub.go:111`): `Subscribe`/`broadcast`/`Recent` | In-process fan-out hub with **bounded ring (200) + per-client buffer + drop-oldest backpressure + JetStream backfill** — the only mechanism today with a real backfill story. |
| `agentEventKind` (`agent_events.go:177`) | Topic → {created,updated,closed} normalization — feed event-type mapping. |
| `agents_events.go:21-34` `handleAgentsEventsStatus` | Pre-EventSource auth probe (Firefox SSE-blocked workaround) — reuse for any new SSE endpoint behind OIDC/AVA. |

### View structs
| Symbol (file:line) | Note |
|---|---|
| `indexLiveAgent` (`index_rail.go:58`) | Most feed-complete row model (actor chip, task, state, uptime, stale, squawk, MR, decision, pod). |
| `indexRecentItem` (`server.go:1704`) | Minimal recent-row (id/title/type/status/project/ago) — feed row baseline. |
| `indexStatsView` (`server.go:1525`) | Feed header counters. |
| `activeSummary` (`active.go:54`) | Agents/Active/Decisions/Queued counter bar. |
| `userHomeBeadRow` / `userHomeSection` (`user_home.go:61,82`) | Lazy multi-section row/section models. |
| `ThinkingEvent` (`thinking_hub.go:44`) | Normalized cross-agent event (actor, text, project, role, agent_bead_id, pod, ts) — a near-ideal feed event schema. |

### Template partials / htmx widgets
| Asset | Note |
|---|---|
| `project_row` partial (reused by `/active`, `/p/<project>`, `index`) | Shared bead-row markup. |
| `gardener_iteration_card` (`templates/partials/gardener_iteration_card.html`, named tpl `gardener_iteration_card`) | Reused for streamed live cards (`agents_events.go:161`) — proves a partial can back both eager render and SSE swap. |
| `data-agents-stream` (`agents.html:179`), `data-events-url` (`telepathy.html:76`) | Client SSE-attach idiom — prepend/swap pre-rendered rows by `data-bead-id`; no htmx-sse extension needed. |
| operator chip (`operator_chip.go` / `operator_initials.go`, via `indexAgentOperatorChip` `index_rail.go:488`) | Compact actor monogram for feed rows. |

## 4. Realtime infrastructure assessment

| Mechanism | Topic / event model | Client pattern | Reconnection / backfill |
|---|---|---|---|
| **`/agents/events`** (`agents_events.go:56`) | Daemon SSE, topic `beads.bead.>`, filtered server-side to `type=agent` (+ optional project / gardener scope). One pre-rendered `<tr>`/`<li>` per lifecycle event (created/updated/closed). | EventSource attaches via `data-agents-stream`; rows swapped/inserted by `data-bead-id`. | **No backfill.** Browser EventSource auto-reconnects, but events between drop and reconnect are lost; full state only reconciles on next page render. Per-connection filter computed at connect. |
| **`/agents/{id}/events`** (`agent_events.go:51`) | Same daemon SSE, filtered to a per-agent interest set (bead + parent task + reverse-deps). | Same EventSource idiom. | **No backfill**, and the interest set is **frozen at connect** — new descendants need a reconnect (`agent_events.go:48`). |
| **`/telepathy/events`** (`telepathy.go:56`, `thinking_hub.go`) | NATS `coop.events.messages.*.thinking`; normalized `ThinkingEvent`. In-process `ThinkingHub` fan-out. | EventSource via `data-events-url`; multi-line `data:` per thought. | **Strongest:** bounded 200-event ring + per-client buffer with drop-oldest backpressure (`thinking_hub.go:340`), and **JetStream tail backfill on startup** (`DeliverLastPerSubject`, `thinking_hub.go:210`) so a fresh subscriber paints immediately. NATS auto-reconnect with `MaxReconnects(-1)`. Nil-safe degrade when NATS absent. |

**Recommendation: model the unified activity feed on the `ThinkingHub` fan-out architecture, but source it from bead-lifecycle events (the `/agents/events` data) rather than only thinking.** Reasoning:

1. **Backfill is the differentiator.** A feed is worthless if it opens blank or silently loses events across a reconnect. Only the hub solves this (ring + JetStream replay + Subscribe-time backfill, `thinking_hub.go:375`). The raw `/agents/events` path has none of it.
2. **In-process fan-out scales to many viewers off one upstream subscription.** `/agents/events` opens a fresh `daemon.EventStream` per browser connection (`agents_events.go:71`); under many tabs that is N daemon SSE connections. A hub multiplexes one upstream → many clients (the hub already does this for thinking).
3. **`ThinkingEvent` is nearly the right schema** (actor, project, role, agent_bead_id, ts, received-at monotonic clock) and the hub's normalization/dedup/empty-drop discipline is exactly what a feed needs.
4. **Keep the bead-lifecycle topic + `agentEventKind` mapping** from `/agents/events` as the primary event source (created/updated/closed across all types, not just agents), and optionally **fold thinking + squawks in as additional event kinds** on the same hub — giving one chronological stream.

Net: a new `ActivityHub` modeled on `ThinkingHub` (ring + backfill + drop-oldest), fed by `Client.EventStream("beads.bead.>")` plus the existing NATS thinking subject, exposed at one `/feed/events` SSE endpoint with the `handleAgentsEventsStatus` auth-probe pattern.

## 5. Gaps & overlaps

### Signals an activity feed wants but no current page surfaces as events
- **Lifecycle transitions as a chronological log.** Front page "Recent" and `/active` show *current state* keyed on `updated_at`; neither shows created→in_progress→closed *transitions* with the actor who caused them. The data exists (`agentEventSSE.ClosedBy`, `Changes`, `agent_events.go:23-27`) but only on per-agent/agents SSE, never aggregated.
- **Comments / mentions / squawks in a fleet stream.** Squawks are joined per-agent on the front page rail (`latestSquawksByAgent`) and inbox merges them per-user (`sectionInbox`), but there is no *fleet-wide* chronological "agent X said Y" / "comment on bead Z" feed.
- **MR open/merge and decision open/resolve as events.** `/me` reports these in 14-day windows; the front page shows MR/decision *badges*. No realtime event when an MR merges or a decision resolves.
- **Thinking integrated with work events.** `/telepathy` is a siloed reasoning stream with no bead-lifecycle context interleaved.

### Static today, should be realtime
- **The entire front page** (`handleIndex`) — banner counts, live-agent rail, recent list: one render, never updates. The live-agent rail in particular is begging for SSE (it already has `data-bead-id`-style rows available via `renderAgentsIndexRow`).
- **`/active`** — explicitly intended to "feel live" (`active.go:66`) but has no SSE/poll; summary bar and rows go stale immediately.
- **Stats banner counters** (live agents, escalations, ready depth) — naturally event-driven.

### Overlaps / redundancy
- **Front-page "Recent" vs `/active` vs `/me` active-agents** all answer "what's in progress" from overlapping `in_progress`/agent queries with different scoping and grouping: front page = global recent-updated; `/active` = global grouped-by-project (operator-scoped by default); `/me` = operator-scoped sections. Three code paths, three view models, one underlying question.
- **Live-agents logic is duplicated:** `computeIndexLiveAgents` (`index_rail.go:283`) and the `/agents` page (`handleAgents`) and `/active` agent counting all independently classify "alive" via `agent_state ∈ {working,spawning,yielding}` (`isAgentStateIn`/`agentStateAlive`).
- **Decisions appear on every surface** (front-page banner + rail panel, `/active` banner+count, `/me` section) — a feed could subsume these as decision-open events.

A unified feed is an opportunity to **collapse the three "what's happening" lists into one event-sourced surface** with pluggable scope lenses (all / my work / this project / this epic), reusing the existing scoping helpers.

## 6. Recommendations for the new feed (handoff)

**For data-source-inventory:**
- Treat `Client.EventStream("beads.bead.>")` (`beadsapi/client_events.go:72`) as the spine; map topics via `agentEventKind` (`agent_events.go:177`) into {created, updated, closed}. The `agentEventSSE` wrapper (`agent_events.go:23`) already carries `ClosedBy` and `Changes` — feed actor + diff come for free.
- Enumerate non-lifecycle event sources: NATS thinking (`thinking_hub.go:18`), squawks (`type=message,label=squawk`, `index_rail.go:424`), comments, MR/merge/decision transitions. Reuse `recentActivityNonActionableTypes` (`server.go:1782`) as the default noise filter and make it configurable per lens.
- Reuse `indexLiveAgent` (`index_rail.go:58`) and `ThinkingEvent` (`thinking_hub.go:44`) as the basis for a unified feed-event schema (need: id, kind, actor/operator, project, ts, title, deep-link, optional text/diff).

**For realtime-delivery:**
- Build an `ActivityHub` modeled on `ThinkingHub` (`thinking_hub.go:111`): one upstream subscription → in-process fan-out, bounded ring, per-client buffer with drop-oldest, and **backfill on Subscribe** (`thinking_hub.go:375`). Do **not** replicate the per-connection-`EventStream` model of `/agents/events` (`agents_events.go:71`) — it does not scale across tabs and has no backfill.
- Implement a **backfill story** beyond an in-memory ring: replay from JetStream `MESSAGE_EVENTS` for thinking (existing, `thinking_hub.go:210`) and from the daemon's recent-events API (`GetRecentEvents`, `client_events.go:62`) for lifecycle, so a fresh connection paints immediately and a reconnect doesn't lose the gap.
- Expose one SSE endpoint (`/feed/events`) with a companion auth-probe handler patterned on `handleAgentsEventsStatus` (`agents_events.go:29`) for OIDC/AVA-fronted EventSource. Support scope query params mirroring the existing `?project=` / `?schedule_id=` / operator filters.
- Pre-render rows server-side (one HTML row per event, swap by `data-id`) as all three current SSE pages do — no htmx-sse extension needed; clients prepend.

**For IA / UX:**
- Position the feed to **subsume the front-page "Recent activity" panel and unify it with `/active`** — both are static "what's in progress" lists today that should be one realtime chronological stream (§5).
- Reuse scope lenses from existing helpers: global (default), "my work" (`resolveDefaultOperatedBy`, `active.go:85`), per-project (`projectFromLabels`), per-bead/epic (`buildAgentEventFilter` interest sets, `agent_events.go:122`).
- Keep the `indexStatsView` banner (`server.go:1525`) / `activeSummary` (`active.go:54`) counters as a live feed header, driven by the same event stream.
- Reuse the lazy-section / load model (`user_home.go:314`) and the operator-chip + `formatAgo`/`fmtTimeAgo` row furniture so feed rows match existing visual language.
- Carry forward the `gardener_iteration_card` precedent (`agents_events.go:161`): back each feed row with a single template partial used for *both* eager render and SSE swap, so streamed and reloaded markup can never drift.