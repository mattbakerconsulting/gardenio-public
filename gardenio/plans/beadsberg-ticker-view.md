# Beadsberg Ticker View (beads-viewer)

Tracking: TBD (filed alongside this plan)
Source ask: ka-0000002912 — "I would like a 'ticker' view in beads-viewer
that lets us see two things in a 'stock ticker' type view with lots of
color and movement (green/red based on age, flashing and moving to top
when updating, etc); two views in one screen: beads (top is most
recently created by default) and events (same). Be expansive, think
'beadsberg terminal'. Find the golden shape." — Matt Baker, 2026-04-27.
Decision: ka-0000007219 chose `plan` (file fresh plan-of-record under
beads-viewer, not the deferred beads3d task ka-0000004838).

## Goal

A first-class **/ticker** page in `beads-viewer` (the live web UI under
`gardenio/controller/cmd/beads-viewer/`) that gives an operator a
two-pane "Bloomberg terminal for beads": a left rail of recently-touched
beads and a right rail of the activity firehose. Density-first, color
and motion communicate state; the page is meant to be left up on a
second monitor.

This is **not** the deprecated `beads3d/` 3D force-graph. The decision
record explicitly redirects to `beads-viewer`.

## What beads-viewer already has

- **EventStream client** (`internal/beadsapi/client_events.go`) — opens
  an SSE channel against the kbeads daemon for any NATS topic glob.
- **Bead lifecycle SSE topic glob** `beads.bead.>` is already consumed
  by `cmd/beads-viewer/agent_events.go` (per-agent filtered lane). The
  parsing shape (`agentEventSSE`, `agentEventBead`,
  `writeAgentEvent`, `agentEventKind`) is the model to reuse.
- **Other live topics** the daemon publishes (grep'd from
  `kbeads/`/`gardenio/`):
  - `beads.bead.{created,updated,closed,deleted,transferred}`
  - `beads.comment.added`
  - `beads.dependency.{added,removed}`
  - `beads.label.{added,removed}`
  - `beads.advice.{created,updated,deleted}`
  - `beads.mail.sent`
  - `beads.session.{end,handoff,pre_push}`
  - `beads.embedding.enabled`
- **Listing endpoint**: `beadsapi.ListBeadsFiltered` already supports
  `Sort:"-created_at"`, `Limit`, status/type/label filters — sufficient
  for the seed query.
