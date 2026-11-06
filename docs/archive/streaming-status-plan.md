# Streaming Agent Status via Slack Chat Streaming API

## Summary

Use Slack's `chat.startStream` / `chat.appendStream` / `chat.stopStream` APIs to show
live, streaming agent status updates in Slack assistant threads. Instead of static
"Thinking..." status indicators and periodic dashboard polls, agents stream real-time
progress directly into the conversation — tool use counts that increment, task
transitions, and eventually richer operational telemetry.

## Background

### What We Have Today

1. **Assistant thread status** (`bot_assistant.go`): Sets a static "Thinking..." loading
   indicator via `assistant.threads.setStatus`. Cleared when the response is ready.
   Shows rotating hint text but no real progress.

2. **Dashboard** (`dashboard.go`): A single pinned message updated every 15s showing
   the agent roster. Content-hashed to avoid redundant API calls. Good for fleet view,
   but not per-agent session detail.

3. **Hook relay** (`hook_relay.go`): Every Claude Code hook event (PreToolUse,
   PostToolUse, Stop, etc.) is published to NATS via `hooks.<agent>.<event>` subjects.
   Currently consumed only by the doots 3D visualization. **This is the data source
   we'll tap for streaming.**

4. **SSE bus** (`sse.go`): kbeads emits `beads.bead.{created,updated,closed}` events.
   The bridge subscribes and dispatches to handlers (agents, decisions, beadactivity,
   dashboard, etc.). This is the bead lifecycle bus, not the tool-level telemetry bus.

5. **Agent cards** (`agents.go`): Per-agent Slack messages showing spawn/crash/state
   changes. Updated via SSE bead.updated events.

### What Slack Offers (New Streaming APIs)

Slack's AI streaming APIs (available in slack-go v0.18.0, which we already use):

- **`chat.startStream(channel, thread_ts)`** → returns `ts` (message timestamp)
- **`chat.appendStream(channel, ts, ...)`** → append markdown text or task_update chunks
- **`chat.stopStream(channel, ts, ...)`** → finalize with optional Block Kit blocks

**Chunk types:**
- `markdown_text`: Streaming text content
- `task_update`: Structured task cards with `{id, title, status, details, output, sources}`
  - Status values: `pending`, `in_progress`, `complete`, `error`
- `plan_update`: Group header for multiple tasks

**`task_display_mode`**: `timeline` (sequential cards) or `plan` (grouped view)

**Rate limits:** `appendStream` is Tier 4 (100+/min), `stopStream` is Tier 2 (20+/min).

**Constraints:**
- Block Kit only allowed in `stopStream`, not start/append
- Unfurling disabled in streaming messages
- `markdown_text` limited to 12,000 chars per call
- Requires `chat:write` scope (already have it)

## Architecture

### Data Flow

```
┌─────────────────────────────────────────────────────────────────┐
│ Agent Pod                                                       │
│                                                                 │
│  Claude Code ──hook──▶ gb hook relay ──▶ NATS hooks.<agent>.*   │
│    (PreToolUse, PostToolUse, Stop, etc.)                        │
└────────────────────────────────────────┬────────────────────────┘
                                         │
                                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ kbeads daemon                                                   │
│                                                                 │
│  NATS sub hooks.* ──▶ /v1/hooks/publish (HTTP)                  │
│  or: direct NATS subscribe                                      │
└────────────────────────────────────────┬────────────────────────┘
                                         │ (forward to bridge)
                                         ▼
┌─────────────────────────────────────────────────────────────────┐
│ Slack Bridge                                                    │
│                                                                 │
│  NEW: HookStream subscriber                                     │
│    ├─ Subscribe to NATS hooks.<agent>.* per active agent        │
│    ├─ Aggregate tool_name counts in memory                      │
│    ├─ Batch append on timer (every 2-3s)                        │
│    └─ Call chat.startStream / appendStream / stopStream          │
│                                                                 │
│  Lifecycle:                                                     │
│    agent.created  → startStream (in assistant thread)            │
│    hooks.*.PreToolUse → increment tool counter → appendStream   │
│    agent.closed   → stopStream (with final summary blocks)      │
└─────────────────────────────────────────────────────────────────┘
```

### Key Design Decisions

1. **Bridge-side aggregation, not per-event streaming.** Tool use hooks fire at high
   frequency (many per second during code gen). We aggregate in the bridge and batch
   `appendStream` calls every 2-3 seconds to stay well within Tier 4 rate limits.

2. **NATS subscription for hook events.** The bridge already connects to NATS for hook
   relay. We add wildcard subscriptions per active agent: `hooks.<agent>.*`.

3. **Streaming only in assistant threads.** The streaming UX (animated text, task cards)
   is designed for the AI container pane. Regular channel threads keep the current
   behavior (static messages + dashboard).

