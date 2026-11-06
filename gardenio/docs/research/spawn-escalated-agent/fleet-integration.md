# Fleet integration & telemetry for daemon-spawned local sessions

**Bead:** ka-0000002753 (research child 5 of 9)
**Epic:** ka-0000006791 — client-side `gb` daemon for "spawn escalated agent"
**Sibling inputs:** ka-0000005882 (authn/authz), ka-0000004798 (spawn-action wire), ka-0000006867 (daemon lifecycle)
**Status:** Research-only — no implementation. Synthesis decision lives in epic Phase 2.

---

## TL;DR — Recommendation

A daemon-spawned local session is **a first-class `type=agent` bead in the same
fleet, distinguished by an `execution=local` field — not a separate lane.**
Everything the controller renders about the fleet (`/agents`, `gb news`, the
per-project fleet tile, the recent-runs glyph strip, the Slack agent card) is
already derived *entirely* from `type=agent` beads and their `agent_state`
field. There is no second data source to plumb. So the cheapest, most
consistent integration is to make the local session **write the same agent bead
the cloud pods write** — same fields, same `agent_state` vocabulary, same
lifecycle — and add exactly three small things:

1. **One new field, `execution` (`local` | `cloud`, default `cloud`)** on the
   agent bead, so surfaces can *badge* local runs (a laptop glyph) without
   forking any query or count. The alive/stuck/idle buckets, chip strips, fleet
   tile, and recent-runs strip all keep working unchanged because they key off
   `agent_state`, not execution.

2. **A lifecycle relay path** so the local session's `agent_state` transitions
   reach the daemon and then the controller. The session cannot write kbeads
   directly (it is behind the operator's NAT and is not a trusted controller
   client). Instead the **daemon** — which already holds the authenticated
   websocket from child 2 — owns the agent bead and mints its transitions from
   `daemon.status` frames the local `gb` emits over the loopback UDS.

3. **A `spawn-intent → agent` produces-edge** so the audit bead from the
   spawn-action wire (child 3) links to the live agent bead. This is what makes
   the local run traceable from the originating decision on `/escalations` and
   `/decisions`, and what feeds a per-feature telemetry rollup (spawn count,
   duration, success rate) without a bespoke metrics table.

Why alongside, not a separate lane: a separate "local fleet" view would fork
every query, count, chip, and tile in `agents.go` and `news.go`, double the
template surface, and split the operator's mental model — for a population that
will start at a handful of runs/day. The badge-not-lane model gives operators
the one thing they actually need (know at a glance *this ran on my laptop*)
while inheriting all existing fleet machinery for free. A `?execution=local`
**filter** (same shape as the existing `?project=` / `?role=` / `?state=`
facets) covers the "show me only local runs" case without a forked page.

---

## 1. How the fleet is populated today (the ground truth)

Read before proposing anything: the fleet is **100% bead-derived**. There is no
separate agent registry, no heartbeat table, no RPC the controller polls. Every
fleet surface reads `type=agent` beads:

### 1.1 `/agents` (beads-viewer)

`gardenio/controller/cmd/beads-viewer/agents.go` issues **two** kbeads list
queries (`agents.go:1416`):

```go
active := ListBeadsFiltered(ListBeadsQuery{
    Types:    []string{"agent"},
    Statuses: []string{"open", "in_progress"},
    Sort:     "updated_at", Limit: agentsActiveLimit /* 100 */,
})
recent := ListBeadsFiltered(ListBeadsQuery{
    Types:    []string{"agent"},
    Statuses: []string{"closed"},
    Sort:     "updated_at", Limit: agentsRecentLimit /* 200 */,
})
```

From those two slices it derives *everything*:

- **Alive/stuck/idle buckets** via `agentStateBucket()` (`agents.go:328`), which
  classifies the `agent_state` field:
  - `agentStateAlive = {working, spawning, yielding}` (`agents.go:696`)
  - `agentStateStuck = {failed, rate_limited, budget_exceeded}`
  - everything else (incl. empty) → idle
- **Per-project fleet tile** (`buildProjectFleetTile`, `agents.go:705`) — same
  alive/stuck/idle/total counts scoped to `?project=<name>`.
- **Role / state / project chip strips** (`buildSecondaryChips`, `agents.go:789`)
  — all pure reductions over the same `active` slice.