- **Linkify** (`linkify.go`) wraps bare `kd-…` IDs.
- **htmx + EventSource pattern** is the existing live-update mechanism
  (server pushes pre-rendered HTML rows; client doesn't reparse JSON).

There is **no** existing `/ticker` route, no existing "events"
aggregation that combines the non-bead topics, and no existing
flash/age styling primitives.

## Design

### Route & layout

- `GET /ticker` — renders the shell: two columns, dense, dark theme.
  - Left: **Beads** rail. Header "BEADS — most recent first". Rows are
    pre-seeded from `ListBeadsFiltered({Sort:"-created_at", Limit:50})`.
  - Right: **Events** rail. Header "EVENTS — live". Empty at page load
    (events are tail-only; we don't backfill — see Open question 2).
  - Top bar: total counts (visible beads / events-this-hour), filter
    pills (`type=`, `project=`, `priority<=`), pause/resume button.
  - Footer: tiny status line — connection state + drop count.
- Add `Ticker` to the top nav next to `Active` / `Decisions` / `Agents`.

The two rails are independently scrollable but share the same row
height and column grid so the eye can sweep across.

### SSE bus

One **single** server-side SSE handler:

- `GET /ticker/stream` — subscribes once to `beads.>` (the wildcard root
  topic) so all five topic families are multiplexed over one socket.
- Per event, the handler:
  1. Decodes `evt.Event` (the topic, e.g. `beads.bead.updated`,
     `beads.comment.added`).
  2. Routes to the **beads pane** when topic ∈
     `{beads.bead.created, beads.bead.updated, beads.bead.closed,
       beads.bead.transferred}`. Renders a `tickerBeadRow`.
  3. Routes to the **events pane** for everything else
     (`comment.*`, `dependency.*`, `label.*`, `advice.*`, `mail.*`,
     `session.*`). Renders a `tickerEventRow`.
  4. Drops `beads.bead.deleted` from the beads pane (the bead is gone;
     show it as an event-pane red flash instead).
  5. Emits two SSE event names so htmx can target two swap regions:
     `event: tick-bead\ndata: <html>\n\n` and
     `event: tick-event\ndata: <html>\n\n`.

The handler **does not** filter by user — it's the firehose. Filtering
is client-side (see "Filters").

### Row shapes (server-rendered HTML)

```
tickerBeadRow:
  ┌──────────────────────────────────────────────────────────────────┐
  │ [TYPE] ka-XXXX  P0  status   Title (truncated 80c)        12s ago│
  └──────────────────────────────────────────────────────────────────┘

tickerEventRow:
  ┌──────────────────────────────────────────────────────────────────┐
  │ [KIND]  ka-XXXX  source/actor  one-line summary           4s ago │
  └──────────────────────────────────────────────────────────────────┘
```

- `[TYPE]` / `[KIND]` is a 3–4 char monospaced tag with a colored
  background (see "Color rules").
- `ka-XXXX` is a link to the bead detail page.
- `Ago` is rendered server-side at emit time and updated client-side
  every 5s (CSS animation + `data-emitted-at` attribute).

### Color rules (the "stock ticker" feel)

Two orthogonal color channels:

1. **Status / kind tag color** — semantic, fixed:
   - `created` / `comment.added` → green (#26a269)
   - `updated` / `transferred` → amber (#cd9309)
   - `closed` → blue (#1a5fb4)
   - `deleted` / mail bounce / session_end-with-error → red (#c01c28)
   - `label`, `dependency`, `advice` → grey (#5e5c64)
2. **Age halo** — temporal, fading:
   - 0–10s old: full-saturation tag color, no halo.
   - 10–60s: 50% halo around the row.
   - 60s–5m: faint halo.
   - >5m: greyscale; the row is a static historical entry.

### Flash & motion rules

- **New row**: inserted at top of its rail with a 250ms slide-down
  CSS animation. Briefly (300ms) shifts to a brighter shade of its tag
  color, then fades.
- **Update on existing bead** (when `beads.bead.updated` arrives for an
  ID already in the beads rail): the existing row is **removed and
  re-inserted at top** with a different micro-animation (a horizontal
  swipe) so the eye registers "this moved" vs "this is new".
- **Cap**: each rail keeps the most recent 200 rows in DOM; older rows
  drop off the bottom (CSS `opacity:0` then DOM removal).
- **Pause button**: stops auto-scroll-to-top and stops dropping. New
  rows queue in a small "+N hidden" badge at the top until resumed.

### Filters (client-side)

Server emits everything; the page filters in JS via `data-*` attrs on
each row:

- `data-type=`, `data-project=`, `data-priority=`, `data-actor=`.
- Filter pills toggle `display:none` on non-matching rows. The "+N
  hidden" counter shows what was filtered out.

This keeps the server simple (no per-connection filter state) and
makes filter changes instantaneous.

### "Recent beads" backfill

On page load only, the **beads** rail seeds with the last 50 beads
(`ListBeadsFiltered({Sort:"-created_at", Limit:50})`) so the page
isn't empty between events. The **events** rail starts empty.

## Code layout

```text
gardenio/controller/cmd/beads-viewer/
  ticker.go            # handler + row builders + tests
  ticker_test.go
  templates/
    ticker.html        # /ticker shell (two rails, top bar, footer)
    partials/
      ticker_bead_row.html
      ticker_event_row.html
  static/
    ticker.css         # dark theme, halo gradients, motion keyframes
    ticker.js          # client filter, pause, counter, ago-tick
```

`server.go` route registrations:

```go
mux.HandleFunc("GET /ticker", s.handleTicker)
mux.HandleFunc("GET /ticker/stream", s.handleTickerStream)
```

`handleTickerStream` mirrors `handleAgentEvents` shape but without the
agent-bead filter (`buildAgentEventFilter` is replaced by a topic-
family router).

## Phases

| Phase | Scope                                                            | Bead     |
|-------|------------------------------------------------------------------|----------|
| P1    | `/ticker` page shell, seed beads rail from `ListBeadsFiltered`,  | (TBD)    |
|       | static styling — no live updates yet.                            |          |
| P2    | `/ticker/stream` SSE endpoint with one rail (beads only),        | (TBD)    |
|       | new-row animation, age halo, no filters.                         |          |
| P3    | Add events rail (route topic families), update-vs-create motion. | (TBD)    |
| P4    | Client filters (type/project/priority), pause, "+N hidden".      | (TBD)    |
| P5    | Polish: counts in top bar, drop-counter footer, keyboard hotkeys | (TBD)    |
|       | (`/` to focus filter, `space` to pause), responsive collapse to  |          |
|       | one rail on narrow viewports.                                    |          |

P1 and P2 alone deliver the "golden shape" the operator asked for.
P3–P5 are nice-to-have iterations.

## Open questions (decisions to surface before implementation)

1. **Topic-family routing rules** — does `beads.bead.deleted` belong
   on the events rail (red flash) or get suppressed? Recommend events
   rail; opposite would lose deletions silently.
2. **Backfill on the events rail** — the listing API has no "recent
   events" view because event topics aren't persisted as beads. Two
   options: (a) leave it empty until the first event arrives, (b) seed
   from `beads.comment.added` history via comments listing. Recommend
   (a) for P1; revisit if the empty rail looks bad.
3. **Project scoping default** — should `/ticker` be all-projects by
   default, or scoped to the operator's `operated_by` project? Filter
   pills can flip it either way; default behavior matters because
   non-acme users would otherwise see acme noise. Recommend
   "all projects" for the operator-only deploy we have today; revisit
   when default-filter work in ka-0000006942 lands.
4. **Connection budget** — `beads.>` is the firehose. At fleet scale
   today (~30 active agents) volume is fine, but we should add a
   server-side rate-limit (drop with `last-N-per-second`) before this
   page goes prod. Out of scope for P1; flag in P2.
5. **Ago-tick on the client** — recompute via JS every 5s vs a single
   `setInterval` rebuilding text from `data-emitted-at`. The latter is
   cheaper at 200 rows × 2 rails. Recommend the latter.

## Non-goals

- No 3D / force-graph (that's `beads3d`, which is deprecated).
- No persistence of the firehose into a new table (live-only; the
  bead store already has the historical record).
- No per-user customization (saved filter sets, layouts) — this is a
  v1 read-only dashboard.
- No mobile layout work — desktop / second monitor only for v1.

## Links

- Source ask: `ka-0000002912`.
- Decision (resolved): `ka-0000007219` (chose `plan`).
- Adjacent (deprecated, NOT GREENLIT): `ka-0000004838` — beads3d ticker.
- Existing per-agent SSE lane (model to follow):
  `gardenio/controller/cmd/beads-viewer/agent_events.go`.
- Existing live-feed plan (similar shape): `gardenio/plans/beads-viewer-squawk-feed.md`.
