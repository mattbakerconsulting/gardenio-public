# Plan: Unified Slack Presence — Streaming for Every Agent

**Date**: 2026-04-15
**Supersedes**: `design-unified-slack-presence.md` (origin/design/unified-slack-presence)
**Decision**: ka-0000007741 — "Keep + unify, but figure out how we ended up
with 12k lines of code... can we simplify without losing features?"

---

## TL;DR

1. **Keep** the Slack streaming API. Matt confirmed it works in regular
   channels today, so the MR !1018 design risk is disproven.
2. **Unify**: every agent gets a card + a stream. The `cardOnly` split
   disappears. Purple shimmer everywhere, under every card.
3. **Simplify**: audit the 11,974 LOC in `bridge/bot_*.go` and delete the
   duplicated presence code, the thread-specific feature gates, and the
   parallel state maps — **without losing any user-visible features**.

Net effect target: **−2,500 to −3,500 production LOC**, same features,
cleaner architecture.

---

## Where the 12k Came From: `bot_*.go` Audit

`find bridge/ -name 'bot_*.go' -not -name '*_test.go' | xargs wc -l` =
**11,974 LOC** across 20 files. Here's the breakdown and why each grew:

| File | LOC | Why it's big | Simplifiable? |
|---|---:|---|---|
| `bot_agents.go` | 1,561 | Card create/refresh + debounce + compression + dashboard aggregation + 3 threading modes | **−400** (kill triple-branch, cardOnly session) |
| `bot_mentions.go` | 1,392 | DM vs app_mention vs thread reply vs assistant thread — 4 dispatch paths | **−300** (merge dispatch, one presence lookup) |
| `bot_commands.go` | 1,235 | `/spawn`, `/kill`, `/kill-thread`, `/status`, `/unrelease`, `/release`, `/schedule` — each with its own threading branch | **−350** (merge `/kill` + `/kill-thread`; one routing lookup) |
| `bot_decisions.go` | 773 | Decision rendering + interaction + resolution + persistence | −50 (route via presence) |
| `bot_notifications.go` | 757 | `resolveThreadedChannel` triple-branch called by every Notify* | **−300** (one presence lookup replaces all branching) |
| `bot_decisions_modal.go` | 725 | Modal UI for decision submission | −0 (leave alone) |
| `bot_formula.go` | 690 | Formula preview/instantiation UI | −0 (leave alone) |
| `bot_agent_kill.go` | 554 | Kill path: stream stop + card compression + threading cleanup | **−200** (one kill path, not two) |
| `bot_channels.go` | 523 | Channel routing cache (project → channel, schedule → channel, concierge) | −50 (merge three caches) |
| `bot_escalations.go` | 449 | Escalation card rendering + threading branch | **−100** (route via presence) |
| `bot_schedule_cmd.go` | 410 | `/schedule` command surface | −0 |
| `bot_beadactivity.go` | 300 | Inline bead activity ("agent claimed X") in threads only | **−80** (unify: always post under card) |
| `bot_assistant.go` | 282 | Assistant-thread context tracking (`assistantCtxChannel` map) | **−150** (goes away in unified model) |
| `bot_reactions.go` | 281 | Slack reactions handling | −0 |
| `bot_concierge.go` | 275 | Concierge channel routing | −40 (merge into unified routing) |
| `bot_threads.go` | 247 | Thread utilities | −50 (subsumed by presence) |
| `bot_unreleased.go` | 224 | Unreleased-version detection | −0 |
| `bot_permission.go` | 222 | Permission elicitation handling | −0 |
| `bot_thread_forward.go` | 220 | Human reply → agent nudge (thread-bound only!) | **+50, −200** (extend to all agents, delete thread-gating code) |
| `bot_schedule_preview.go` etc | ~50 | Tiny helpers | −0 |

**Simplifiable total: ~2,220 LOC**, plus ~300 in `bot.go` (the 11 parallel
maps) and ~200 in `agentstream.go` (cardOnly branch). Call it **−2,500 to
−3,500 production LOC** for the whole migration.

---

## Root Causes of the Sprawl

### 1. Triple-branch routing, copy-pasted
Every notification function (decision, escalation, crash, bead activity,
permission, etc.) implements the same 3-way routing:
```go
if threadBound { return threadChannel, threadTS }
if cardMode { return channel, cardTS }
return channel, ""  // flat
```
That's **~15 callsites × 10 lines = 150 lines of pure duplication**, and
each callsite has its own subtle variation (null checks, fallbacks,
debounce interaction). The cognitive cost is much higher than the LOC.

### 2. Feature gates on threading mode
Thread-bound agents get features that card-mode agents don't:
- Reply forwarding (`bot_thread_forward.go`)
- Inline bead activity in the thread (`bot_beadactivity.go`)
- Live streaming timeline (`agentstream.go` StartSession)
- Assistant thread context (`bot_assistant.go`)

