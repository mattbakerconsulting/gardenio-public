# Agent chat: a Slack-shaped surface where every message is a live bead

**Status:** design spec / PRD, approved in conversation 2026-09-20, awaiting written review.
**Repo:** gardenio, `gardenio/controller/cmd/beads-viewer`.
**Mockups:** `assets/agent-chat/` (HTML fixtures plus PNG captures, see §11).
**Prior art this builds on:** the `/plans` card grammar (`templates/partials/plan_card.html`), the `/feed` prototype and its design of record (`gardenio/docs/beads-viewer-activity-feed-*.md`), the thread composer (`thread_compose.go`), the Slack bridge thread-forward path (`internal/bridge/bot_thread_forward.go`).

---

## 0. Decisions already taken

These were settled in the brainstorming conversation and are not open.

| # | Decision | Choice |
|---|---|---|
| D1 | What a message is | **One live card per bead.** A card rewrites itself in place as the bead moves. Never one card per event. |
| D2 | Message families | Squawks, operator messages, decisions and their reports (tldr, summary, design, post-mortem, bundle), escalations, mail, wrapups. Lifecycle beats (spawned, claimed, stopped, task closed) are thin one-line markers, the only append-only items. |
| D3 | Top-level unit | **The agent.** Messages are filed under the agent that produced or received them. The agent's plan is visible on the card, the assigned task is the most prominent element. |
| D4 | Closed agents | Stay in the stream. The card flips to a **closed face** that leads with the wrapup (tldr first). |
| D5 | Layout | **Two-pane default** (agent rail left, one conversation right). A **single-column stream view** with messages nested inside each agent card is a toggle. |
| D6 | Write path | Reuse the Slack model: a closed `type=message` bead assigned to the agent, attributed to the operator, plus a nudge. |
| D7 | Writing to a closed agent | **@mention semantics.** The message respawns the agent with session resume. The composer says so before you type. |
| D8 | Live updates | **Not seed-then-stream.** Events carry an agent id only; the client refetches the server-rendered card. A versions poll is the floor. One render path. |
| D9 | Stack | New page in beads-viewer, Go templates + htmx + one SSE stream off the existing `BeadFeedHub`. No new binary, no build step. |
| D10 | Scope | New route. Does not replace `/`, `/feed` or `/inbox`. Promotion is a later, separate decision. |
| D11 | Card title | **The task, not the agent.** The agent is an initials link in the tail; its full name lives in tooltips and search. A plan eyebrow sits above the title and is the only mention of the plan on the face. |
| D12 | Interaction | **Lightboxes.** Every non-toggle button opens a rich server-rendered modal over the page with the URL mirrored, never a navigation. |

## 1. Goals and non-goals

**Goals**

- Bring the density of the plan card to a conversation surface: an operator scans the fleet in one glance and acts (answer, approve, reply, resume) without leaving the page.
- Every agent is a conversation; every conversation reads from spawn to wrapup as a story with the assigned task always in view.
- Every card is truthful live state, never a stale snapshot of an event.
- The operator can talk back, with the same outcome a Slack reply or @mention has today.

**Non-goals**

- Not a replacement for the `/agents` fleet table, the `/plans` board, the Deck or the inbox. Those stay.
- Not a chat with the oracle (that is `gv-chatbox-prd.md`).
- Not a Slack client. The viewer still never holds a Slack token; relay stays in the bridge.
- Not a new event store. The daemon's durable events and the viewer's feed hub are the only sources.

## 2. Information architecture

### 2.1 Routes

| Route | What |
|---|---|
| `GET /agents/chat` | The page. Two-pane default. Query: `agent=<id>` selects the open conversation; `view=stream` switches to the nested single column; `project=`, `role=`, `state=live|closed|all`, `scope=mine|everyone` filter the rail; `q=` client-side search, mirrored to the URL like `/plans`. |
| `GET /agents/chat/rail` | Partial: the rail list for the current filters. |
| `GET /agents/chat/card/{agent}` | Partial: one full agent card (both faces render from here). **The only place an agent card is rendered.** |
| `GET /agents/chat/conversation/{agent}` | Partial: header card plus the message list plus the composer. |
| `GET /agents/chat/message/{bead}` | Partial: one message card. **The only place a message card is rendered.** |
| `GET /agents/chat/events` | SSE. Event name `agent-changed`, data is the agent id. Nothing else. |
| `GET /agents/chat/events-status` | The auth probe every SSE client here fetches first (copy of `handleAgentsEventsStatus`). |
| `GET /agents/chat/versions?ids=a,b,c` | JSON `{agentID: versionString}` or **204** when nothing moved since `If-None-Match`. |
| `POST /agents/chat/{agent}/say` | Composer. Operator-gated. See §7. |