4. **Graceful fallback.** If streaming APIs aren't available (older Slack workspace,
   API errors), fall back to existing `setStatus` + static messages.

## Implementation Plan

### Phase 1: Simple Status Streaming (MVP)

**Goal:** When an agent is working in an assistant thread, stream a live status message
showing tool use count and current activity.

#### 1.1 — Add `AgentStreamManager` to bridge

New file: `internal/bridge/agentstream.go`

```go
// AgentStreamManager tracks active streaming sessions per agent.
// Each agent with an active assistant thread gets a streaming message
// that shows real-time tool use stats.
type AgentStreamManager struct {
    api      *slack.Client
    logger   *slog.Logger

    mu       sync.Mutex
    sessions map[string]*agentStreamSession // agent name → session
}

type agentStreamSession struct {
    channelID  string
    threadTS   string    // assistant thread ts
    streamTS   string    // streaming message ts (from startStream)
    toolCounts map[string]int  // tool_name → count
    totalTools int
    lastAppend time.Time
    status     string    // "working", "reading", "editing", etc.
    dirty      bool
}
```

**Responsibilities:**
- `StartSession(agent, channel, threadTS)` — called when agent spawns in assistant thread
- `HandleToolEvent(agent, toolName, event)` — called on PreToolUse/PostToolUse hooks
- `StopSession(agent)` — called when agent closes; finalizes stream
- Background goroutine flushes dirty sessions every 2-3s via `appendStream`

#### 1.2 — Subscribe to hook events in the bridge

In `cmd/slack-bridge/main.go`, add NATS subscription for hook events:

```go
// Subscribe to hook events for streaming status
natsConn, _ := nats.Connect(cfg.NatsURL)
hookSub, _ := natsConn.Subscribe("hooks.>", func(msg *nats.Msg) {
    streamMgr.HandleHookEvent(msg.Subject, msg.Data)
})
```

Or, if kbeads already forwards hook events to SSE, subscribe via SSE on a new topic.

#### 1.3 — Wire into agent lifecycle

Extend `Agents.handleCreated` and `Agents.handleClosed`:
- On agent spawn in an assistant thread → `streamMgr.StartSession(...)`
- On agent close → `streamMgr.StopSession(...)`

#### 1.4 — Streaming message format

**Start stream** (on agent spawn):
```
🤖 Agent `worker-1` is working...
```

**Append stream** (every 2-3s while tools are running):
```
🤖 Agent `worker-1` · 12 tool calls
  📖 Read (4) · 🔍 Grep (3) · ✏️ Edit (2) · 💻 Bash (2) · 🌐 WebFetch (1)
  Currently: reading files...
```

**Stop stream** (on agent done/failed):
Final `stopStream` with Block Kit summary blocks showing:
- Total tool calls with breakdown
- Duration
- Outcome (completed / failed / stopped)

### Phase 2: Task Progress Cards

**Goal:** Use Slack's `task_update` chunk type to show agent work items as structured
task cards with status progression.

#### 2.1 — Map bead lifecycle to task_update chunks

When agent claims a bead → `appendStream` with `task_update`:
```json
{
  "type": "task_update",
  "id": "ka-XXXXX",
  "title": "Fix nil pointer in bridge",
  "status": "in_progress"
}
```

When agent closes a bead → update to `complete`:
```json
{
  "type": "task_update",
  "id": "ka-XXXXX",
  "title": "Fix nil pointer in bridge",
  "status": "complete",
  "output": "Fixed in commit abc1234. PR #42 created."
}
```

#### 2.2 — Use `plan` display mode for multi-task agents

When an agent has multiple tasks (e.g., epic breakdown), use `task_display_mode: "plan"`
to group them:

```json
{
  "type": "plan_update",
  "title": "Refactor auth system"
}
```

Then individual `task_update` chunks for each subtask.

#### 2.3 — Wire BeadActivity events

The existing `BeadActivity` watcher already tracks create/claim/close for work items.
Extend it to also push `task_update` chunks to the stream manager.

### Phase 3: Rich Telemetry Streaming

**Goal:** Stream richer operational data as the agent works.

#### 3.1 — Git activity

On `PostToolUse` where `tool_name == "Bash"` and command contains `git commit/push`:
```
📦 Committed: fix(bridge): handle nil pointer
🚀 Pushed to branch `fix/nil-pointer`
```

#### 3.2 — Test results

On `PostToolUse` where `tool_name == "Bash"` and output contains test patterns:
```
✅ Tests: 42 passed, 0 failed (3.2s)
```

#### 3.3 — Decision checkpoints

When agent creates a decision bead:
```json
{
  "type": "task_update",
  "id": "decision-ka-XXXXX",
  "title": "Needs input: session handling approach",
  "status": "pending"
}
```