- **Recent-runs glyph strip** (`buildRecentRuns`, `agents.go:524`) — walks the
  window-trimmed `closed` agents newest-first, emits up to `recentRunsLimit`
  (20) glyphs, outcome from the `wrapup` blockers field (`recentRunOutcome`,
  `agents.go:672`): `ok | fail | platform | unknown`.
- **Prewarmed-pool filtering** (`isUnengagedPrewarmedAgent`, `agents.go:366`) —
  drops warm-standby pods before classification.

Fields read off the agent bead (`agents.go:1859`+): `agent`, `role`,
`agent_state`, `pod_phase`, `task_id`, `task_title`, `spawned_by`,
`schedule_id`, `last_progress_at`, `wrapup`, `slack_thread_channel`/`_ts`,
`last_session_id`, `mr_url`/`mr_state`/`mr_iid` (via produces-graph walk).

### 1.2 `gb news`

`gardenio/controller/cmd/gb/news.go:131` runs the same shape — list `type=agent`
`in_progress` + `closed`, filtered by `project:<name>` label — plus `decision`
and `report` lists. It is another pure reduction over agent beads.

### 1.3 Slack agent card + lifecycle notifications

`gardenio/controller/internal/bridge/agents.go` subscribes to the **kbeads SSE
event stream** (`Agents.RegisterHandlers`, `bridge/agents.go:112`) and reacts to
agent-bead `created` / `updated` / `closed` events:

- `handleCreated` → spawn notification / card.
- `handleUpdated` → `agent_state` change → `NotifyAgentState` → card refresh.
- `handleClosed` → crash vs. clean-done disambiguation from `agent_state`
  (`done` = clean; `failed`/`pod_phase=failed` = crash, minus reaper cleanups
  via `isReaperCleanup`).

**Takeaway:** the entire fleet UX is a *view* over agent beads streamed through
kbeads SSE. If a local session's agent bead is written with the same fields and
transitions, it appears everywhere — `/agents`, the tile, the strip, `gb news`,
and the Slack card — with **zero new rendering code**. The only real work is
(a) *who writes that bead* for a NAT'd laptop process, and (b) *how surfaces
badge it* as local.

---

## 2. The core problem: a local session can't write kbeads directly

A cloud crew pod writes its own agent bead because it runs inside the cluster
with kbeads reachable and a gate identity (`KD_AGENT_ID`/`KD_ACTOR`, injected by
podmanager, `manager.go:1815`). A daemon-spawned **local** Claude Code session
has neither:

- It is on the operator's laptop, behind NAT — it cannot reach kbeads' gRPC/HTTP
  endpoint, and we explicitly do **not** want to (child 2: the daemon holds the
  *only* authenticated channel; the session is untrusted).
- It has no controller-issued gate identity of its own.

So the agent bead for a local session must be **owned and written by the
daemon**, which already:

- holds the long-lived authenticated websocket to the controller (child 2,
  Layer 1),
- received the signed spawn payload (child 3) carrying `decision_id`,
  `audit_bead_id`, working dir, ref, permissions,
- supervises the local `coop`/Claude Code process lifecycle (child 1).

The daemon is the natural **lifecycle authority** for the local session. The
local `gb` inside the session reports *up* to the daemon over loopback; the
daemon reports *up* to the controller over the websocket; the controller (or the
daemon acting through it) writes the agent bead. This is the same trust
gradient child 2 already established — nothing new.

---

## 3. Lifecycle wire diagram