Decision answers, escalation approve and reject, and agent restart keep their existing endpoints and swap the affected card in place via htmx.

### 2.2 Panes

**Rail (left, 22rem, scrolls independently).** One compact agent card per agent bead in scope, in attention order (§8). A selected card is tinted like `.plan-card.is-selected`. Each carries an unread dot from the existing last-seen mechanism and a small count of members that need the operator (open decisions plus open escalations).

**Conversation (right, fills).** The full agent card as a sticky header. Below it the message list, oldest at the top, newest at the bottom, scrolled to the bottom on open. Lifecycle markers interleave. The composer is pinned to the foot.

**Stream view (`view=stream`).** One column of full agent cards, same order as the rail, each with its last three messages nested and a "N earlier" fold. Clicking a card opens the two-pane view on that agent.

### 2.3 Responsive

Below 60rem the two panes collapse: the rail is the page, tapping an agent replaces it with the conversation full-width and a back link, and the browser back button returns to the rail. This is the same treatment `.feed-layout` and the plans cockpit already give two-column layouts. Under `pointer: coarse` every button reaches 44px via the transparent `::after` hit-area idiom, not larger visuals. `prefers-reduced-motion` disables the pulse dots.

### 2.4 Lightboxes are the preferred interaction

Added on review 2026-09-20. Every button on a card that is not a one-word toggle opens a **lightbox**: a centred modal over the current view, holding a rich server-rendered partial, with the URL mirrored as `?lb=<kind>:<id>` (pushState, Esc or backdrop closes, back button closes) exactly as the plans peek drawer does with `?peek=`. There is one lightbox host on the page (`#lb`), one JS driver (`agent_chat_lightbox.js`, a sibling of `plans_board.js`), and every body is an htmx GET of an existing or new partial. Nothing navigates away, the conversation and its scroll position survive, and the card behind updates in place when the action lands.

Why this makes the cards nicer: the face can stay at the density of the plan card because depth is one click away and never a page away. A mis-tap on a dense chip strip opens context, it does not fire an action. And every action gets the lineage the operator actually wants before committing: the plan and step, the task, the agent, related MRs, and what happens next.

| Trigger on the card | Lightbox body | Source partial |
|---|---|---|
| A decision option, or the decision header | Full prompt, every option with its long label, rationale and `artifact_type`, a note field, lineage rail (plan, step, agent, related MRs, earlier decisions), an "after you resolve" line, keyboard `1–9` pick and `⏎` resolve. Resolve posts to the existing endpoint and swaps the card behind. | new `agent_chat_lb_decision.html`, built from `type_decision.html` + `decision_context.html` |
| Approve or reject on an escalation | Operation type, sensitivity, the parsed `operation_spec` targets, what the replay executor will do, capability check result, reason field. | `type_escalation.html` |
| `task →`, a task id, a bead pill | The bead preview (fields, description, comments tail, MR state) with a link to the full page. | `bead_preview.go` partial, already used by hover previews |
| `plan cockpit →`, the plan eyebrow | The plan peek header plus the step timeline, anchored at the agent's step. | `GET /plans/{id}/peek` |
| `agent →` | Agent card, spend, exit reason, transcript tail, restart lever. | `type_agent.html` compact form |
| `terminal →` | The coopmux terminal embedded, full height. | `agent_terminal.css` + xterm bundle, as on the agent page |
| `resume ▶`, `stop`, `nudge` | Confirmation with the concrete outcome: session volume present or not, last session age, estimated cold start, the message that will be sent. | new `agent_chat_lb_confirm.html` |
| `expand` on a report, `more` on a wrapup row | Full markdown render of the report or wrapup with its metadata. | `type_report.html`, wrapup section of `type_agent.html` |
| A lifecycle marker | The underlying event with its full payload diff. | `bead_activity.html` row, expanded |

