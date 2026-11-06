# ADR — beads-viewer agent terminal: renderer choice

- **Bead:** [ka-0000003333](https://beads.gardenio.e2e.example.com/ka-0000003333)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Predecessor:** [ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916) — R1 architecture ADR (`docs/research/beads-viewer-agent-terminal-architecture.md`)
- **Status:** Proposed (research-only; no implementation until human green-lights via `gb decision`)
- **Scope:** How the agent-detail page in `gardenio/controller/cmd/beads-viewer/` *renders* the live PTY stream + screen snapshots. Excludes: send-commands UX (R3), event interleaving with bead events (R4), iOS feature-parity matrix (R5), implementation plan (R6).

## TL;DR

> **Recommendation: xterm.js, lazy-loaded only on `/agents/{id}`.** Plain `<pre>`
> of the coopmux `/screen` snapshot is shipped *alongside* xterm.js as the
> `<noscript>` / read-only fallback (it is what htmx renders before the WS
> attaches). SSE→HTML fragment swap is rejected as the primary path because it
> requires re-implementing an ANSI-screen state machine in Go just to feed an
> htmx swap, when the data plane is already raw PTY bytes that a terminal
> emulator handles natively.

The decision rests on three claims, in priority order:

1. **The R1 data plane is raw PTY bytes over WS.** The architecture chose
   direct browser → `wss://coopmux.<host>/ws/{session_id}`. The WS frames are
   PTY bytes including ANSI CSI sequences (cursor moves, colors, alternate
   screen). The renderer either parses them (xterm.js) or throws them away
   (`<pre>`) or re-implements an ANSI parser server-side (SSE+fragment).
   xterm.js parses what is already on the wire.
2. **The iOS user-visible parity bar is *event-shaped*, not terminal-shaped.**
   The iOS Gardenio app renders an activity feed of structured bead events, not
   a PTY emulator (see "iOS parity bar"). beads-viewer can therefore meet iOS
   parity with the *event* surface (R4) and exceed it cheaply with a real
   terminal — every additional fidelity point xterm.js gives us (true scrollback,
   selection, copy/paste, alt-screen, OSC 52) is parity-positive, not
   parity-negative.
3. **xterm.js fits the bundle budget when lazy-loaded.** Core + FitAddon +
   WebLinksAddon is ~330 KB minified / ~100 KB gzipped, loaded only on the
   agent-detail route. The base beads-viewer page (htmx + a few KB of bespoke
   JS) is unaffected.

## Context

R1 settled the network architecture: the agent-detail page is served by
beads-viewer (Go htmx), opens a WebSocket *directly* to `coopmux.<host>/ws/{session_id}`
for high-volume PTY fan-out, and uses htmx for control-plane verbs
(`/screen` snapshot, `/input`, `/agent/nudge`, `/agent/respond`) proxied through
beads-viewer's Go server. R2 must decide what JavaScript and HTML the
agent-detail page actually contains to *display* the stream.

The renderer choice is constrained by what the wire carries:

- **WS stream (`/ws/{session_id}`)** — Text frames of raw PTY bytes. ANSI
  escapes, cursor positioning, alt-screen, color, OSC sequences all flow as-is
  (`coop/crates/mux/src/transport/ws.rs`).
- **Screen snapshot (`GET /api/v1/sessions/{id}/screen`)** — already-rendered
  text + ANSI screen state, returned as JSON.
- **Output ring (`GET /api/v1/sessions/{id}/output?offset=&limit=`)** — raw PTY
  bytes; used for catchup after a network blip or page reload (R4).

Per R1's anti-scope, this ADR cannot ask coopmux to change its frame format.

### Anti-scope

- No Go / template / JS code changes (sketches only).
- No coopmux API changes.
- No decision on event interleaving (R4) — but the renderer must not paint
  R4 into a corner.
- No write-authorization decision (R3) — but the renderer must support a
  read-only mode without a full rewrite.

## Options considered

### Option A — xterm.js (recommended)

The browser embeds [xterm.js](https://xtermjs.org/) v5 in the
`/agents/{id}` page. On mount, htmx renders the wrapper (header card, controls,
empty terminal `<div id="term">`). Inline JS attaches a `Terminal` instance to
that div, opens the WS to `wss://coopmux.<host>/ws/{session_id}`, and pipes
incoming Text frames through `term.write(...)`. The htmx-rendered initial
`<pre>` of the `/screen` snapshot is rendered *first*, so users without JS
(or with JS still loading) see the current screen state in plain HTML; the
xterm instance replaces it on attach.

**Pros**

- Full ANSI / VT220 fidelity with no Go code: cursor moves, colors, bold,
  alt-screen (vim, less), OSC 52 clipboard, mouse reporting (if we ever want
  it), bracketed paste.
- True scrollback (default 1000 lines, configurable). PTY-mode line discipline
  is correct; users can scroll back through tool output without server
  pagination.
- Browser-native copy/paste (`Cmd/Ctrl-C`, right-click, double-click word
  selection) works without our doing any JS.
- Active upstream (Microsoft VS Code uses xterm.js); pinned versions, no
  bespoke maintenance burden.
- Lazy-loadable: `<script defer src="/static/xterm.bundle.js">` only on
  `/agents/{id}`; the rest of beads-viewer is unaffected.

**Cons**

- Adds a JS dependency to the agent-detail route (~100 KB gzipped). The rest
  of beads-viewer is htmx-first; this route is the first one with a "real" JS
  component. Mitigation: lazy-load and cache aggressively (immutable static
  asset, content-hashed filename).
- htmx-hostile *for the terminal area itself*: htmx swaps DOM, xterm.js owns
  a canvas. We cannot use htmx attributes to update the terminal contents —
  but we don't need to (the WS does it). Control verbs (input, nudge, respond)
  remain pure htmx because they target sibling elements (status pill, toast
  region), not the terminal canvas.
- Accessibility on a canvas-rendered terminal is weaker than on plain HTML.
  xterm.js exposes a screen-reader mode (`screenReaderMode: true`) that maps
  cells to an ARIA live region. We must turn it on; default-off is the wrong
  default for our user base.

**Bundle budget (sketch).** Numbers are pessimistic ceilings — pin to actuals
in R6 by running `vite build` against a minimal entrypoint:

| Bundle | Min | Gzip | Notes |
|---|---:|---:|---|
| xterm core | 250 KB | 80 KB | The terminal itself. |
| `xterm-addon-fit` | 5 KB | 2 KB | Resizes terminal to viewport. |
| `xterm-addon-web-links` | 5 KB | 2 KB | Click-through URLs in scrollback. |
| Glue (WS open, snapshot replace, reconnect) | ~5 KB | 2 KB | Hand-written. |
| **Total** | **~265 KB** | **~86 KB** | Cap budget at **125 KB gzipped** for v1. |

Cap **125 KB gzipped** as the soft budget for the agent-detail-route bundle
(includes xterm + addons + glue). If a future addon (e.g. `xterm-addon-search`,
`xterm-addon-canvas` for perf) pushes us over, raise the budget explicitly in
the implementation MR; do not silently grow.

### Option B — Plain `<pre>` re-render via htmx swap of `/screen`

The page contains `<pre id="term" hx-get="/agents/{id}/screen" hx-trigger="every 500ms" hx-swap="innerHTML">`.
beads-viewer proxies `/screen` from coopmux, returns the rendered screen text
as an HTML fragment (with optional `<span>`-coloring derived from ANSI), and
htmx swaps the `<pre>` every half-second. No WS. No client JS beyond htmx
itself.

**Pros**

- Zero JS beyond htmx. Smallest possible client surface; no canvas, no second
  transport.
- Composes naturally with the rest of beads-viewer's htmx-native control plane.
- Trivially accessible: a `<pre>` is plain text in the DOM, screen readers
  handle it without configuration.
- Easiest mobile story: a `<pre>` is just text; no canvas re-paint cost, no
  IME quirks.

**Cons**

- **Throws away the R1 WS architecture.** R1 chose direct browser→coopmux WS
  *because* PTY fan-out is high-volume; if the renderer never opens the WS,
  the WS leg is dead weight. We could keep the WS wiring for R3 input
  (`POST /input` is HTTP, not WS, so this is moot), but then R4's "interleave
  bead events" path needs a separate stream anyway. This option implicitly
  redoes R1.
- **Polling at 500 ms is the wrong knob.** Too fast and we hammer coopmux;
  too slow and the user sees a stale screen. The WS is the right primitive
  for live output; throwing it away to use HTTP polling is an architecture
  regression.
- **No scrollback.** `/screen` returns the *current* screen state — a
  terminal frame buffer, typically 24×80 or whatever the PTY thinks the size
  is. Anything that scrolled off the top is gone. Scrollback would require
  the server to also stream `/output` and the page to maintain a buffer,
  which is xterm.js's job.
- **No selection / copy across re-renders.** Every htmx swap blows away the
  user's text selection. Copy/paste in mid-scroll is a constant fight with
  the swap clock.
- **ANSI fidelity ceiling is "color, maybe."** Cursor positioning, alt-screen
  (vim), OSC 52 clipboard, bracketed paste, mouse reporting all degrade or
  break. For an agent that runs Claude Code (which uses alt-screen for its
  TUI) this is user-visibly broken.

**Verdict.** Acceptable as the *snapshot* leg / no-JS fallback (we render the
initial `<pre>` of `/screen` server-side so the page is meaningful before JS
attaches). Insufficient as the primary live renderer.

### Option C — SSE → HTML fragment swap

beads-viewer opens an SSE channel to the browser, internally consuming the
coopmux WS server-side, parsing the ANSI stream into per-line HTML fragments
on the Go side, and pushing each line as an `<event: row>` SSE message. The
page uses htmx's SSE extension (`hx-ext="sse"`) to swap a row template into
the terminal area as each event arrives.

**Pros**

- htmx-native end to end. The renderer is "swap a `<div>` per row" — no
  canvas, no second transport visible to the page, no bespoke JS module.
- Server controls fidelity: we can choose how much ANSI to honor (color, bold,
  underline) and serialize the rest as text.
- One transport in the page (SSE over HTTPS to beads-viewer), so cookie scope
  for `coopmux.<host>` is no longer load-bearing for the live data plane.
- Authorization for the read leg becomes a beads-viewer concern again, which
  pairs nicely with the R3 write gate.

**Cons**

- **Re-implements an ANSI screen state machine in Go.** A naive line-shaped
  parser handles SGR (color/bold), but cursor moves, alt-screen, line-erase,
  cursor-save, and bracketed paste need a full state machine. Existing Go
  ANSI libraries (e.g. `ansi`, `vt10x`, `tcell`) handle pieces; integrating
  one is a real chunk of work and a new failure mode (parser bugs become user
  bugs). xterm.js is *the* widely-used implementation; we would essentially
  ship a worse copy of it on the server.
- **Reverses R1's data-plane direction.** R1 explicitly avoided putting Go in
  the live data path because it adds a byte-shovel between two pods and
  re-introduces reconnect-storm coupling. Switching the renderer to SSE
  re-introduces those costs (steady-state PTY bytes flowing through
  beads-viewer, every browser reconnect a fresh upstream WS connect).
- **Scrollback still belongs to the server.** Per-row swaps need the server
  to remember enough rows to handle reconnect/replay. Either we cap scrollback
  at the browser (in which case xterm.js does it better) or we keep state
  per-session in beads-viewer (a new ring buffer the parent epic's anti-scope
  forbids).
- **alt-screen kills row-shaped rendering.** A TUI that uses alt-screen
  (vim, less, htop, Claude Code's input box) doesn't append rows — it
  rewrites the whole screen in place. The "stream HTML rows" mental model
  breaks. The fallback is "render the alt-screen as a single replaced
  fragment," at which point we are doing terminal emulation in Go.

**Verdict.** Architecturally clean *if* we were free to redesign the data
plane. Given R1, this option pays the cost of a Go ANSI parser and a Go
byte-shovel without buying anything xterm.js doesn't already give us.

### Option matrix

| Concern | A. xterm.js | B. `<pre>` of `/screen` | C. SSE → HTML rows |
|---|---|---|---|
| Bundle weight (gzip, agent route only) | ~86 KB | 0 KB | ~5 KB (htmx-sse ext) |
| ANSI fidelity (color, bold, underline) | full | partial (server-coloring) | partial (server-coloring) |
| ANSI cursor / alt-screen / bracketed paste | full | none | requires full Go ANSI VM |
| Scrollback depth | configurable, 1000 default | none (only current screen) | server-capped |
| Copy / paste (selection survives updates) | native, robust | breaks on every swap | breaks on every row swap |
| OSC 52 clipboard hooks | yes | no | no |
| Accessibility (screen readers) | needs `screenReaderMode: true` | best-by-default (`<pre>` is plain text) | good (DOM rows) |
| Mobile viewport | works (FitAddon); IME quirks | trivial | works |
| Composes with R1 (direct WS) | yes | rejects WS | rejects WS, adds Go byte-shovel |
| Server complexity added | none | trivial proxy of `/screen` | full ANSI state machine in Go |
| Composes with R4 (event interleaving) | yes (events render in a sibling htmx region) | yes (events render in a sibling htmx region) | yes if we pick a multiplex |
| Composes with R3 (read-only mode) | yes (don't wire input handler) | yes (no input by default) | yes |
| Maintenance burden | upstream xterm.js + glue | tiny | non-trivial Go parser |

### Trade-off discussion

- **Bundle weight.** xterm.js's ~86 KB gzip is real but bounded; lazy-loaded
  on one route. Compare to the ~50 KB the agent-detail page already adds to
  baseline beads-viewer for activity-feed rendering (R4) regardless of
  renderer. The marginal cost of a real terminal is acceptable.
- **ANSI escape support.** Anything short of a full emulator is a parity
  regression for any agent that uses alt-screen TUIs (Claude Code does;
  its input UI is alt-screen). `<pre>` and SSE-row are *visibly broken* on
  these workloads.
- **Mobile viewport.** xterm.js's FitAddon recomputes geometry on resize;
  works on phones, with the caveat that the on-screen keyboard occludes the
  terminal. R3 will likely make mobile read-only, which sidesteps the IME
  quirks. `<pre>` is trivially mobile-friendly. SSE-row is fine.
- **Copy / paste.** xterm.js wins decisively. `<pre>`'s "swap blows away
  selection" is constant friction. SSE-row preserves selection only between
  swaps that don't touch the selected nodes, which the user can't predict.
- **Scrollback depth.** Only xterm.js gives true client-side scrollback
  without server state. `<pre>` has none; SSE-row caps it at server memory.
- **Accessibility.** `<pre>` is the cleanest a11y story by default. xterm.js
  needs `screenReaderMode: true` (configurable) and a focus-management story.
  Document this as an explicit requirement in R6.
- **Composes with R3 (write gate).** All three options send writes via the
  htmx control plane (`POST /input`, `POST /agent/nudge`, `POST /agent/respond`)
  per R1; the renderer is read-only either way. Read-only mode is one missing
  event handler, not a redesign.
- **Composes with R4 (event interleaving).** Bead events live in a sibling
  region (e.g. a side-panel or a thin event-strip below the terminal). They
  do *not* need to enter the terminal canvas. xterm.js owning the canvas
  while htmx owns events keeps the responsibilities separated.

## iOS parity bar

The iOS Gardenio app's "agent detail" view is **not a terminal emulator**. It
renders a structured activity feed of bead events.

Source: `usr/Gardenio/Gardenio/AgentsTab.swift` (`AgentDetailView`, lines 383+;
`ActivityLineRow`, lines 841+) and `usr/Gardenio/Gardenio/Models.swift`
(`AgentActivityLine`, lines 383–396; `ActivityLineType`, lines 398–415).

What it actually displays:

- A `LazyVStack` of `ActivityLineRow` views, one per `AgentActivityLine`.
- Each line carries: timestamp, an `ActivityLineType` (`PreToolUse`,
  `PostToolUse`, `UserPromptSubmit`, `assistant_text`, `thinking`, `squawk`,
  `Notification`, `SessionStart`/`End`, `AgentStarted`/`Stopped`/`Crashed`,
  `PreCompact`, `SubagentStart`/`Stop`, ...), an actor, a `detail` string, and
  optional `toolName` / `toolInput` / `fullToolInput` / `fullToolResponse` /
  `cwd` / `sessionId`.
- Rendering is SwiftUI Text rows, monospaced font for tool names and IDs,
  proportional font for prompts and squawks. An expand chevron reveals
  full tool input/output in a side block.
- `slackMode` filter shows only "slack-grade" event types
  (squawks, prompts, assistant text). Toggleable.
- A *count* footer ("123 events") and an idle indicator. No PTY frame buffer,
  no ANSI parsing, no scrollback discipline beyond LazyVStack scrolling.

What this means for parity:

1. **The iOS user does not see ANSI escapes, cursor moves, or a terminal
   frame buffer.** They see a structured event log with one row per bead
   event. This is shaped by NATS bead events, not coopmux PTY output.
2. **The iOS feature parity bar belongs to R4 (event interleaving) and R5
   (full parity matrix), not R2.** Whatever R2 picks for the *terminal* surface
   is incremental over the iOS surface, not a substitute for it.
3. **Therefore xterm.js is parity-positive, not parity-negative.** Picking a
   real terminal does not "outrun iOS"; it adds a different surface (PTY
   bytes) on top of the same bead-event surface iOS already has.
4. **The htmx-`<pre>`-only option is *not* iOS parity** — it is a strict
   subset (no scrollback, no selection that survives an update, no alt-screen).

The user-visible parity bar for beads-viewer is therefore: **(a)** match iOS
on the bead-event surface (R4 / R5 deliverable), **(b)** add a live PTY
terminal as a *new* surface beads-viewer offers that iOS does not. R2 is
about (b). xterm.js is the only option that delivers a live PTY terminal at
parity with what users expect from "a real terminal."

## Decision

**Adopt Option A — xterm.js, lazy-loaded on `/agents/{id}` only.** Ship
Option B (server-rendered `<pre>` of `/screen`) as the initial HTML so the
page is meaningful before JS attaches and degrades gracefully when JS is
disabled. Reject Option C as the primary path because the cost (Go ANSI
state machine + steady-state byte-shovel) does not buy anything xterm.js
doesn't already give us.

### Rendering pipeline (sketch — NOT implementation)

```text
Server-rendered first paint           Live attach (after defer-load)
─────────────────────────────         ──────────────────────────────
beads-viewer GET /agents/{id}         Browser: load /static/xterm.bundle.js
  → renders bead_show.html              → new Terminal({ scrollback: 1000,
  → embeds:                                            screenReaderMode: true })
    <section id="agent-terminal">     → term.open(document.getElementById('term'))
      <pre id="term-fallback">        → term.write(initialScreenAnsiText)
        {{ .ScreenAnsiText }}         → ws = new WebSocket(
      </pre>                              `wss://coopmux.<host>/ws/${sessionId}`)
      <div id="term"                  → ws.onmessage = e => term.write(e.data)
           hidden></div>              → on attach: hide #term-fallback, show #term
      <div id="term-status"           → on close: htmx GET /agents/{id}/screen
           hx-...>                      to refresh fallback; reopen WS w/ backoff
    </section>
```

### Bundle-size budget

- Cap the agent-detail JS bundle at **125 KB gzipped** for v1. Track it in CI
  (the implementation plan in R6 should add a size assertion to the build).
- Do **not** ship xterm on routes other than `/agents/{id}`. The base
  beads-viewer page must remain unaffected.
- Use a content-hashed filename + immutable Cache-Control so the asset is
  cached aggressively (loaded at most once per release).

### Mobile / desktop parity check

| Surface | Desktop | Mobile |
|---|---|---|
| ANSI rendering | full | full (xterm.js renders the same on touch) |
| Scrollback | mouse wheel + keyboard | swipe |
| Selection / copy | drag-select + `Cmd/Ctrl-C` (or right-click) | tap-and-hold (works in xterm.js v5) |
| Resize | continuous (FitAddon) | continuous (FitAddon on `visualViewport` resize) |
| Input (R3 — out of scope here) | full keyboard | on-screen keyboard; IME quirks; **R3 likely defaults mobile to read-only** |
| Accessibility | `screenReaderMode: true` + focus ring | same; iOS VoiceOver navigates the live region |
| `<noscript>` fallback | `<pre>` of `/screen` | `<pre>` of `/screen` |

The mobile experience is *good for read*, *acceptable for short input*, and
explicitly *not* a primary write surface. R3 should formalize this
(read-only on viewport widths under, say, 600 px, or behind an explicit
"Enable input" toggle that defaults off on touch).

The desktop experience is the primary case and is fully featured.

### Accessibility checklist (must be in the R6 implementation plan)

- `screenReaderMode: true` enabled (NOT default-off).
- The terminal `<div>` has `role="application"` + an accessible name.
- A keyboard-focus indicator on the terminal container.
- The `<pre>` fallback remains in the DOM (visually hidden) for screen
  readers that prefer it; alternatively, the WS-attached terminal pushes
  a textual mirror to a sibling `aria-live="polite"` region.
- Document a "scroll to bottom" / "pause autoscroll" affordance — both for
  ergonomics and a11y.

## Open questions (downstream tasks)

| ID | Question | Affects |
|---|---|---|
| OQ-7 | Confirm xterm.js v5 minor pin and addon set in R6 (specifically: do we need `xterm-addon-canvas` for perf on long sessions, or stay on the default DOM renderer?). | R6 |
| OQ-8 | Where does the static asset live? (`gardenio/controller/cmd/beads-viewer/static/`?) Do we vendor xterm.js or pull from an npm step in CI? Pin the answer in R6 — vendoring keeps the implementation MR self-contained; npm makes upgrades easier. | R6 |
| OQ-9 | Bundle size assertion in CI: which job runs it (RWX `build-beads-viewer`?), and what is the failure mode (warn vs. red)? | R6 |
| OQ-10 | When R4 picks an event-interleaving model, does it want bead events rendered *inside* the xterm canvas (e.g. as italic injected lines) or in a sibling region? Recommendation here is sibling region (keeps the terminal a clean PTY mirror); R4 to confirm. | R4 |
| OQ-11 | Reconnect/replay: on WS drop, do we (a) re-fetch `/screen` and resume from there, or (b) re-fetch `/output?offset=` and replay raw bytes through `term.write`? (b) preserves alt-screen state more faithfully but is more code. | R4 |
| OQ-12 | A11y validation: do we have a screen-reader test path in the beads-viewer Playwright suite? If not, file a follow-up; the renderer's a11y story should be testable. | R6 |

## Anti-scope reminders

- **No coopmux changes.** xterm.js consumes whatever coopmux's WS already emits.
- **No new transport.** WS leg per R1; control verbs per R1; SSE not introduced.
- **No bespoke ANSI parser in Go.** xterm.js owns ANSI parsing.
- **No xterm.js on routes other than `/agents/{id}`.** Lazy-load only.

## Acceptance for this ADR

- [x] Three options compared with trade-offs (bundle, ANSI, mobile, copy/paste,
      scrollback, a11y).
- [x] iOS Gardenio app's terminal renderer surveyed (`AgentsTab.swift`
      `AgentDetailView` + `ActivityLineRow`); the parity bar is event-shaped,
      not terminal-shaped.
- [x] Recommendation (xterm.js, lazy-loaded; `<pre>` fallback) with rationale.
- [x] Bundle-size budget noted (125 KB gzipped, agent-detail route only).
- [x] Mobile / desktop parity check called out (table + explicit
      "mobile defaults to read-only per R3").
- [x] Open questions filed for downstream tasks (R4, R6).
- [x] No Go / template / JS / coopmux code changes.