```
┌────────────────────────── operator laptop ──────────────────────────┐
│                                                                      │
│  ┌───────────────┐   loopback UDS      ┌──────────────────────────┐  │
│  │ Local session │  gb daemon.status   │        gb daemon         │  │
│  │ (Claude Code  │ ──────────────────► │  (child 1 process model, │  │
│  │  + local gb)  │   {agent_state,     │   child 2 ws client)     │  │
│  │               │    last_progress,   │                          │  │
│  │  gb squawk    │    wrapup, mr_url}   │   owns the agent bead    │  │
│  │  gb stop      │                     │   for this local session │  │
│  └───────────────┘                     └────────────┬─────────────┘  │
│                                                      │                │
└──────────────────────────────────────────────────────┼───────────────┘
                                                        │ authenticated
                                                        │ websocket (child 2)
                                                        │ daemon.status frames
                                                        ▼
                                    ┌──────────────────────────────────┐
                                    │           controller             │
                                    │  ws handler → beadsapi Upsert     │
                                    │  type=agent bead                  │
                                    │  execution=local                  │
                                    └────────────┬─────────────────────┘
                                                 │ writes / updates
                                                 ▼
                                    ┌──────────────────────────────────┐
                                    │             kbeads                │
                                    │  type=agent bead (the record)     │
                                    │  emits SSE created/updated/closed │
                                    └────────────┬─────────────────────┘
                                                 │ SSE stream
                          ┌──────────────────────┼───────────────────────┐
                          ▼                       ▼                       ▼
                    ┌───────────┐          ┌────────────┐          ┌──────────────┐
                    │ /agents   │          │  gb news   │          │ bridge →     │
                    │ tile/strip│          │            │          │ Slack card   │
                    └───────────┘          └────────────┘          └──────────────┘
```

### 3.1 State transitions (mapped to the existing vocabulary)

The local session **reuses the existing `agent_state` values** so no bucket,
chip, tile, or glyph needs a new case. Full existing vocabulary
(`agents.go:692`): `idle, spawning, working, yielding, done, failed,
rate_limited, budget_exceeded` (+ pool-only `prewarmed`/`assigning`).

| Local event | `agent_state` written | Who emits | Controller action |
|---|---|---|---|
| Operator clicks "spawn escalated" | `spawning` | daemon (on receipt of signed payload, **before** launch) | Upsert agent bead `open`, `execution=local`, link produces-edge from spawn-intent |
| Claude Code process up, first turn | `working` | daemon (from first `daemon.status` frame the local `gb` emits) | Update bead → SSE → card flips to working |
| Progress (squawk / turn boundary) | `working` + `last_progress_at` bump | local `gb` → daemon | Update `last_progress_at` (drives the stale-badge heuristic) |
| `gb yield` (waiting on operator input in-session) | `yielding` | local `gb` → daemon | Update → still counts alive |
| Rate-limit / budget hit locally | `rate_limited` / `budget_exceeded` | local `gb` or daemon | Update → stuck bucket |
| `gb stop` with clean wrapup | `done` + `wrapup` | local `gb` → daemon | Close bead `closed`, `agent_state=done` → recent-runs `ok`/`fail` from wrapup |
| Session crash / non-zero exit / operator kills it | `failed` + `close_reason` | **daemon** (process supervisor detects exit without a `done`) | Close bead `failed` → bridge crash notification (unless reaper-style) |
| Daemon dies / laptop sleeps mid-run | `failed` after heartbeat-miss grace | **controller reaper** (see §3.3) | Close bead `failed`, `close_reason=daemon-heartbeat-lost` |

The mapping is deliberately 1:1 with cloud agents. A local run that finishes
clean is indistinguishable in the buckets from a cloud run that finishes clean —
by design. The `execution=local` badge is the *only* visible difference.

### 3.2 `gb stop` from inside the local session

This is a called-out question in the task. Today `gb stop` (child `stop.go`)
runs the decision-gate + wrapup-validation flow, then closes the agent bead and
signals the entrypoint not to restart the pod. For a local session:

- The **wrapup/decision-gate/ask-gate logic is unchanged** — it runs locally in
  the session's `gb` binary exactly as it does in a pod. The operator still gets
  the ask-gate nudge, still must file an escalation/decision for an operator-ask,
  etc. (These gates are pure client-side logic over the wrapup JSON.)
- The difference is the **terminal side effect**. A pod's `gb stop` writes the
  closed bead directly and the entrypoint reads a stop-signal. A local session's
  `gb stop` instead emits a final `daemon.shutdown` frame (agent_state `done`,
  wrapup attached) to the daemon over the UDS. **The daemon** closes the agent
  bead over the websocket and tears down the local process. There is **no
  auto-restart** for local sessions — the "pod will not restart" semantic maps
  to "the daemon does not relaunch Claude Code"; the operator re-invokes the
  spawn action if they want it back. (This matches the despawn-class advice: a
  local operator-driven session is inherently terminal.)
- **Offline daemon at stop time** (edge case): if the local `gb stop` can't
  reach the daemon (daemon crashed), it writes a local journal entry the daemon
  reconciles on next start, and prints the wrapup to the operator so nothing is
  lost. The controller reaper (§3.3) will have already flipped the bead to
  `failed` on heartbeat-loss; the daemon reconciliation posts the real wrapup as
  a comment and corrects the terminal state to `done` if the journal shows a
  clean stop.