Rules: a lightbox is never nested (opening one from inside another replaces it, back returns); it is the same size class on every kind (`min(56rem, 100%)`, 90vh); on phone width it is full-screen with the rail hidden; focus is trapped and returned to the trigger; every action inside re-renders the card behind via the same `agent-changed` refetch path (§6) so the lightbox never carries its own copy of card state.

## 3. The agent card

Both faces share one DOM skeleton so a state change swaps in place without a layout jump. Elements in layout order. Class names are proposals; the density grammar rules in §9 apply to every element.

### 3.0 Plan eyebrow (`.ac-eyebrow`)

One small line above the head on every face: `PLAN` in caps, the plan title in 0.64rem bold clamped to one line, and the step position (`· #7 of 17`), linking to the plan cockpit anchored at that step. An agent with no plan ancestor reads `NO PLAN · unfiled work`. The eyebrow is the **only** place the plan is named on the card face; the task band does not repeat it (review 2026-09-20: the first mockup showed the plan twice, eyebrow plus breadcrumb, and the redundancy was cut).

### 3.1 Head row (`.ac-head`, flex-wrap, baseline)

Changed on review 2026-09-20: the agent name is not the important thing, the task is. The card is titled by the task.

1. **Task title** as the card title, 0.8rem bold, two-line clamp, linking to the task bead. A gardener, whose "task" is its plan epic, titles by the epic. An agent with no claimed bead titles by its spawn prompt's first line.
2. Tail group (`.ac-head-tail`, wraps as a unit): the **agent link**, an initials avatar (`actorInitials` with the redundant leading project token stripped) plus `agent →`, with the full agent name and bead id in the tooltip, linking to the agent page; then the compact age of the last activity in the conversation (`AgoCompact`), stale tint past 24h for a live agent.

The full agent name appears nowhere on the face. It lives in tooltips, in the page `<title>` when a conversation is open, and in the search haystack.

### 3.2 Chip strip (`.ac-meta`, column-gap .3rem, row-gap .18rem)

Zero-suppressed and folded, in this order:

- **State chip** with a pulsing dot: `running · live`, `spawning`, `stuck · 2h` (alive state but stale heartbeat, loud), `idle`, `stopped`, `crashed · 143` (exit code folded on), `retired`. Same tone classes as `plan_agent_chip`.
- **Role chip** (`crew`, `gardener`, `plan`, `reviewer`).
- **Project chip**, suppressed when the rail is filtered to that project.
- **Priority pill**, only when the assigned task is not P2, same colours as the plan card.
- **Operator initials** (`by MA`), suppressed when scope is `mine`.
- **Needs-you chip** `2 to answer · 3d` linking to the first open decision, loud past 14d. **Escalation badge** `⛔ merge MR · 7h`, loud past 7d. These two are the #1 and #2 attention tiers exactly as on the plan card.
- **Spend** `$4.12`, tabular numerals.

### 3.3 The band

The band is the card's centre of gravity and is the only element that differs between faces.

**Running face: task band (`.ac-task`).** A full-width tinted band under the title. One line: the task id and its status chip. The plan is not repeated here, the eyebrow (§3.0) already names it. When the agent has an **open decision or escalation**, that message card is rendered inside the band directly under the task line with its inline answer or approve controls, so the operator never scrolls to act.

**Closed face: wrapup band (`.ac-wrapup`).** Same band footprint. First line: the wrapup `tldr` in bold, clamped to two lines. Then up to three labelled compact rows: `did` (accomplishments), `blocked` (blockers), `handoff` (handoff notes), each one line with the full text in the tooltip and a `more` fold. Then a pill row: `beads_closed` as bead-id pills, `pull_requests` as MR pills with the CI glyph vocabulary (`✓ ✕ ▸ ◷ ⊘ •`). A single muted line under the band carries the task id, `closed`, and a `task →` link; the title above already names it. **No wrapup** (crash, reap, or a stop without one) renders the band loud: `⚠ stopped without a wrapup · 143`, followed by the last message the agent sent, because that is what the operator needs to see.

### 3.4 Last-message line (`.ac-last`)