Each gate requires `if b.isThreadBound(agent) { ... }` branches scattered
across the codebase. Every new feature asks "does this apply to both
modes?" and the answer is usually "sort of, but..."

### 3. Parallel state maps
`Bot` struct has **15 maps keyed by agent identity** (11 for agent state
+ 4 for threading/caching). They all update in lockstep during spawn,
but each has its own lifecycle rules, locking discipline, and persistence
path. When a new attribute is added, a new map is added — never a struct
field — because the existing maps are hard to refactor.

### 4. Two session types in AgentStreamManager
`StartSession` (real streaming) vs `StartCardSession` (tool count only)
diverge through the entire flush/stop path with `if cardOnly` branches.
This is ~200 LOC of duplicated logic that only exists because streaming
was assumed to be assistant-thread-only.

### 5. Command duplication
`/kill` and `/kill-thread` do almost the same thing with different
threading cleanup. Same with spawn routing in DMs vs channels vs threads.

---

## Target Architecture

### `AgentPresence` — one struct, replaces 15 maps

```go
type AgentLocation struct {
    Channel  string
    ThreadTS string  // empty = top-level; set = reply in thread
}

type AgentPresence struct {
    // Location
    Location      AgentLocation
    SpawnChannel  string  // where /spawn was issued (may differ from Location)

    // Slack message refs
    CardTS        string  // the agent card (always exists)
    StreamTS      string  // streaming message under the card (always exists)

    // Agent metadata
    PodName       string
    ImageTag      string
    Role          string
    Project       string
    ScheduleTitle string

    // State
    State         string    // spawning/working/done/failed
    Pending       int       // open decision count
    SpawnedAt     time.Time
    Seen          time.Time

    // Card debounce
    LastUpdate time.Time
    Dirty      bool
}
```

Single `agentPresence map[string]*AgentPresence` replaces the 15 maps.
`StateManager` persists this struct directly (JSON).

### One notification path

```go
// Before: every Notify* calls resolveThreadedChannel and branches
func (b *Bot) NotifyDecision(ctx context.Context, agent string, dec *Decision) {
    channel, threadTS := b.resolveThreadedChannel(ctx, agent)  // triple-branch
    // ... post decision ...
}

// After: single presence lookup
func (b *Bot) NotifyDecision(ctx context.Context, agent string, dec *Decision) {
    p := b.presence(agent)
    // p.Location.Channel, p.CardTS — always set
    // ... post decision under p.CardTS ...
}
```

Every notification threads under `p.CardTS`. Period.

### One streaming path

`StartCardSession` deleted. Every agent's streaming message is posted as
a reply under its card:

```go
// On agent spawn:
card := b.ensureAgentCard(ctx, agent, presence.Location)       // always
stream := b.streamMgr.StartSession(ctx, agent, presence.Location.Channel,
                                    card.TS, userID)           // always
```