### 3.3 Liveness & the heartbeat reaper

A laptop is far less reliable than a pod — it sleeps, loses wifi, gets closed.
We must not leave zombie `working` local agents in the alive bucket forever.

- The daemon sends `daemon.heartbeat` frames every 30s over the websocket
  (child 2 already specifies these for socket-keepalive). Piggyback per-session
  liveness: each heartbeat carries the set of live local `session_id`s.
- The controller runs a **local-agent reaper**: any `execution=local` agent bead
  in an alive `agent_state` whose owning daemon has missed heartbeats for
  `> localAgentGracePeriod` (proposed **90s**, 3 missed beats) is closed
  `failed` with `close_reason=daemon-heartbeat-lost`. This is the same
  reaper-cleanup class `isReaperCleanup` (`bridge/agents.go:30`) already
  suppresses from crash-paging — so a laptop lid-close does **not** page the
  operator; it just greys the card.
- On daemon reconnect, if the session is actually still alive (laptop woke, run
  continued), the daemon re-asserts it: reopen is not needed — the daemon
  Upserts a fresh agent bead continuation, or (simpler) the grace period is set
  long enough (90s) that a brief network blip never trips it. **Design
  synthesis should pick** between "long grace, no reopen" (simpler) and "reopen
  on reconnect" (more forgiving) — recommendation: **long grace, no reopen**,
  because a local run that genuinely survives a 90s network gap is rare and the
  operator is right there to re-spawn.

---

## 4. UI placement proposal

### 4.1 Principle: badge, don't fork

Local runs sit in the **same** `/agents` page, the **same** `gb news`, the
**same** fleet tile. The only additions:

1. **A `local` glyph** (🖥 / laptop) on the agent card and the recent-runs
   strip glyph, driven by `execution=local`.
2. **An `execution` facet** in the existing chip strip
   (`buildSecondaryChips`), giving `All · cloud · local` alongside the existing
   role and state facets. Clicking `local` sets `?execution=local` — a filter,
   not a separate page.
3. **The card's "pod" column** (which shows the K8s pod name for cloud agents)
   renders **`laptop · <operator>`** for local agents, since there is no pod.
   The existing compact-table column header (`name | role | state | pod | task
   | id | up | active`, `agents.go:81`) is unchanged; only the cell content
   differs.

### 4.2 Agent card — cloud vs local (ASCII mock)

Cloud agent card (unchanged):

```
┌────────────────────────────────────────────────────────────┐
│ ● working   gardenio-research-fleet-integration-3ry   crew   │
│   pod: crew-abc123  ·  task: ka-0000002753  ·  up 4m         │
│   last progress 12s ago                                     │
└────────────────────────────────────────────────────────────┘
```

Local agent card (new — note the 🖥 badge and the pod cell):

```
┌────────────────────────────────────────────────────────────┐
│ ● working  🖥 local   matt@laptop / spawn-esc   crew        │
│   host: laptop · matt  ·  task: ka-DurS02…  ·  up 4m         │
│   spawned from decision ka-0000004126 · last progress 8s ago│
└────────────────────────────────────────────────────────────┘
```

The `🖥 local` badge sits immediately after the state dot so it reads in the
same scan as `working`. The "spawned from decision …" line is populated from
the produces-edge back to the spawn-intent/decision bead (§5), giving the
operator one-click traceback to the originating escalation.

### 4.3 Recent-runs glyph strip

The strip (`buildRecentRuns`) is untouched structurally; each glyph for an
`execution=local` run gets a **small laptop corner badge**, exactly the way MR
state already draws a secondary corner badge (`decorateRecentRunsWithMRState`,
`agents.go:589` — reuse that exact mechanism). Outcome glyph
(ok ✓ / fail ✗ / platform ⚠ / unknown ·) is unchanged.

```
Recent runs:  ✓ ✓ ✗ ✓ [✓🖥] ✓ [✗🖥] ✓ ✓ · ✓ ...
                        ↑ local run, clean   ↑ local run, blocked
```

### 4.4 Fleet tile