Rail and stream only, not in the conversation header. Kind glyph, author, compact age, and the first line of the newest message clamped to one line.

### 3.5 Lever row (`.ac-levers`)

Operator-gated. Running: `terminal →` (coopmux), `nudge`, `stop`. Closed: `resume ▶` (respawn with session resume) and the same terminal link when a transcript exists. Same `hx-target="closest .ac-card"` swap the plan card uses.

## 4. Message cards

Each message is one card keyed on its bead id (`id="msg-{id}"`). Author, direction and kind reuse `chatMessage` (`chat_message.go`). Inside a conversation the agent's own messages are attributed as `agent` with an initials avatar and the full name in the tooltip, never the long name on every bubble. Direction bubbles reuse `.bead-chat-msg.dir-from-user` and `.dir-from-agent`.

| Family | Source | Face |
|---|---|---|
| **Squawk** | `type=message`, label `squawk` | Agent bubble. Author, age, markdown body. The bead it auto-commented onto as a chip. |
| **Operator message** | `type=message`, labels `slack-thread-reply`, `slack-mention`, or the new `chat-say`; `operated_by` the human | Mirrored bubble. Delivery facet folded on the age: `· nudged`, `· next turn` (throttled), `· respawning`, `· relayed to Slack`. |
| **Decision** | `type=decision` where requesting agent is this agent | Prompt, options as inline buttons (`POST /b/{id}/decision/resolve`), wait age loud past 14d. Answered: chosen option, who, when. Its report (a `type=report` with `decision_id`) nests under it folded with the tldr visible. |
| **Escalation** | `type=escalation` where requesting agent is this agent | Operation type, sensitivity, targets from `operation_spec` as chips, approve and reject gated on `approve:escalation`, then `result_status` and executor after replay. Loud past 7d. |
| **Report** | `type=report` | `report_type` chip, tldr or first paragraph, rest folded. A `bundle` report links to the bundle page. |
| **Mail** | `type=mail` to or from this agent | `from:` and assignee as chips, body folded past three lines. |
| **Wrapup** | `fields.wrapup` on the agent bead | Rendered as the last message (same face as the closed band, minus the task line) so the conversation reads to its end. |
| **Lifecycle marker** | agent bead events: created, claimed, stopped; task bead closed | One thin centred line, glyph plus text plus age. Not a card, not swapped, not keyed on a bead. |

## 5. Data model

### 5.1 Membership

An agent's conversation is the set of beads that reference it:

- `type=message` and `type=mail` where the agent is `assignee`, `fields.source_agent`, or the `from:` label.
- `type=decision` where `requesting_agent` or `requesting_agent_bead_id` is the agent.
- `type=escalation` where `requesting_agent` is the agent.
- `type=report` whose `decision_id` is one of the decisions above.
- The agent bead itself for the wrapup and lifecycle.
- The assigned task bead (`fields.task_id` or the claimed bead) for the band, and its plan ancestor for the eyebrow.

Membership is computed server-side by a two-wave batched enrich modelled on `planIndexEnrich`: wave one lists agents and the member beads in one filtered query per family; wave two joins tasks, plans, and decision reports. Each join is best-effort and degrades to empty, never to a 5xx. A test pins the daemon call count per page render.

### 5.2 Version

An agent's version is the max `updated_at` across its member set plus its own bead, rendered as an opaque string. It is computed in the same enrich and stamped on the card as `data-version`. The versions endpoint recomputes only the max, not the render.

### 5.3 Liveness

Bead-derived, exactly as `plans_crew.go`: alive `agent_state` and an open bead and a heartbeat within `agentStaleThreshold` (1h). Alive state with a frozen heartbeat renders as stuck. No Kubernetes call from the page path.

## 6. Live updates: invalidate and refetch

The recorded failures of seed-then-stream in this codebase all come from letting the stream carry the card: payload-shaped rows blank out dropped fields, streamed cards skip the enrich joins, and a dropped or late subscription is a missing row. This design removes the cause.