When decision is resolved → update to `complete`.

#### 3.4 — Subagent tracking

On `SubagentStart` hook:
```json
{
  "type": "task_update",
  "id": "subagent-abc123",
  "title": "Research: explore codebase",
  "status": "in_progress"
}
```

On `SubagentStop` → `complete`.

## Technical Considerations

### slack-go v0.18.0 Streaming Support

slack-go v0.18.0 includes `StartStreamContext`, `AppendStreamContext`, and
`StopStreamContext` methods. We can use these directly — no raw HTTP calls needed.

Need to verify the exact method signatures and chunk parameter format. If the Go
library doesn't support the `chunks` parameter yet (it may only support `markdown_text`),
we'll need to either:
- Add a PR to slack-go, or
- Use raw `api.PostJSON` calls for chunk-based streaming

### NATS vs SSE for Hook Events

Two options for getting hook events into the bridge:

**Option A: Direct NATS subscription** (recommended for Phase 1)
- Bridge already has NATS connectivity
- Subscribe to `hooks.>` wildcard
- Low latency, no intermediary

**Option B: kbeads SSE forwarding**
- Hook events published via HTTP to kbeads (`/v1/hooks/publish`)
- kbeads could re-emit them as SSE events on a new topic (e.g., `hooks.agent.*`)
- More consistent with the existing SSE-based bridge architecture
- Requires kbeads changes

**Recommendation:** Start with Option A (direct NATS) for speed, migrate to Option B
when we want all event routing through kbeads.

### Rate Limiting

- `appendStream`: Tier 4 (100+/min) — plenty of headroom for 2-3s batched updates
- `stopStream`: Tier 2 (20+/min) — fine for agent lifecycle events
- With 10 concurrent agents, worst case = 10 agents × 1 append/3s = ~200/min
- Need per-agent rate limiting in the stream manager (min 2s between appends)

### Memory Management

- `agentStreamSession` is lightweight (~200 bytes + tool count map)
- Sessions are cleaned up on agent close
- Failsafe: sessions older than 24h are garbage collected

### Assistant Thread Detection

We already track assistant threads via `isAssistantThread()` in `bot.go`. Need to
persist the mapping: agent name → assistant thread (channel, thread_ts) so the stream
manager knows where to stream.

The bridge's `StateManager` already persists message references by bead ID. We can
extend it with agent→thread mappings.

## File Changes Summary

### Phase 1 (MVP)

| File | Change |
|------|--------|
| `internal/bridge/agentstream.go` | **NEW** — AgentStreamManager, session lifecycle |
| `internal/bridge/agentstream_test.go` | **NEW** — Unit tests with mock Slack server |
| `cmd/slack-bridge/main.go` | Add NATS sub for hooks, wire AgentStreamManager |
| `internal/bridge/bot_assistant.go` | Track agent→thread mapping on spawn |
| `internal/bridge/agents.go` | Wire spawn/close to stream manager |
| `internal/bridge/state.go` | Add agent stream session persistence |

### Phase 2 (Task Cards)

| File | Change |
|------|--------|
| `internal/bridge/agentstream.go` | Add task_update chunk support |
| `internal/bridge/beadactivity.go` | Push task events to stream manager |

### Phase 3 (Rich Telemetry)

| File | Change |
|------|--------|
| `internal/bridge/agentstream.go` | Git/test/decision/subagent event parsing |
| `cmd/gb/hook_relay.go` | Possibly enrich hook payloads with more context |

## Open Questions

1. **slack-go chunks support:** Does v0.18.0 support the `chunks` parameter for
   `task_update` objects, or only `markdown_text`? Need to check the library source.
   If not, Phase 1 uses only `markdown_text`, and we add chunks support later.

2. **Streaming outside assistant threads:** Should we also stream in regular channel
   threads (where agents are spawned via @mention)? The streaming API works in any
   thread, but the task_update UI may only render properly in assistant threads.

3. **Dashboard integration:** Should the dashboard also use streaming for its refresh
   cycle? Probably not — the dashboard is a single long-lived message, not a
   conversational reply. Keep it as `chat.update`.

4. **Multi-agent threads:** If multiple agents share an assistant thread (unlikely but
   possible), each gets its own streaming message with its own `ts`.

## Success Metrics

- Agent status is visible in Slack within 3s of a tool use event
- Tool use count increments live as the agent works
- No increase in Slack API error rate (rate limits, etc.)
- Streaming messages finalize cleanly on agent stop/crash

## Timeline Estimate

- **Phase 1:** ~2-3 days of implementation + testing
- **Phase 2:** ~1-2 days (builds on Phase 1 infrastructure)
- **Phase 3:** ~2-3 days (incremental enrichment)