The per-project fleet tile (`?project=gardenio`) keeps its alive/stuck/idle/total
counts. Add **one line** under the totals: `N local` (count of
`execution=local` among active), so an operator scanning the tile sees local
participation without drilling in. No new query — it is a filter over the
`active` slice already in hand.

```
┌──────────── gardenio fleet ────────────┐
│  alive 9   stuck 1   idle 3   total 13 │
│  ▁▂▄▆▅▃ (6h)              2 local 🖥    │  ← new line
└────────────────────────────────────────┘
```

### 4.5 `gb news`

`gb news` gets a `🖥` prefix on local agent rows in the in-progress section, and
a one-line footer `N local sessions active` when any are present. No query
change — it already lists `type=agent` `in_progress`; it just reads the new
`execution` field for the prefix.

---

## 5. Traceability: spawn-intent → agent produces-edge

Child 3 (spawn-action wire) creates a **`type=spawn-intent` audit bead** as a
child of the decision, carrying `state: spawning` and the forwarded payload.
This child owns the seam to the live agent bead:

- When the daemon Upserts the local agent bead, it writes a **`produces` edge**
  from the spawn-intent bead to the agent bead (same edge type the fleet already
  walks for MRs, `decorateRecentRunsWithMRState`).
- This makes the chain queryable end-to-end:
  `decision → spawn-intent → agent(local) → [MR produced]`.
- On `/escalations` and `/decisions`, the decision row can show a live "spawned
  locally · working" chip that hot-links to the agent card — the operator sees,
  from the escalation surface, that their local spawn is alive and progressing.
- The spawn-intent bead's `state` field is updated in lockstep with the agent
  bead's `agent_state` (daemon writes both from the same frame), so the audit
  bead is a durable, queryable record even after the agent bead ages out of the
  6h recent window.

---

## 6. Telemetry schema (evaluate the feature)

The task asks: *what telemetry does the controller need (spawn count, duration,
success rate) to evaluate the feature?* Because everything is bead-derived, the
telemetry is a **query over agent beads with `execution=local`**, not a new
metrics pipeline. Required fields on the agent bead (all either existing or the
one new `execution` field):

| Metric | Derivation | Source fields |
|---|---|---|
| **Spawn count** | count of `execution=local` agent beads created in window | `execution`, `created_at` |
| **Active local now** | alive-bucket count where `execution=local` | `execution`, `agent_state` |
| **Duration (per run)** | `closed_at − created_at` (or `updated_at − created_at` for the last transition) | timestamps |
| **Success rate** | `done` / (`done` + `failed`), excluding reaper-cleanup closes | `agent_state`, `close_reason` |
| **Time-to-first-turn** | first `working` transition − `created_at` (spawn latency) | `spawning`→`working` timestamps |
| **Crash rate** | `failed` non-reaper / total closed | `agent_state`, `close_reason` |
| **Heartbeat-loss rate** | `close_reason=daemon-heartbeat-lost` / total | `close_reason` |
| **Spawn→decision linkage** | fraction of local runs with a produces-edge from a spawn-intent | edge presence |
| **MR yield** | local runs that produced a merged MR | produces-graph walk (existing) |

**Where it surfaces:** a small **"Local sessions (7d)"** rollup block on
`/agents?execution=local` (mirror the existing `Last24h` summary block,
`agents.go` `agentsHistoryWindow`) with: spawns, success rate, median duration,
median time-to-first-turn, crash/heartbeat-loss counts. Because the block is a
reduction over the same two list queries (widen the window for the rollup), it
needs **no new controller endpoint** — it is the `Last24h` pattern parameterized
by `execution=local` and a 7d window.

**Two-independent-derivations check** (per advice ka-0000005924): spawn count
should reconcile two ways — (a) count of `execution=local` agent beads, and
(b) count of `type=spawn-intent` beads that reached `state != spawning`. A
persistent divergence between the two means a spawn payload was forwarded but no
agent bead was ever written (daemon dropped it) — a real bug worth surfacing on
the rollup as a "unlinked spawns" counter.

---

## 7. What actually needs to be built (scope handoff to Phase 3)

None of this is implemented here (out of scope). For the synthesis decision, the
concrete deltas are small and enumerated so the estimate is honest:

1. **kbeads type-config**: add `execution` to the `type:agent` allowed fields.
   ⚠️ Per advice ka-0000000061, prod gardenio has a **stored** `type:agent`
   config row shadowing `builtinConfigs` — the field must be added to the
   **live stored config**, not just the Go source, or writes silently drop it.
   Verify against the live daemon.