1. **First paint is server-rendered** through the card and message partials.
2. **Events carry an id only.** The SSE handler subscribes once to `BeadFeedHub`, maps each bead event to its owning agent (by the membership rules in §5.1, using a small in-process index refreshed on render), coalesces per agent over 500ms, and emits `agent-changed` with the agent id. A bead event that maps to no agent on any client's page is dropped.
3. **The client refetches.** On `agent-changed` for an agent on screen, the client issues `htmx.ajax('GET', /agents/chat/card/{id})` for the rail card and, if that agent's conversation is open, the conversation partial. Swaps are by element id. **No card is ever built from an event payload.**
4. **Versions poll is the floor.** Every 30s the client sends the visible agent ids and the last version vector; the server answers 204 or the changed subset. Changed agents are refetched. A lost SSE connection, a hub buffer overflow, or the window between paint and subscribe all heal within one poll with no dedup logic in the client.
5. **Reconnect is a poll.** On SSE reconnect the client runs the versions poll immediately. There is no event replay and no `since` cursor on this surface.
6. **Render cache.** Card partials are memoised per agent for 3s keyed on version, so a burst of hints from many tabs collapses into one render, the `feedSeedCache` idea applied per card.

Lifecycle markers are the one exception: they are appended by the conversation refetch, not pushed, so they inherit the same guarantees.

## 7. The write path

`POST /agents/chat/{agent}/say`, operator-gated, body `text` up to `maxCommentLen`.

1. Write one **closed `type=message` bead** assigned to the agent, `created_by` and `operated_by` the operator, label `chat-say`, idempotency key `chat-say:{agent}:{sha256(text)[:16]}:{minute}` so a double submit collapses.
2. **If the agent bead carries `slack_thread_channel` and `slack_thread_ts`**, also stamp those fields and the bridge's `ThreadSayLabel`, and stop. The bridge's `ThreadMessageForward` relays it into Slack and then runs `SpawnOrRespawnForViewerReply`, which nudges a live agent or respawns a terminal one with session resume. This keeps the two front ends one conversation.
3. **Otherwise the viewer does the outcome itself.** Live agent: `bridge.NudgeAgent` with `Chat reply (bead {id}): {text}`, throttled per agent with the bridge's interval. Terminal agent: the existing agent restart path with session resume, then the nudge with boot retry.
4. The response re-renders the conversation partial. The operator's message appears with its delivery facet, and the agent card flips to `spawning` on a respawn.

The composer placeholder reads `Message {agent}` for a live agent and `Message {agent} · sending will resume it` for a closed one, so D7 is never a surprise. A closed agent whose session volume is gone renders the composer disabled with the reason.

## 8. Attention, order, unread

- **Rail and stream order:** attention tiers first, first match wins: open decision, open escalation, stuck, spawning or running, idle, closed. Within a tier oldest-waiting first for the first three, latest activity first for the rest. This is `planAttention` with agent states in place of plan states.
- **Unread:** `unread_listing.js` with `data-last-activity-ts` set to the conversation's newest member timestamp; opening a conversation bumps the high-water mark. A rail card with unread members gets the dot.
- **Nav badge:** `Chat · N` where N is agents with an open decision or escalation across the operator's scope, computed alongside `InboxCount`.
- **Search:** client-side over `data-search` built from agent name, id, role, project, task title, plan title, and the last message, exactly `planCardSearchText`.

## 9. Density grammar (inherited, test-enforced)

Every rule below is lifted from the plan card and gets a served-HTML guard in the new tests.

- Add signal, not rows: ages and qualifiers fold onto the chip that owns them (`· 3d`, `· 143`, `· nudged`).
- Zero-suppression: a chip renders only when it discriminates (no P2 pill, no project chip on a filtered rail, no `by` chip on `scope=mine`).
- Calm then loud: every wait age is muted until its own threshold, then `#a3282a` light / `#ff8a8a` dark. Thresholds: decision 14d, escalation 7d, stuck 1h, live-agent silence 24h.
- Compact on the face, long in the tooltip: `AgoCompact` and truncated text on the face, RFC3339 and full text in `title=`, humanised form in `aria-label`.
- Type scale: name and task title 0.75 to 0.8rem, chips 0.72rem, ids monospace, all numerals `tabular-nums`.
- Clamps: name 2 lines, task title 2 lines, tldr 2 lines, bubbles 3 lines then a fold.
- Byte density: `{{- -}}` trims throughout, a byte-identical-visible test like `plans_card_byte_density_test.go`.
- Dark skins: every hard-coded text colour in the sheet is named by a `html[data-skin^="dark"]` override, structurally checked like `plans_dark_coverage_test.go`.
- Row gap split `column-gap .3rem; row-gap .18rem` on every chip strip.

