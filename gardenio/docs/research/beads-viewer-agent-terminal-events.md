# ADR — beads-viewer agent terminal: event streaming model + reconnect/replay

- **Bead:** [ka-0000004621](https://beads.gardenio.e2e.example.com/ka-0000004621)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Predecessor:** R1 architecture ADR — `gardenio/docs/research/beads-viewer-agent-terminal-architecture.md` ([ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916))
- **Status:** Proposed (research-only; no implementation until human green-lights via `gb decision`)
- **Scope:** The *event streaming model* of the agent terminal — what flows over the page, on which transports, and how the page recovers from page reload, network blip, and agent rotation. Excludes: renderer choice (R2), send-commands UX/auth (R3), iOS feature checklist (R5).

## TL;DR

> **Recommendation: PTY-only on the live transport; bead events as a parallel
> SSE feed rendered into a separate sidecar lane in the same view.** The PTY
> lane uses the existing direct browser → coopmux WebSocket
> (`wss://coopmux.<host>/ws/{session_id}?subscribe=pty,screen,state`) chosen
> in R1; the bead-events lane uses a new beads-viewer SSE endpoint
> (`/agents/{id}/events`) that proxies the daemon's existing SSE stream
> (`beadsapi.Client.EventStream`) filtered to `beads.bead.>` plus
> `decisions.>` and `mail.>` for the bead's `id` and any descendants.
> Reconnect is anchored on **byte offset for PTY** (coopmux's
> `RingBuffer.total_written` is a monotonic per-session counter, exposed via
> `GET /api/v1/sessions/{id}/output?offset=`) and **wall-clock
> "since" cursor for bead events** (the SSE channel re-subscribes and the
> page issues a `?since=<rfc3339>` REST catchup against the daemon for any
> missed events). Agent rotation is detected by the bead-events lane —
> `bead.updated` carrying a new `last_session_id` field — and triggers a
> WS swap on the PTY lane without a page reload. The page renders into two
> independent React-free DOM regions; htmx swaps drive the bead-events
> lane, JS owns the PTY lane.

The trade-off: **two transports, two clocks, two reconnect strategies** —
but they are *interleaved by the user, not by the wire*. We get
iOS-parity in the visible layout (one pane for the agent's full picture)
without paying the engineering cost of a new multiplex protocol or any
coopmux change. The cost we accept is one more long-lived stream from the
browser (coopmux WS + beads SSE) and one new beads-viewer route.

## Context

### Goal

The agent-terminal view in `gardenio/controller/cmd/beads-viewer/` (Go htmx)
must render an agent bead as a fully-fledged terminal — the iOS-parity
target from the parent epic. R1 settled *how* the browser reaches coopmux
(hybrid: direct WS for stream, htmx-via-Go for control). This ADR settles
*what is on the wire* — specifically:

1. **What flows on the live channel** — PTY bytes only, or PTY interleaved
   with bead events (comments, state transitions, decisions, mail)?
2. **How the page survives interruptions** — page reload, network blip,
   agent rotation (the agent bead carries forward, but the coopmux
   `last_session_id` it points at can change).
3. **How bursty PTY output is throttled** so the page does not freeze under
   a busy agent.
4. **Whether mobile and desktop need different streaming contracts.**

### What already exists (do not invent)

**coopmux** ([R1 §"What already exists"]):

- `GET /ws/{session_id}?subscribe=pty,screen,state` — JSON-text WS frames.
  `SubscriptionFlags::parse` accepts `pty`/`output`, `screen`, `state`,
  `replay` (`coop/crates/mux/src/upstream/bridge.rs:50-67`).
- `GET /api/v1/sessions/{id}/output?offset=&limit=` — raw bytes from the
  PTY ring. Backed by a global-offset ring buffer
  (`coop/crates/cli/src/ring.rs`): `total_written` is monotonic per
  session; `read_from(offset)` returns `None` if the offset is older than
  `oldest_offset()` (i.e. has been overwritten). This is the canonical
  resume primitive for PTY — *if* the offset is still in the ring.
- `GET /api/v1/sessions/{id}/screen` — current rendered screen (text +
  ANSI). Cold-start fallback when offset-based resume is impossible.
- `GET /api/v1/sessions/{id}/transcripts/catchup?since=<seq>` — structured
  turn-level history. Useful for very long backfills or coarse "page
  reload after lunch" semantics, not for in-stream recovery.

**beads-viewer / beadsapi** has a generic SSE client over the daemon:

- `beadsapi.Client.EventStream(ctx, topics)` (`gardenio/controller/internal/beadsapi/client_events.go`)
  opens `GET /v1/events/stream?topics=<filter>` and returns a channel of
  `SSEEvent{Event, Data}`. Already used in production by
  `bridge/api_decisions_sse.go` and `bridge/api_escalations_sse.go` with
  topic filters like `beads.bead.created,beads.bead.closed,beads.bead.updated`.
- The daemon's SSE endpoint accepts NATS-style topic filters. There is no
  per-bead push out of the box; we filter on the client side or set a
  topic narrow enough to be acceptable (see below).

**Agent identity vs session identity.** The agent bead is the stable
identity; its `last_session_id` field points at the *current* coopmux
session (`gardenio/controller/internal/bridge/bot_agent_kill.go:201-206`).
On rotation (kill + respawn), the bead is re-assigned to a new pod and
`last_session_id` is updated. The viewer must therefore follow the bead,
not pin to a session_id at page load.

### What is *not* in scope here

- **Renderer choice (R2).** This ADR talks about *frames on the wire* and
  the DOM regions they target, not how those frames become pixels. The
  decisions here are renderer-agnostic: xterm.js, `<pre>` re-render, or
  htmx fragment swaps would all consume the same WS+SSE contract.
- **Authorization to *send* (R3).** All decisions here are read-mostly.
  Write verbs flow through beads-viewer's Go server (R1) and inherit the
  R3 policy gate.
- **iOS feature parity matrix (R5).** This ADR identifies the data needed
  to *support* parity (PTY + bead events available in one view). R5 picks
  which parity surfaces actually ship.
- **Coopmux protocol changes.** Per parent-epic anti-scope, this ADR does
  not propose new coopmux endpoints or new WS message types. Every
  primitive used below already exists.

## Options for the streaming model

The decision tree fans out on whether bead events ride the same transport
as PTY bytes, and where the multiplex (if any) is built.

### Option A — PTY-only stream; bead events via separate beads-viewer SSE (recommended)

Two independent live channels reach the page:

- **PTY lane.** Browser → `wss://coopmux.<host>/ws/{session_id}` direct
  (R1's stream leg). Subscription string: `pty,screen,state`. Frames:
  - `{"type":"pty","seq":<u64>,"data":"<base64-or-utf8>"}` (existing
    coopmux contract; the data field is the same byte slice the ring
    buffer accepts at offset).
  - `{"type":"screen", ...}` (rendered screen snapshot, occasional).
  - `{"type":"state", ...}` (driver state transitions: idle/working/...).
- **Events lane.** Browser → `https://beads-viewer.<host>/agents/{id}/events`
  is a new beads-viewer SSE endpoint. The Go handler proxies a filtered
  view of the daemon SSE stream:
  - `EventStream(ctx, "beads.bead.>,decisions.>,mail.>")`
  - filtered server-side to events whose `bead_id` matches the agent
    bead, the agent's parent task, or a child of either (so a comment on
    the work bead the agent is driving still surfaces).
  - re-emitted to the browser as SSE with `event: <kind>` and a JSON
    payload that includes the original NATS subject as `topic`.

Reconnect is per-lane, with different anchors (see "Reconnect/replay"
below). Rendering is into two visually-adjacent DOM regions but they are
independently scrolled and independently resumed.

**Pros**

- **No coopmux change.** Every coopmux primitive already exists.
- **Clean reconnect.** Each lane has its own native cursor: PTY uses byte
  offset (which coopmux already exposes); events use wall-clock since.
  No need to invent a unified clock.
- **Failure isolation.** A flaky beads daemon does not freeze the
  terminal; a chatty PTY does not throttle bead-event delivery.
- **htmx-friendly.** The events lane can render to htmx `hx-swap-oob`
  fragments — every bead-event handler is already structured this way in
  beads-viewer. The PTY lane is JS, but it is the *only* JS we need.
- **Simpler authorization.** R3 will gate the *write* surface in
  beads-viewer; reads on both lanes remain behind the existing
  ForwardAuth + OIDC verifier (R1 §"Auth token model").

**Cons**

- **Two clocks.** A bead comment that arrives via SSE is not strictly
  ordered against PTY bytes from the same wall-clock instant. The user
  sees a "comment landed" indicator that may render slightly before or
  after the corresponding agent stdout. We accept this — the events lane
  is *not* attempting to be a transcript of the agent's reasoning; it is
  a sidebar of what the *system* observed about the agent.
- **One more long-lived connection per page.** A SSE plus a WS. The SSE
  is cheap (server-side already implemented; Go HTTP/1.1 with
  `flusher.Flush()` is fine), but it is a new resource against
  beads-viewer.
- **Agent rotation requires page-side coordination across lanes.** The
  events lane sees the rotation first (`bead.updated` with a new
  `last_session_id`); JS must close+reopen the PTY WS to the new session
  ID. This is cleanly a one-way notification (events → PTY swap), not a
  shared state machine.

### Option B — Multiplexed single WS (PTY + events on one socket)

Extend coopmux's WS contract to also carry bead-events, or have
beads-viewer terminate the browser WS and fan in coopmux PTY +
beadsapi SSE on the back end.

**Pros**

- One reconnect strategy. One backpressure strategy. One DOM resume
  story.
- Strict ordering between bead events and PTY bytes if the merge happens
  at the source.

**Cons**

- **Either coopmux changes (anti-scope), or beads-viewer becomes a Go WS
  proxy (R1 §"Option B" — explicitly rejected).** Both are exactly the
  doors R1 closed.
- **Cross-clock ordering is a lie anyway.** The "strict ordering" is only
  strict at the merge node; the original events are still on different
  clocks (PTY is a per-session monotonic counter, beads is a postgres
  TIMESTAMPTZ). Pretending we have a single clock costs work and gives
  little.
- **Replay becomes coupled.** A page reload now needs a single resume
  cursor that can address both lanes. There is no good single primitive
  — byte offset does not work for bead events; "since" timestamp loses
  PTY data that was overwritten in the ring.

### Option C — PTY-only, bead events deferred to a separate "Activity" tab

Punt event interleaving entirely. Render PTY in the agent view; require
the user to open the bead-detail tab to see comments, state, mail.

**Pros**

- Trivial to ship. Smallest possible change.

**Cons**

- **Misses the parent epic's iOS-parity goal.** The Slack thread's framing
  is "an agent bead should be a fully fledged terminal much like the iOS
  app" — and the iOS app surfaces bead activity inline. Splitting it
  across tabs is a regression.
- **Still has to solve the rotation problem.** Even PTY-only must follow
  agent identity through `last_session_id` rotation, which requires *some*
  bead-event subscription on the page. Once we're paying for the SSE we
  may as well surface its content.

### Option D — Render everything as bead events; agent stdout becomes a streaming bead comment

Push PTY bytes into beads as comments on the agent bead and consume one
SSE stream. (The Slack bridge does something analogous —
`bridge/agentstream.go` already turns agent state into Slack messages.)

**Pros**

- Single transport. Single audit trail. Server-side persistence of the
  full terminal in postgres.

**Cons**

- **Storage and event-bus blast radius.** A busy agent emits hundreds of
  KB/s of PTY. Routing that through beads = a write per chunk = postgres
  write amplification + NATS fanout to every subscriber including the
  Slack bridge, which would melt.
- **Already covered by coopmux.** The coopmux ring buffer + recording
  already persist the PTY stream. Re-storing it in beads is duplication.
- **Loses ANSI fidelity.** Bead comments are markdown-rendered; agent
  output uses raw ANSI escapes that the renderer must consume directly.
- Out of scope per the parent epic anti-scope ("Do not duplicate Slack
  agentstream logic").

## Decision

**Adopt Option A: PTY-only on the live channel; bead events as a parallel
beads-viewer SSE feed.**

Three load-bearing claims:

1. **The reconnect/replay primitives we have are heterogeneous; pretending
   otherwise costs more than rendering them in two adjacent DOM regions.**
   coopmux's PTY ring is byte-offset addressable; beads SSE is wall-clock
   addressable with a daemon-side filter. Both are battle-tested. Joining
   them on the wire would mean inventing a third primitive and getting
   replay correctness right twice.
2. **Failure isolation matters more than ordering fidelity.** A user
   watching an agent through the viewer cares that *both* signals keep
   flowing under partial failure. Two transports degrade independently;
   one transport degrades together.
3. **htmx is the existing idiom for everything else in beads-viewer.**
   Bead events have a natural htmx representation already used elsewhere
   (decisions, mail, comments). Forcing them through a JS-managed merged
   WS just to share a connection with PTY is a step away from the
   prevailing idiom for no real win.

If a future requirement materializes for *strict* ordering between agent
PTY and bead events (e.g., audit replay where `bead.closed` must
provably land after the last PTY byte), this ADR should be revisited
under the lens of Option B.

## Reconnect/replay state machine

The page maintains two independent state machines, one per lane. Both
share an outer "agent identity" cursor (the bead ID), which is what the
URL pins.

### Outer machine — agent identity

```
                ┌─────────────┐
                │  PageMount  │
                └──────┬──────┘
                       │  (URL = /agents/{bead_id})
                       │  fetch initial bead snapshot
                       │  read bead.last_session_id  → S0
                       ▼
   ┌────────►  ┌─────────────────────────┐
   │           │  TrackingSession(S0)    │
   │           │  (PTY lane → S0,         │
   │           │   Events lane filtered   │
   │           │   to bead_id = agent)    │
   │           └─────────────┬───────────┘
   │                         │
   │  events lane sees       │
   │  bead.updated{          │
   │    last_session_id=S1}  │
   │                         ▼
   │           ┌─────────────────────────┐
   │           │  RotateToSession(S1)    │
   │           │  PTY lane: WS close →   │
   │           │  reopen at S1 (cold)    │
   │           │  Events lane: unchanged │
   │           └─────────────┬───────────┘
   │                         │
   └─────────────────────────┘
```

Page reload restarts the machine; URL → bead → fresh `last_session_id` →
fresh WS. Crucially: **the page does not store session_id in the URL**.
A user sharing `/agents/{bead_id}` mid-rotation hands the recipient the
agent, not a stale session.

### PTY lane — byte-offset reconnect

```
                ┌────────────┐
                │   Open     │
                └─────┬──────┘
                      │
                      │  GET /api/v1/sessions/{id}/screen      (cold init)
                      │  → seed renderer with current screen
                      │  GET /api/v1/sessions/{id}/output?offset=0&limit=N
                      │  → backfill last N bytes for scrollback
                      │  open WS, remember `seq` of every frame
                      ▼
                ┌────────────┐
                │ Streaming  │◄────── WS frames append to renderer
                └─────┬──────┘        & advance localOffset = seq
                      │
                      │  WS error / close (browser reports "lost")
                      ▼
                ┌────────────┐
                │ Reconnect  │
                └─────┬──────┘
                      │
                      │  if localOffset known:
                      │      GET /output?offset=localOffset
                      │      ─→ 200 with bytes:    catchup branch
                      │      ─→ 200 with empty:    no gap (resume clean)
                      │      ─→ requested-offset-overwritten:
                      │              GET /screen + /output?offset=<oldest>
                      │              ─→ "ring overflowed" cold reseed
                      │  reopen WS
                      ▼
                ┌────────────┐
                │ Streaming  │
                └────────────┘
```

The contract relies on coopmux's `RingBuffer::total_written` being a
*monotonic per-session* byte counter (verified in `ring.rs:7,42-86`).
Crucially, it is per-session — across rotation the counter restarts, so
the outer state machine (above) zeroes `localOffset` on `RotateToSession`.

### Events lane — wall-clock since cursor

```
                ┌────────────┐
                │   Open     │
                └─────┬──────┘
                      │
                      │  GET /agents/{id}/events?since=<page-mount-rfc3339>
                      │  ─→ catchup REST (filtered SSE replay)
                      │  remember lastSeenAt = max(event.ts)
                      ▼
                ┌────────────┐
                │ Streaming  │◄────── SSE frames; update lastSeenAt
                └─────┬──────┘
                      │
                      │  EventSource error (network blip / server hangup)
                      ▼
                ┌────────────┐
                │ Reconnect  │
                └─────┬──────┘
                      │
                      │  GET /agents/{id}/events?since=lastSeenAt
                      │  (browser-built EventSource with last-event-id
                      │   header is fine if we use SSE id: lines, but
                      │   `since` query is safer because daemon SSE
                      │   already speaks topic filters and we control
                      │   the proxy)
                      ▼
                ┌────────────┐
                │ Streaming  │
                └────────────┘
```

Bead events are durable in postgres; even a multi-minute disconnect
recovers cleanly via `since`. The proxy implementation in beads-viewer
wraps two daemon calls back-to-back: a one-shot REST query (or an SSE
catchup endpoint, whichever the daemon offers) followed by the live
`EventStream` channel — same pattern as
`bridge/api_decisions_sse.go`.

### Page reload (cold start)

A reload is just outer-machine re-entry: every cursor resets, PTY does a
`/screen` + `/output?offset=0&limit=N` cold seed (where `N` is the
"scrollback budget" — see "Render budget" below), events lane fetches
since page-mount time. Total cost on cold start: 3 short HTTP roundtrips
(bead detail, screen, output backfill) + 2 long-lived stream opens.

### Network blip

The two lanes notice independently. The browser's WS `onclose` and SSE
`onerror` both fire; each lane does its own reconnect with its own
cursor; neither waits on the other. The user-visible signal is a brief
"reconnecting…" pill in each lane's header. We do not blank the
renderer on disconnect — the existing screen stays painted, and frames
resume on top of it.

### Agent rotation

This is the only cross-lane coordination. The events lane sees a
`bead.updated` event whose payload carries `fields.last_session_id` ≠ the
session_id the PTY lane is currently subscribed to. The lane fires a
`session_changed` event into a tiny in-page bus; the PTY lane's reducer
matches on it, closes its WS cleanly, drops `localOffset`, and reopens
against the new session_id following the same cold-start path as page
mount (screen seed + WS open). The events lane itself does not change
subscription — it filters by *bead_id*, which is invariant under rotation.

Edge case: rotation happens while the events lane is in `Reconnect`. We
queue `session_changed` until the events lane returns to `Streaming` (or
times out and surfaces an explicit error). If the user navigates away
during a queued rotation, no harm done — page unmount tears down both
lanes.

## Backpressure and render budget

A "busy" agent (compiler output, test runner, tail of a verbose log)
emits hundreds of KB/s of PTY. The DOM cannot absorb that at frame rate
without freezing.

The proposal is **renderer-agnostic budget enforcement at the JS edge**,
not at the wire:

- The WS path delivers frames as fast as coopmux sends them; no
  back-channel throttle. (Adding a throttle here would mean more
  coopmux-side state to manage, which we are explicitly not doing.)
- The page maintains a small in-memory queue (a few MB high-watermark)
  of un-rendered bytes per session. The renderer (R2 will pick the
  technology) drains the queue on `requestAnimationFrame`, with a per-
  frame cap (e.g., 64 KB or "all bytes accumulated since the last
  frame", whichever is smaller). Excess bytes stay in the queue.
- If the queue exceeds high watermark for more than a configurable window
  (proposal: 2 s / 8 MB), the page enters **fast-forward mode**: it
  drops the queue, fetches a fresh `/screen` snapshot, and applies it
  whole. The user sees a brief "catching up…" pill and a screen jump,
  not a frozen tab. ANSI scrollback for the dropped window is
  recoverable via `/output?offset=...` if the user explicitly scrolls up.

This keeps three properties:

1. **Steady-state liveness.** Under normal load (a few KB/s — typical for
   a Claude session) nothing drops; every byte renders in real time.
2. **No browser freeze.** Under burst load the queue absorbs the pressure;
   the renderer's per-frame budget caps DOM work; if the burst is
   sustained the screen-snapshot fallback collapses the queue.
3. **No coopmux change.** All adaptation is on the consumer side. coopmux
   continues to fan out at line rate.

The bead-events lane has no backpressure problem in practice: bead-event
rates are O(events/min), not O(KB/s), and even pathological cases (a
spammy `comment.created` storm) are bounded by daemon write throughput.
We render those as they arrive.

### Why not WS-level flow control?

WebSockets do not expose per-message flow control to the application
layer in browsers. We *could* close-and-reopen as a coarse pause/resume,
but that round-trips through coopmux's session lookup and forfeits the
existing `WsBridge` reuse logic. The renderer-side queue is the right
layer.

## Mobile vs desktop differences

Mobile is a real concern: a phone in a pocket may suspend the WS
silently, the screen is small, and JavaScript heap is tighter. The
recommendation is **same protocol, different defaults**:

| Concern | Desktop default | Mobile default | Why |
|---|---|---|---|
| Initial scrollback fetch | last 256 KB of PTY | last 32 KB | Lower memory ceiling; mobile users typically scroll less of an agent terminal |
| Renderer queue high-watermark | 8 MB / 2 s | 1 MB / 1 s | Smaller DOM budget; faster collapse to screen snapshot |
| WS visibility heuristic | none | on `document.visibilitychange` → "hidden" hold WS open with `Page Visibility API`; on resume from background after >30 s, force a `Reconnect` cycle (the OS likely killed the socket without telling us) | iOS Safari aggressively suspends background tabs; we lose frames silently |
| Events lane rendering | inline, expandable | collapsed by default; tap-to-expand | Vertical real estate |
| Send-commands UI | full keyboard | virtual key bar (Esc, Tab, Ctrl, ↑↓) — but this is R3 territory | (R3 will spec) |

Mobile detection is a single `matchMedia('(pointer: coarse)')` check; we
do *not* sniff user-agent. The protocol is identical between the two —
a desktop with bad network behaves like mobile, and vice versa.

The above are *defaults*, not hard limits. A user-toggleable
"high-fidelity / low-fidelity" pill in the agent view header lets a
mobile user opt back into desktop budgets if they're on Wi-Fi and
willing to spend the battery.

## Interleaving decision (PTY-only vs PTY+events)

**Decision: PTY-only on the live transport; bead events as a parallel
SSE feed rendered into a *separate visual lane in the same view*.**

This is the same shape as a chat app where the user's own typing flows
into the message composer while incoming server-pushed reactions update
the message list independently. The user perceives a unified picture
without us having to merge the streams on the wire.

Concretely, the agent-view layout (R2 will detail the markup) is
expected to look roughly like:

```
┌────────────────────────────────────────────────────────────────┐
│ <bead title>                       [agent: <id>] [session: SX] │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  PTY render region                                             │
│  (xterm.js / <pre> / fragment swap — R2 picks)                 │
│                                                                │
│  ──────────── inline events overlay (timestamped pills) ───────│
│                                                                │
├────────────────────────────────────────────────────────────────┤
│  Send-commands form (R3-gated)                                 │
└────────────────────────────────────────────────────────────────┘
```

Bead events render as *time-pinned pills overlaid on the PTY scroll
position* at the wall-clock minute they arrived, not interleaved into
the byte stream. The pill carries:

- icon (comment / state-change / mail / decision-resolved / nudge)
- one-line summary
- relative time (`2 min ago`)
- click-through to the bead detail (existing route).

This achieves "full agent picture in one pane" (parent epic goal) while
keeping the streams clock-independent.

Rationale recap (from "Decision" above): heterogeneous reconnect
primitives, failure isolation, htmx-native control plane.

## Open questions (downstream)

| ID | Question | Affects |
|---|---|---|
| OQ-1 | Does the daemon SSE endpoint support a `since=<rfc3339>` catchup parameter, or do we layer one in beads-viewer using `bead.list?updated_since=`? Either works; we pick whichever is already in `kbeads/`. | events lane reconnect |
| OQ-2 | What is the expected byte-rate ceiling for a single coopmux session in production? If realistic peaks are <100 KB/s, the renderer-queue defaults can be tightened (smaller queue, never fast-forward). | render budget |
| OQ-3 | The PTY frame format on the WS — `{"type":"pty","seq":...,"data":...}` — is it stable enough to depend on across coopmux versions? `WsBridge` is the source of truth; do we want a frame-version handshake? (Likely not for v1; document the assumption and pin via integration test in R6.) | renderer (R2) |
| OQ-4 | Does the agent bead's `bead.updated` event always carry the changed `fields.last_session_id`, or only the bead diff? If diff-only, the events lane needs to refetch the bead on every `bead.updated` — adds latency to rotation handling. | rotation detection |
| OQ-5 | Should the events lane scope include the agent bead's *parent* (e.g. comments on the work bead the agent is driving)? Captured in the recommendation as a yes-by-default; R5 (iOS parity) should confirm against the iOS app's behavior. | events filter |
| OQ-6 | Mobile background-tab behavior: do we want a smart "session paused" UX when the page is hidden for >30 s, or always force a hard reconnect on visibility resume? | mobile UX |
| OQ-7 | Multi-tab semantics: if one user opens the same agent in two tabs, both will hold a WS to coopmux. The existing `WsBridge` reuse logic is per-client, so this is fine for coopmux; but our renderer queue is per-tab. Document and accept. | multi-tab |

## Acceptance for this ADR

- [x] Streaming model recommendation + rationale (Option A: PTY-only +
      parallel SSE bead-events lane).
- [x] Reconnect/replay state machine sketched (outer agent-identity
      machine + per-lane PTY/events machines, with explicit handling for
      page reload, network blip, and agent rotation).
- [x] Interleaving decision (PTY-only on the wire; visually-adjacent
      lanes in the page) with rationale.
- [x] Backpressure & render-budget strategy (renderer-queue with
      fast-forward fallback to `/screen`; no coopmux change).
- [x] Mobile vs desktop differences (same protocol, tighter defaults +
      visibility heuristics).
- [x] No Go / template / JS / coopmux code changes proposed in this ADR.
- [x] Open questions filed for downstream tasks (R2, R5, R6) and
      verification points before implementation.