2. **Daemon → controller ws frames**: `daemon.spawn-ack`, `daemon.status`,
   `daemon.heartbeat` (with live session set), `daemon.shutdown` — carrying the
   agent-bead field payload. (Child 2 already defines the frame *envelope*; this
   defines the agent-lifecycle *message bodies*.)
3. **Controller ws handler → beadsapi Upsert**: write/close the agent bead from
   daemon frames; write the produces-edge from the spawn-intent bead.
4. **Controller local-agent reaper**: heartbeat-loss → `failed` +
   `daemon-heartbeat-lost`, classified as reaper-cleanup (no page).
5. **beads-viewer**: `execution` facet chip + `?execution=local` filter; `🖥`
   badge on card, recent-runs glyph (reuse the MR-corner-badge mechanism), and
   the pod-cell → `host: laptop · <operator>` swap; `N local` line on the fleet
   tile; the "Local sessions (7d)" rollup block.
6. **`gb news`**: `🖥` row prefix + `N local sessions active` footer.
7. **local `gb stop`**: emit `daemon.shutdown` over UDS instead of writing the
   closed bead directly; offline-daemon journal fallback.

Everything else — buckets, chips, tile counts, SSE-driven card refresh, Slack
crash disambiguation, `gb news` structure — is **inherited unchanged** because
the local agent bead is just another `type=agent` bead.

---

## 8. Answers to the task's specific questions

- **Does a local session register as a `gb` agent with the controller?** Yes —
  as a `type=agent` bead, but **written by the daemon**, not the session
  (the session is NAT'd and untrusted). Same bead shape as cloud agents.
- **Pushed or polled?** Pushed. The daemon pushes `daemon.status` frames over
  the child-2 websocket; the controller Upserts the bead; kbeads SSE fans out to
  every surface. No polling of the laptop.
- **Alongside cloud agents or a separate local lane?** Alongside, badged with
  `execution=local` and filterable via `?execution=local`. A separate lane would
  fork every query/count/tile for a tiny population — rejected.
- **How do lifecycle events flow (spawn/idle/despawn/crash)?** §3.1 table.
  Spawn = daemon writes `spawning` on payload receipt; running = `working` from
  first session frame; despawn = `done` via `gb stop`→`daemon.shutdown`; crash =
  `failed` from the daemon's process supervisor, or `daemon-heartbeat-lost` from
  the controller reaper if the whole laptop drops.
- **How does `gb stop` interact with the controller's normal flow?** §3.2 —
  gates/wrapup logic unchanged; terminal side-effect goes daemon→controller
  instead of pod→entrypoint; no auto-restart (local sessions are terminal).
- **Telemetry the controller needs?** §6 — all bead-derived; spawn count,
  duration, success rate, time-to-first-turn, crash/heartbeat-loss, spawn→
  decision linkage, MR yield. Rollup is the existing `Last24h` block
  parameterized by `execution=local`.
- **How does the recent-runs strip / fleet tile look for local agents?** §4.3–
  4.4 — recent-runs glyph gets a laptop corner badge (reusing the MR-state
  corner-badge mechanism); fleet tile gets a `N local 🖥` line. No structural
  change.

---

## 9. Open questions for design synthesis

1. **Long-grace vs reopen-on-reconnect** for the heartbeat reaper (§3.3).
   Recommendation: long grace (90s), no reopen.
2. **Does a local session get a Slack thread/card at all,** or only the
   `/agents` presence? Recommendation: yes — reuse the existing card so the
   operator's own local run posts progress the same way cloud crew do; the
   bridge already keys off the agent bead, so it is free. Gate behind an
   operator preference if the noise is unwanted.
3. **`execution` value space** — `local` | `cloud` is enough today, but should
   we reserve `remote` for future cloud-agent-tool "remote" runs so the field
   is a general execution-substrate discriminator? Recommendation: yes, make it
   an open enum defaulting to `cloud`.
4. **Prewarm/pool semantics** — local sessions never pool; confirm the
   prewarmed filters (`isUnengagedPrewarmedAgent`) never false-match a local
   agent (they key on the `prewarmed-` name prefix, so they won't — but assert
   it in a test).
```