## 10. Tests

| Test | Guards |
|---|---|
| `agent_chat_index_test.go` | Fake-daemon render of `/agents/chat`: one rail card per agent bead, selected conversation renders, stream view renders the nested fold. |
| `agent_chat_card_faces_test.go` | Running face has the task band and no wrapup band; closed face has the wrapup band with tldr first; missing wrapup renders the loud band. |
| `agent_chat_enrich_calls_test.go` | Daemon call count per page render is fixed regardless of agent count. |
| `agent_chat_density_test.go` | The §9 CSS rules exist verbatim in the served page. |
| `agent_chat_dark_coverage_test.go` | Structural dark override coverage. |
| `agent_chat_events_test.go` | A bead event on a member maps to exactly one `agent-changed` id; bursts coalesce; non-members emit nothing. |
| `agent_chat_versions_test.go` | 204 when unchanged; changed subset otherwise; version moves when any member moves. |
| `agent_chat_say_test.go` | Live agent nudged; Slack-bound agent gets the relay label and no direct nudge; terminal agent respawned; idempotent double submit. |
| `agent_chat_responsive_test.go` | Collapse below 60rem, 44px targets under `pointer: coarse`. |
| `agent_chat_lightbox_test.go` | Every card button that is not a toggle carries the lightbox attributes; `?lb=` round-trips; the decision body renders lineage and options from the fake daemon. |
| Playwright, `tests/beads-viewer-e2e/agent_chat.spec.js` | Open a conversation, answer a decision inline, send a message, see the card update without reload. |

## 11. Mockups

Static HTML with fixture data drawn from live agent names, served locally and captured in Chrome. Files in `assets/agent-chat/`:

| File | What |
|---|---|
| `mockup.html` | The fixture page. `?face=running|closed`, `?view=stream`, `?skin=baseline|dark-terminal`, `?pane=rail|conversation` (phone). Serve the folder with any static server and open it. |
| `phone.html` | Frames `mockup.html` twice at 375px, rail and conversation. |
| `two-pane-running.jpg` | Default view, a running crew agent with an open decision pinned in the task band. |
| `two-pane-closed.jpg` | Same view, a closed agent leading with its wrapup. |
| `stream.jpg` | The nested single-column view. |
| `two-pane-running-dark.jpg` | Dark-terminal skin. |
| `phone.jpg` | 375px rail and conversation side by side. |
| `lightbox-decision.jpg` | The decision lightbox over the running view (`?lightbox=decision`). |
| `plan-cards-zoom.png`, `plans-board-top.jpg`, `existing-activity-feed.jpg`, `existing-deck.jpg` | Reference captures of the surfaces this borrows from. |

Known refinement the mockup does not show: on phone width the conversation header should render the compact rail face, not the full card, so the message list gets the screen.

## 12. Implementation phases (input to the beads plan)

1. **Read-only page.** Routes, enrich, both card faces, message families, rail and conversation partials, stream view, density and dark tests. No SSE, no composer. Ships behind the route only.
2. **Live.** Events endpoint with id-only hints, versions endpoint, client refetch, render cache. Events and versions tests.
3. **Write.** Composer endpoint with the three outcomes, `chat-say` label handling in the bridge forwarder (accept the new label alongside `thread-say`), say tests, Playwright spec.
4. **Attention and unread.** Tiers, nav badge, unread wiring, search.
5. **Promotion decision.** Separate bead, operator's call, as with `/feed`.

## 13. Assumptions stated

- The bridge's `SpawnOrRespawnForViewerReply` is reusable for an agent-keyed message when a Slack thread exists. If it turns out to be thread-shape-specific, phase 3 adds an agent-keyed sibling in the bridge rather than reimplementing respawn in the viewer.
- `fields.task_id` or the claimed-bead pointer identifies the assigned task on every crew agent bead. If some roles lack it, the band falls back to the newest bead the agent has claimed.
- The wrapup JSON shape in `gb/wrapup.go` is stable; the closed face reads it and ignores `custom` keys beyond listing them folded.