The `cardOnly` boolean in `agentStreamSession` is deleted. The flush loop
always appends structured chunks. The `UnsafeMsgOptionEndpoint` shim stays
(it's the only way to inject `task_display_mode` and `chunks`), but it
becomes the one streaming path rather than a conditional branch.

### One kill path

`/kill-thread` deleted. `/kill <agent>` uses the presence lookup to:
1. Stop the stream under the card
2. Compress the card to single-line format
3. Post final status message in the card's thread
4. Remove `presence[agent]`

Same code regardless of whether the agent was spawned from a thread or
from a slash command.

---

## Migration Plan — 6 Phases (reduced from 7)

Each phase is independently mergeable and independently revertible.

### Phase 1 — Introduce `AgentPresence` (shadow mode) — ~300 LOC

- Add `AgentPresence` struct + `agentPresence` map on `Bot`
- On spawn/claim/close, populate `agentPresence[agent]` alongside the
  existing 15 maps
- Add validation log: detect mismatches between presence and the maps
- No behavior change
- **Risk**: very low

### Phase 2 — Route notifications through presence — ~400 LOC net reduction

- Replace `resolveThreadedChannel` with `b.presence(agent).Location` lookup
  in `bot_notifications.go`, `bot_decisions.go`, `bot_escalations.go`,
  `bot_beadactivity.go`, `bot_permission.go`
- Delete the `resolveThreadedChannel` function
- Keep the 15 maps updating in parallel (safety net)
- **Risk**: medium — one function replaces many callers. Mitigation:
  exhaustive callsite search + E2E slack-threads test

### Phase 3 — Unify streaming; every agent gets a real stream — ~200 LOC net reduction

- Delete `StartCardSession`; all agents use `StartSession`
- Delete `cardOnly` flag in `agentStreamSession`
- On spawn, always `StartSession` with `card.TS` as the parent
- Card-mode agents now get the purple-shimmer timeline
- **Risk**: medium — Matt confirmed streaming works outside assistant
  threads, so no rollback plan needed. Canary: enable for one
  channel first via feature flag, verify shimmer renders correctly.

### Phase 4 — Thread-bound agents get full cards — ~150 LOC net reduction

- When spawning from a thread, post the agent card as a reply under the
  thread's parent (or under the spawn message)
- Stream goes under the card (per Phase 3)
- Delete `threadSpawnMsgs` map
- Delete the `bot_assistant.go` assistant-thread-context tracking
  (presence handles it)
- **Risk**: medium — changes the Slack UX for thread users.
  Mitigation: feature flag, roll out per-workspace.

### Phase 5 — Collapse state maps + enable reply forwarding for all — ~700 LOC net reduction

- Remove the 11 `agent*` maps from `Bot`; all reads/writes go through
  `agentPresence`
- Update `StateManager` to persist `AgentPresence` directly
- Extend `bot_thread_forward.go` to match any agent whose card thread
  receives a reply (not just thread-bound)
- Delete the `agentThreadBound` check
- **Risk**: low — pure refactor once Phases 1-4 are stable

### Phase 6 — Merge `/kill` + `/kill-thread`; merge channel caches — ~300 LOC net reduction

- `/kill <agent>` auto-detects presence; does the right cleanup
- Delete `/kill-thread` command handler (keep the command name as alias
  for one release, then remove)
- Merge `projectChannelCache`, `scheduleChannelCache`,
  `conciergeChannelCache` into one cache with a typed key
- **Risk**: low — cleanup phase

---

## Feature Preservation Matrix

| Feature | Today | After unification |
|---|---|---|
| Thread-bound agent: live tool timeline | ✅ (purple shimmer) | ✅ (same) |
| Card-mode agent: tool stats on card | ✅ (chat.update) | ✅ + live shimmer under card |
| Flat agent: top-level message per event | ✅ | ✅ (now with a card on top — opt-out via config) |
| Human replies → agent nudge | ✅ thread-bound only | ✅ any agent |
| Decision modals | ✅ | ✅ (identical) |
| Dashboard aggregation | ✅ | ✅ (cleaner — one source) |
| Card compression on stale | ✅ | ✅ (extended to all) |
| Crash/error notifications | ✅ | ✅ (threaded under card) |
| `/kill` + `/kill-thread` | ✅ (two commands) | ✅ (one command) |
| Survive bridge restart | ✅ cards only | ✅ full presence |
| Inline bead activity | ✅ thread-bound only | ✅ any agent |
| Permission elicitation | ✅ | ✅ (threaded under card) |

**No features lost. Three features gained**: reply forwarding, inline
bead activity, and live timeline for non-thread agents.

---

## Risks and Mitigations

1. **Always-post-card noise in busy channels**
   - Mitigation: card compression already handles this; one-line cards
     collapse quickly. Add per-channel config to suppress cards if needed.

2. **Replying in any card's thread now nudges the agent**
   - Mitigation: stays behind a channel-level feature flag until Phase 6;
     bot adds a :eyes: reaction when it forwards so users see it happened.

3. **Phase 3 streaming rollout**
   - Mitigation: Matt confirms streaming works in regular channels.
     Canary one channel, verify, then enable globally.

4. **Persistence format change**
   - Mitigation: `StateManager` supports dual-read (old maps + new
     presence) during Phases 1-4; cut over in Phase 5.

---

## What This Plan Doesn't Do

- **Doesn't touch** Slack API usage patterns — same endpoints, same rate
  limits, same retry logic
- **Doesn't touch** bead schema — `slack_thread_channel` / `slack_thread_ts`
  are read from agent beads exactly as today
- **Doesn't touch** `bot_decisions_modal.go`, `bot_formula.go`,
  `bot_permission.go`, `bot_reactions.go`, `bot_unreleased.go` — these
  are presence-agnostic and already clean
- **Doesn't remove** the streaming API (Matt's correction noted)

---

## Estimated Work

- Phase 1: ~0.5 day — struct + dual-write
- Phase 2: ~1 day — 15 callsite migration + test
- Phase 3: ~1 day — streaming unification + canary verification
- Phase 4: ~1 day — thread-bound cards
- Phase 5: ~1.5 days — map deletion + forwarding extension
- Phase 6: ~0.5 day — command merge + cache merge

**Total: ~5.5 days**, each phase shippable independently.

---

## Decision

Approve this plan → Phase 1 begins on the feature branch
`design/unified-presence-streaming-every-agent`.

Revise → feedback on what to change before Phase 1 starts.
