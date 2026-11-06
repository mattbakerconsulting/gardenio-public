# ADR — beads-viewer agent terminal: iOS-parity feature checklist

- **Bead:** [ka-0000003585](https://beads.gardenio.e2e.example.com/ka-0000003585)
- **Parent epic:** [ka-0000003802](https://beads.gardenio.e2e.example.com/ka-0000003802) — beads-viewer iOS-parity Phase 3 (agent-terminal custom view)
- **Predecessors:**
  - R1 architecture — `gardenio/docs/research/beads-viewer-agent-terminal-architecture.md` ([ka-0000006916](https://beads.gardenio.e2e.example.com/ka-0000006916))
  - R2 renderer — `gardenio/docs/research/beads-viewer-agent-terminal-renderer.md` ([ka-0000003333](https://beads.gardenio.e2e.example.com/ka-0000003333))
  - R3 send-commands — `gardenio/docs/research/beads-viewer-agent-terminal-send-commands.md` ([ka-0000002915](https://beads.gardenio.e2e.example.com/ka-0000002915))
  - R4 events — `gardenio/docs/research/beads-viewer-agent-terminal-events.md` ([ka-0000004621](https://beads.gardenio.e2e.example.com/ka-0000004621))
- **Status:** Proposed (research-only; no implementation until human green-lights via `gb decision`)
- **Scope:** Map every visible feature of the iOS Gardenio `AgentDetailView` to a proposed beads-viewer behavior, with explicit parity gap. Excludes: implementation plan (R6).

## TL;DR

> The iOS `AgentDetailView` is **not a PTY terminal** — it is a structured
> activity feed of bead/Slack-grade events, with a header strip of agent
> metadata, an inline pending-decisions block, and a single text input that
> sends a *mail bead* (not keystrokes) to the agent. The parent epic asks
> beads-viewer to be a "fully fledged terminal much like the iOS app";
> **fully-fledged terminal** is the *aspiration*, **iOS app** is the
> *reference UI*. R1–R4 already commit beads-viewer to a real PTY pane
> (xterm.js over WS) plus a parallel SSE bead-events lane (R4) and four
> write verbs (R3 — `input`, `keys`, `nudge`, `respond`). Therefore
> beads-viewer **meets full iOS parity** on every event-feed feature **and
> exceeds it** on PTY fidelity, deep-linking, copy/paste, side-by-side
> compare, and key-level input.
>
> The only iOS surfaces deliberately omitted are tightly-coupled SwiftUI
> idioms with no good htmx equivalent: navigation-stack push, swipe-to-
> respawn (lives on the *list* row, not the detail view), AppStorage-backed
> per-device font-size persistence, and the "clear local activity" trash
> button (which only clears local-device state — there is no server
> truncate). All four are listed as **Omitted** with rationale below.

## Method

This document was produced by reading the iOS source under
`usr/Gardenio/Gardenio/`:

- `AgentsTab.swift:383-592` — `struct AgentDetailView` (the screen itself)
- `AgentsTab.swift:594-677` — `private struct AgentDetailHeader`
- `AgentsTab.swift:741-837` — `private struct RespawnPromptSheet` (reachable
  *from the agent list row* via swipe, **not** from `AgentDetailView`)
- `AgentsTab.swift:839-995` — `private struct ActivityLineRow`
- `Models.swift:208-221` — `struct AgentInfo`
- `Models.swift:383-472` — `struct AgentActivityLine` + `enum
  ActivityLineType` (icon/color/`isSlackGrade`)

Every iOS feature called out below was observed *visually rendered* in the
detail view; I did not infer features from neighboring screens (the agent
list, the respawn sheet, the in-progress tab) unless they participate in
the detail-view flow.

## What `AgentDetailView` actually shows

```
┌─────────────────────────────────────────────────┐
│  navigationTitle: <actor>                       │
│  toolbar: [N events]  [filter]  [Aa]  [trash]   │
├─────────────────────────────────────────────────┤
│ AgentDetailHeader                               │
│   • state pill (working/idle, color-coded)      │
│   • coop-link indicator (just a presence dot)   │
│   • "started 5m ago"                            │
│   • project · role badges                       │
│   • claimed task title (Label + ticket icon)    │
│   • toolName · session(8) · cwd                 │
├─────────────────────────────────────────────────┤
│ Inline pending decisions (if any)               │
│   DecisionRow(.inline) — selectable options     │
├─────────────────────────────────────────────────┤
│ ScrollView                                      │
│   ActivityLineRow × N                           │
│     timestamp │ icon │ detail [chevron]         │
│     (tap → expand: input, output, cwd, sess)    │
├─────────────────────────────────────────────────┤
│ HStack: TextField "Message <actor>..."   [⏵]    │
│   submit / paperplane → store.sendMessage(...)  │
│   delivers a *mail bead*, not a keystroke       │
└─────────────────────────────────────────────────┘
```

Two things to note before reading the matrix:

1. **There is no PTY pane.** `AgentDetailView` does not render terminal
   output, ANSI, or scrollback. The "live" surface is `lines: [AgentActivityLine]`
   filled by `store.startAgentStream(actor:)` and rendered with structured
   per-event affordances (icon, color, expansion). The closest iOS gets to
   a terminal is the *expanded* `Input` / `Output` blob inside an
   `ActivityLineRow` (`AgentsTab.swift:976-994`), which is `textSelection
   (.enabled)` and `lineLimit(30)`.
2. **There is no nudge / respond / keys.** The only write affordance is
   `sendMessageToAgent()` (`AgentsTab.swift:582-591`), which calls
   `store.sendMessage(to:body:)` — a mail bead. There is no `Ctrl-C`, no
   `Enter` button, no nudge prod, no respond UI. iOS therefore sets a
   *very low* parity floor on the input side; R3's four-verb surface is
   already strictly more capable.

## 3-column parity matrix

Legend for the **Gap** column: **Full** = beads-viewer matches the iOS
behavior; **Partial** = beads-viewer renders the same intent but trades
a SwiftUI idiom for an htmx-native equivalent; **Exceeds** = beads-viewer
goes beyond what iOS does (with rationale); **Omitted** = beads-viewer
does not render the iOS feature, with rationale.

### 1. Header strip (`AgentDetailHeader`)

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **State pill** color-coded `working` (green) / other (orange), pulled from `bead.fields.agent_state ?? bead.fields.status` (`AgentsTab.swift:604-613`) | Same pill in the bead-show header strip. Source the same fields off the daemon `bead.fields`. The bead-events SSE lane (R4) already drives `bead.updated`; pill swaps on the next event. | **Full** |
| **Coop-link indicator** — green link icon + "coop" caption when `KbeadsStore.parseCoopURL` finds a URL in `bead.notes` (`AgentsTab.swift:614-623`) | Same indicator, rendered server-side from the same `notes` parse. On beads-viewer this is *also* the affordance that gates whether the PTY pane mounts at all — no coop URL ⇒ pane shows the static `<noscript>` fallback (R2 §"`<pre>` fallback") with a one-line "agent has no live session" message. | **Exceeds** — iOS dot is purely informational; beads-viewer uses the same signal to decide whether to attach `wss://coopmux.<host>/ws/{session_id}` (R1). |
| **"started X ago"** caption from `bead.createdAt` (`AgentsTab.swift:625-629`) | Same caption, generated server-side via the existing `fmtTimeAgo` helper (the viewer already has equivalents in `bead_show.html`). htmx swap on every `bead.updated`. | **Full** |
| **Project + role labels** with SF Symbols `folder` and `person.text.rectangle` (`AgentsTab.swift:633-643`) | Same two pills with equivalent inline SVGs (or unicode glyphs). Already a solved pattern — `type_agent.html` renders both today as `<dd>` rows. The new agent view promotes them into the header strip. | **Full** |
| **Claimed task title** with ticket icon, `lineLimit(2)`, sourced from roster `agentInfo.taskTitle` (`AgentsTab.swift:647-652`) | Same affordance, **as a hyperlink** to `/{taskId}` (the bead-show route already exists). Two-line CSS clamp via `-webkit-line-clamp: 2`. | **Exceeds** — iOS task title is plain text; viewer turns it into a deep-link to the task bead. |
| **Tool/session/cwd footer** — `agentInfo.toolName` (blue monospaced), `session.prefix(8)`, `cwd` lineLimit-1 (`AgentsTab.swift:654-674`) | Same three fields rendered server-side from the daemon roster API (see `agents.go` — viewer already calls the roster). `cwd` becomes a `<span title="...">` with CSS `text-overflow: ellipsis` so hover reveals the full path. | **Exceeds** — viewer can show full cwd on hover; iOS truncates with no recovery. |

### 2. Pending decisions block

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **Inline `DecisionRow(.inline)`** for every decision in `store.decisions` where `decision.requestedBy == actor`, scrollable up to 480 px, orange-tinted background (`AgentsTab.swift:441-453`) | beads-viewer already renders decisions in `bead_show.html` via the daemon's decision API. The agent view embeds the same partial filtered to this agent's open decisions, with the orange-tint container to match the iOS "this needs you" emphasis. Bead-events SSE (R4 §"bead-events lane") drives appearance / disappearance without page reload. | **Full** |

### 3. Activity feed (the "terminal-shaped" surface)

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **PTY / scrollback / live terminal** | **iOS has none**, so there is no parity floor. beads-viewer ships an xterm.js pane (R2) attached to `wss://coopmux.<host>/ws/{session_id}` (R1) with the FitAddon for terminal sizing and the WebLinksAddon for clickable URLs. PTY output is throttled per R4 §"backpressure". | **Exceeds (large)** — full ANSI fidelity, alt-screen, true scrollback, terminal selection, OSC 52 clipboard. |
| **Structured activity feed** (`displayedLines`, `LazyVStack`, `ActivityLineRow`) | Rendered server-side as the **bead-events lane** (R4) — a separate DOM region next to (or below) the xterm.js pane. The htmx hx-swap target is appended to from the SSE feed (`/agents/{id}/events`). One DOM node per event keeps the LazyVStack-equivalent cheap. | **Full** |
| **Per-row icon + color** (16 `ActivityLineType` cases, `Models.swift:417-453`) | Same 16 cases mapped to inline SVGs and CSS classes (`act-tooluse`, `act-squawk`, `act-crashed`, …). Color tokens already live in beads-viewer's stylesheet; we add a new block for activity-line types. | **Full** |
| **Slack-mode filter toggle** (toolbar menu, `@AppStorage("activityFeedSlackMode")` defaults true; `isSlackGrade` filter in `Models.swift:458-471`) | Server-side query param `?mode=slack` (default) vs `?mode=raw`. Toolbar button is a `<button>` that issues an htmx GET swapping the feed region. Setting persists in a cookie (already used elsewhere in beads-viewer) so it survives reload. | **Full** |
| **Events count badge** "N events" or "M/N" when filtered (`AgentsTab.swift:537-541`) | Same span in the toolbar, rendered as `<span hx-swap-oob="true" id="evcount">`. SSE events update the count without re-rendering the feed. | **Full** |
| **Font size selector** (4 sizes via `enum AgentFontSize`, `@AppStorage("agentFontSize")`) | CSS-driven font scale on `.agent-feed` and `.xterm` with three sizes: small/medium/large. Persisted in cookie. iOS has 4 sizes; viewer collapses `mediumLarge` and `large` into one — a deliberate trim, no information loss. | **Partial** — same intent (operator-controlled density), one fewer step on the slider. |
| **Auto-scroll-to-end** on new event (`onChange(of: displayedLines.count)` → `proxy.scrollTo(.bottom)`) | "Stick to bottom" CSS class toggled by JS — track `isPinnedToBottom` from the user's last scroll position; new events `scrollTop = scrollHeight` only when pinned. R4 §"page reload" already specifies the same logic for the PTY pane; the bead-events lane reuses it. | **Full** |
| **Tap-to-expand** an `ActivityLineRow` to show full tool input/output, cwd, session id (`AgentsTab.swift:867-994`) | `<details>`/`<summary>` per row. The summary is the collapsed line; the contents block carries `<pre>` for input + output (with `textSelection(.enabled)` equivalent — `user-select: text`), the metadata row (rawType, cwd suffix, session prefix), and the same color-coded section heads. | **Full** |
| **Tool failure / agent crash highlight** — red background on rows of type `toolFail` or `agentCrashed` (`AgentsTab.swift:963-966`) | CSS class `.act-fail` with `background: var(--red-tint)`. Driven by the same `type` field on the SSE event. | **Full** |
| **Squawk prose rendering** — non-monospaced, lineLimit 3 collapsed, multiline expanded (`AgentsTab.swift:905-912`) | Squawk rows render in the page font (not monospace) and clamp to 3 lines via `-webkit-line-clamp` until expanded. | **Full** |
| **Empty-state messaging** with two variants: "No events yet" vs "No Slack-grade events yet — N raw events hidden" (`AgentsTab.swift:456-480`) | Same two strings rendered server-side based on `len(events)` and the active mode. The "N raw events hidden" line links to the same view with `?mode=raw`. | **Exceeds** — viewer makes the recovery action a real link; iOS makes it a hint. |
| **Stream lifecycle** — `onAppear { store.startAgentStream(actor:) }` / `onDisappear { stop }` | Browser equivalent: SSE `EventSource` opened on page load, `EventSource.close()` on `beforeunload`. R4 §"reconnect/replay" defines the resume path (since-cursor for bead events, byte-offset for PTY). | **Full** |

### 4. Send-message bar (the iOS write surface)

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **`TextField "Message <actor>…"`** with paperplane button, submit-on-return, disabled while sending; calls `store.sendMessage(to:body:)` (a *mail bead*, not a keystroke) (`AgentsTab.swift:506-526, 582-591`) | beads-viewer ships **four** write affordances per R3: (1) `<form hx-post="/agents/{id}/input">` with a textarea + Send button — the iOS equivalent, but it actually feeds coopmux `POST /api/v1/sessions/{id}/input` (typed text into the PTY); (2) a "Send mail bead" form that mirrors iOS's behavior 1-to-1 by creating a mail bead; (3) a `keys` row with buttons for `Enter`, `Esc`, `Tab`, `Ctrl-C`, `Ctrl-D`, `Ctrl-\` (each a POST to `/agents/{id}/keys`, with confirmation on the destructive three per R3 §"destructive-key confirmation"); (4) Nudge / Respond buttons backed by the existing coopmux verbs. All four are gated by the **read-only-by-default + "Take control" toggle** + `gardenio-operator` role (R3 §"authorization"). | **Exceeds (large)** — iOS users can only mail; viewer operators can mail *and* drive the actual TTY. |

### 5. Toolbar / chrome

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **Navigation title = actor name** (`navigationTitle(actor)` + `inline` display mode, `AgentsTab.swift:531-532`) | `<title>` = `actor — beads-viewer`; `<h1>` = actor with the same metadata strip immediately below. The bead-show breadcrumb already does this for non-agent beads; the agent view inherits it. | **Full** |
| **Connection badge** (`ConnectionBadge(connected: store.connected)`) — globally on `AgentsTab`, but the same connectivity state colors the toolbar on the detail screen via re-render | Per-pane connection indicator: PTY-WS green/red dot driven by xterm.js's `onClose`; bead-events SSE green/red dot driven by `EventSource.onerror`. Both on the toolbar so a glance shows which transport is healthy. | **Exceeds** — iOS shows one global dot; viewer separates the two transports so the operator can see a half-degraded state. |
| **Refresh button** (`store.refreshAll()` on the parent `AgentsTab`) | Page-level `Reload` link in the toolbar that re-issues the `/screen` snapshot + bead-events catchup (R4 §"page reload"). xterm.js holds its scrollback; bead-events lane fetches `?since=<rfc3339>`. | **Full** |
| **`trash` "clear activity"** button — local-device only, calls `store.clearActivity(for: actor)` (`AgentsTab.swift:571-576`) | **Omitted.** The iOS button only clears the in-memory `agentActivity[actor]` array on the device; it does not delete server data. In a multi-device web view that local-only clear has no analog (each browser tab is already its own client; close the tab and the in-memory state is gone). Adding a "Clear" button that only resets the current page's DOM would mislead operators into thinking it clears history. Page reload (already on the toolbar) reaches the same end state. | **Omitted** (rationale above) |
| **`textformat.size` font menu** (4 sizes) | Collapsed to **3 sizes** (small / medium / large), same intent. See activity-feed row above. | **Partial** |
| **`line.3.horizontal.decrease.circle` filter menu** with Slack-mode toggle | Same toggle, see activity-feed row above. | **Full** |

### 6. iOS-specific idioms (intentionally not ported)

| iOS feature | Proposed beads-viewer behavior | Gap |
|---|---|---|
| **`NavigationStack` push** from the agent list (`AgentsTab.swift:153-155`) | beads-viewer already has flat URL-addressable bead pages. Navigation is a hyperlink, not a stack push. There is no back-stack to model. | **Omitted** — htmx-native navigation supersedes; nothing to port. |
| **Swipe-to-respawn** on the *list* row (`AgentsTab.swift:135-143`, opens `RespawnPromptSheet`) | Lives on the agent list, not in `AgentDetailView`. Out of scope for this ADR (the bead, "Phase 3 agent-terminal *custom view*", is detail-view-only). The respawn flow is already covered by the existing bead-actions partial; a follow-up can add a "Respawn" button to the agent header strip if desired, but it is not iOS-parity for the *detail* surface. | **Omitted (out of scope)** — re-list as a R6 follow-up if the implementation plan wants it. |
| **`@AppStorage` per-device persistence** for filter mode and font size | Browser equivalent is cookies (already used by beads-viewer for theme and column widths). Same UX, different storage. | **Partial** — captured under the filter/font-size rows above; no separate work item. |
| **Pull-to-refresh** (`refreshable` on the parent list) | Browser pull-to-refresh is OS-native and does the right thing (full page reload). No JS needed. | **Full (free)** |
| **SF Symbols icons** (`waveform.path`, `bubble.left.and.bubble.right`, `paperplane.fill`, `wrench`, etc.) | All replaced by inline SVG sprites already in beads-viewer's `static/icons/`. The empty-state and per-row icons map 1:1 to SVG equivalents. | **Full** — visually equivalent, free of SF Symbols license/availability concerns. |
| **`presentationDetents([.medium, .large])`** on the `RespawnPromptSheet` | Out of scope (sheet lives on list, not detail). When/if respawn is added to the detail header, an htmx modal swaps in — htmx has no concept of detents and does not need one. | **Omitted (out of scope)** |
| **Haptic feedback** (implicit in SwiftUI buttons) | No browser equivalent that adds value here. Visual-only feedback (button press states, spinner on pending writes). | **Omitted** — no analog. |
| **VoiceOver / Dynamic Type integration** | Web equivalents (semantic HTML, ARIA labels on the toolbar buttons, `prefers-reduced-motion` respected by xterm.js animations, browser zoom for type scaling). Not a feature port; a baseline accessibility floor. | **Full (different mechanism)** |

### 7. Beads-viewer features that *exceed* iOS (consolidated)

This section calls out the user-visible features the viewer adds on top of
iOS parity. They earn the parent epic's "fully fledged terminal much like
the iOS app" framing — *like* the iOS app, plus the things only a browser
can do.

| Capability | What viewer does | Why iOS can't / doesn't |
|---|---|---|
| **Real PTY terminal** | xterm.js pane attached to coopmux WS (R1 + R2). Full ANSI / alt-screen / scrollback. | iOS shows structured events only — no PTY surface. |
| **Deep-linkable everything** | Every state (the agent, the bead, the task it claimed, the decision it raised, the parent epic, the spawning thread) is a URL. The header's claimed-task title links to `/{taskId}`; squawk rows that mention bead IDs auto-link via WebLinksAddon-style regex. | iOS uses a navigation stack; deep-linking exists (`gardenio://...`) but is not the primary affordance and does not survive paste-into-browser. |
| **Browser clipboard** | xterm.js terminal selection + Ctrl/Cmd-C copies real text (not screenshot). OSC 52 inbound clipboard write supported per xterm.js defaults. Bead-events lane content is `user-select: text`. | iOS terminal-style selection across an event feed is awkward; selection inside an iOS `Text` element doesn't compose into a single buffer. |
| **Side-by-side compare** | Two browser tabs / split window naturally compare two agents' feeds; viewer's URL scheme makes that one drag-and-drop. A future tab-strip in beads-viewer (already a known follow-up) could embed two agent feeds in one page. | iOS shows one detail screen at a time; iPadOS multitasking is the only escape and is not first-class. |
| **Per-transport health badges** | Separate connection dots for the PTY WS and the bead-events SSE so operators can tell "agent is working but events are stale" from "agent is dead". | iOS has one composite dot. |
| **Hover for full cwd** | `<span title="full/path">truncated/path</span>` reveals the full working directory on hover. | iOS truncation is one-way. |
| **Fall-through `<pre>` fallback** | When JS is disabled or the WS fails, the page still renders the latest `/screen` snapshot as a static `<pre>` (R2 §"`<pre>` fallback"). | iOS has no JS-disabled state to fall back to. |
| **Audit trail** | Every write verb stamps `X-Gardenio-User` + `X-Gardenio-Roles` and writes a structured access-log line in beads-viewer (R3 §"audit"). | iOS writes are mail beads attributed to the device's OIDC user, but there is no per-keystroke audit because iOS does not send keystrokes. |
| **`gardenio-operator` role gate** | Read-only by default. "Take control" toggle gated on the existing Keycloak role (R3). | iOS does not differentiate read-only vs. write users — possession of the device is the gate. |
| **Search-in-feed** (cheap follow-up; not iOS) | Browser `Ctrl-F` already searches the rendered DOM. xterm.js has a Search addon (40 KB gz). | iOS has no in-feed find; the activity feed is opaque to the system find affordance. |

## Summary roll-up

| Bucket | Count | Notes |
|---|---|---|
| Full parity | 14 | One-for-one rendering of the iOS feature with htmx-native primitives. |
| Partial | 3 | Same intent, slightly trimmed surface (font-size sizes, `@AppStorage` → cookies, kept under "Partial" only because the storage substrate differs). |
| Exceeds | 8 | viewer offers more than iOS — most importantly the PTY pane + key-level input. |
| Omitted | 7 | All accounted for above with explicit rationale; no "TBD" entries. |

**No feature in `AgentDetailView` is left unaddressed.** The omissions split
cleanly into three reasons:

1. **No good web analog** — local-only clear, haptics, navigation-stack
   push, sheet detents.
2. **Out of scope for the *detail* view** — swipe-to-respawn, `RespawnPromptSheet`
   (both live on the agent list and may earn a follow-up).
3. **Different storage substrate, same UX** — `@AppStorage` filter/font
   persistence becomes a cookie.

## How this ADR feeds R6

The R6 implementation plan can lift this matrix verbatim into a phased rollout:

1. **Skeleton template** — header strip, decisions block, empty-state
   feed, send-message form (mail bead). Yields **Full** parity for header,
   decisions, empty-state, and the iOS-equivalent mail input — one
   meaningful page with zero JS.
2. **Read-only stream** — wire the bead-events SSE lane (R4) into the
   feed. Yields **Full** parity for activity feed, slack-mode, count
   badge, auto-scroll, expansion, failure highlight, squawk prose.
3. **PTY pane** — drop in xterm.js + WS attach (R1 + R2). Yields the
   first **Exceeds** capability and the parent epic's headline feature.
4. **Input** — text/keys POSTs gated by Take-control + `gardenio-operator`
   (R3). Yields the rest of the **Exceeds** column for write affordances.
5. **Polish** — font-size selector, hover-for-cwd, deep-links, search.

R6 should also file follow-up tasks for the two **out-of-scope omissions**
(respawn entry from the detail view; per-tab pinning for side-by-side
compare) so they are visible to the human approver but not load-bearing
for the green-light decision.

## Open questions for R6 / human approval

None are blocking this ADR; flagged here so R6 captures them:

- **Cookie names** for the per-page persistence (`agent_feed_mode`,
  `agent_feed_density`?) — pick a prefix consistent with existing
  beads-viewer cookies.
- **Side-by-side compare**: is this a v1 polish item (two tabs is fine)
  or a real split-pane in `bead_show.html`? Defer to operator demand.
- **Search-in-feed**: Ctrl-F is enough at v1; ship the xterm.js Search
  addon only if operators ask for it.

These questions are scoped to R6's plan-of-record. They do not affect the
parity matrix — every iOS feature has a definite proposed behavior here.

## Anti-scope

- **Implementation.** No Go, template, or JS code lands in this MR.
- **Changes to the iOS app.** This ADR is a one-way mapping from iOS to
  viewer, not a redesign of `AgentDetailView`.
- **Changes to coopmux.** R1/R3's anti-scope holds: no new coopmux
  endpoints, no new ring buffer, no new auth provider.
- **Re-deciding R1–R4.** Where this ADR cites a transport, renderer, write
  verb, or reconnect strategy, it is *consuming* the predecessor decision.
  Disagreements with R1–R4 belong in a new ADR or a `gb decision`, not in
  this one.
