# Hook Event Schema for NATS

## Overview

Claude Code hooks in agent pods publish tool and lifecycle events to the
`HOOK_EVENTS` NATS JetStream stream.

> **Consumer status, verified 2026-09-21.** The original consumer was beads3d
> (the "doots" visualisation); it was deleted in `f1fd0c5f`. The only remaining
> non-test subscriber to `hooks.>` is `bridge.HookStream`
> (`gardenio/controller/internal/bridge/hookstream.go:229`), built solely by
> `gardenio/controller/cmd/slack-bridge/main.go:591`. slack-bridge is disabled
> on the live cluster, so **these events currently reach no consumer in
> production.** Producers below are accurate and the stream is durable; only the
> read side is missing. Plan of record:
> `gardenio/plans/claude-session-subagent-tracking.md`.

## NATS Stream

- **Stream:** `HOOK_EVENTS` (already exists)
- **Subject pattern:** `hooks.>`
- **Subject format:** `hooks.<agent>.<event>`
  - Example: `hooks.worker-1.PreToolUse`

## Events to Capture

### High-priority (real-time activity visualization)

| Event | Purpose | Fires when |
|-------|---------|------------|
| `PreToolUse` | Show tool about to execute | Before tool runs |
| `PostToolUse` | Show tool result | After tool succeeds |
| `Stop` | Agent finished responding | Turn complete |
| `SubagentStart` | Parallel work spawned | Subagent created |
| `SubagentStop` | Parallel work finished | Subagent done |

### Medium-priority (session lifecycle)

| Event | Purpose | Fires when |
|-------|---------|------------|
| `SessionStart` | Agent session began | New/resumed session |
| `SessionEnd` | Agent session ended | Session terminates |
| `PreCompact` | Context compaction | Before compaction |
| `PostCompact` | Context recovery | After compaction |

### Agent teams (when enabled)

| Event | Purpose | Fires when |
|-------|---------|------------|
| `TeammateIdle` | Teammate went idle | Teammate has no more work |
| `TaskCompleted` | Task marked done | Teammate finishes a task |

### Error and notification events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `PostToolUseFailure` | Tool invocation failed | Tool returns error |
| `StopFailure` | Agent stop failed | Stop hook errors |
| `Notification` | Claude Code notification | Internal notification |
| `TaskCreated` | Agent created a task | TaskCreate tool used |

### Environment events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `CwdChanged` | Working directory changed | Agent switches worktree or repo |
| `FileChanged` | Watched file modified | Config or build artifact changed |

### Permission events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `PermissionRequest` | Permission dialog shown | Agent needs permission approval |

### Configuration and setup events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `ConfigChange` | Settings changed | Config file updated |
| `InstructionsLoaded` | CLAUDE.md loaded | Instructions file read |
| `Setup` | Setup phase | Init or maintenance setup |

### Elicitation events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `Elicitation` | MCP elicitation shown | MCP server requests user input |
| `ElicitationResult` | Elicitation resolved | User responds to elicitation |

### Worktree events

| Event | Purpose | Fires when |
|-------|---------|------------|
| `WorktreeCreate` | Worktree created | Git worktree set up |
| `WorktreeRemove` | Worktree removed | Git worktree cleaned up |

### Not captured

| Event | Reason |
|-------|--------|
| `UserPromptSubmit` | In agent pods these are system prompts, low signal |

## Payload Schema

All events share a common envelope:

```json
{
  "agent": "worker-1",
  "agent_bead_id": "gd-2bZJkQ8VZR",
  "project": "gardenio",
  "role": "crew",
  "session_id": "abc-123-def",
  "harness": "claude",
  "event": "PreToolUse",
  "ts": "2026-03-02T07:30:00.000Z",
  "cwd": "/home/agent/workspace",
  "is_sidechain": false
}
```

### Common Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `agent` | string | yes | Agent name from `$HOSTNAME` (e.g., `worker-1`) |
| `agent_bead_id` | string | no | Agent bead id, e.g. `gd-2bZJkQ8VZR`. Omitted off-cluster |
| `project` | string | no | Gardenio project the agent is working in, from `BOAT_PROJECT`. Omitted off-cluster |
| `role` | string | no | Agent's functional role (e.g. `crew`, `gardener`), from `BOAT_ROLE`. Omitted off-cluster |
| `session_id` | string | yes | Harness-neutral session ID: the Claude Code session ID, or the Codex session (thread) ID |
| `harness` | string | yes | Agent CLI that fired the hook: `claude` or `codex`. From `GARDENIO_HARNESS`, else sniffed from the payload (`turn_id` / `rollout-*` `transcript_path` mean Codex) |
| `event` | string | yes | Hook event name |
| `ts` | string | yes | ISO 8601 timestamp with milliseconds |
| `cwd` | string | no | Working directory |
| `is_sidechain` | boolean | no | `true` when the tool call was issued from a subagent/sidechain context rather than the main conversation thread. Omitted when `false` |

`project` and `role` are two of the P2 HOOK_EVENTS consumer's bounded metric
labels (`gardenio_tool_latency_seconds{tool,project,source,env}`,
plan `gardenio/plans/agent-tool-latency-attribution.md` §P2, bead
`gd-sWwPnwkZtw`) — small and fixed, unlike `agent`/`agent_bead_id`.

**Use `agent_bead_id`, not `agent`, as the join key.** `agent` is a display name
derived from `BOAT_AGENT` or the pod hostname and then passed through
`sanitizeSubject`, so it is lossy and repeats across pods — it cannot be
resolved back to a bead. `agent_bead_id` is the stable key that lets a consumer
attach an event to the `type=agent` and `type=session` beads that already exist,
which is what makes subagent lineage expressible at all.

The producer reads it from `KD_AGENT_ID`, falling back to `BOAT_AGENT_BEAD_ID`
(`cmd/gb/hook_relay.go`, `resolveAgentBeadID`), matching every other `gb` call
site. It is `omitempty`: a run with no agent context, such as a developer
laptop, ships the key absent rather than empty, so consumers must tolerate its
absence and must not treat `""` as an id.

### PreToolUse

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PreToolUse",
  "ts": "2026-03-02T07:30:00.000Z",
  "tool_name": "Bash",
  "tool_use_id": "toolu_014m2vK9",
  "prompt_id": "prompt-abc",
  "permission_mode": "default",
  "tool_input": {
    "command": "go test ./...",
    "description": "Run tests"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `tool_name` | string | Tool name: `Bash`, `Edit`, `Write`, `Read`, `Glob`, `Grep`, `Agent`, `WebFetch`, `WebSearch`, MCP tools |
| `tool_input` | object | Tool parameters (truncated to 1KB if large) |
| `tool_use_id` | string | Claude's pairing key for this call, stable across its PreToolUse and PostToolUse/PostToolUseFailure events (see **Tool call pairing fields** below) |
| `prompt_id` | string | ID of the assistant turn that issued this call |
| `permission_mode` | string | Session permission mode at call time: `default`, `acceptEdits`, `bypassPermissions`, `plan` |

### PostToolUse

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PostToolUse",
  "ts": "2026-03-02T07:30:01.500Z",
  "tool_name": "Bash",
  "tool_use_id": "toolu_014m2vK9",
  "prompt_id": "prompt-abc",
  "permission_mode": "default",
  "duration_ms": 415,
  "duration_source": "claude",
  "tool_input": {
    "command": "go test ./..."
  },
  "tool_response": {
    "success": true
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `tool_name` | string | Same as PreToolUse |
| `tool_input` | object | Tool parameters (truncated) |
| `tool_response` | object | Tool result summary (truncated to 1KB) |
| `tool_use_id` | string | Same pairing key as this call's PreToolUse |
| `prompt_id` | string | ID of the assistant turn that issued this call |
| `permission_mode` | string | Session permission mode at call time |
| `duration_ms` | integer | Claude Code's own measurement of tool execution time. **Optional** — see below |
| `duration_source` | string | `"claude"` when `duration_ms` came from Claude Code's own payload. Omitted (along with `duration_ms`) when the producing Claude Code version doesn't ship it |
| `output_bytes` | integer | Bytes of every string in the response before the 1KB cut (a shell call's stdout and stderr). Omitted when the response has no text. The Commons jobs record reads it (`internal/commons/jobs.go`) |

`PostToolUseFailure` carries `tool_use_id`, `prompt_id` and `permission_mode`
the same way, but never `duration_ms` — a failed call has no completed
measurement to report.

#### Tool call pairing fields

`tool_use_id` is Claude's own pairing key, unique within a session, and the
reason it is worth stamping on *both* `PreToolUse` and the matching
`PostToolUse`/`PostToolUseFailure`: pairing by tool name and FIFO arrival order
mis-attributes ~37% of durations once two same-named calls overlap (coop's fix
for its own FIFO pairing bug is `gd-Kl42VWWRd6`). A consumer that needs to pair
these events should key on `tool_use_id`, not on `tool_name` + arrival order.

`duration_ms` is `omitempty` and CC-version dependent: older Claude Code
builds don't send it. Treat its absence as "unknown", not "zero". A consumer
that needs a duration when `duration_ms` is absent must derive one itself,
typically from the arrival timestamps of the paired `PreToolUse`/`PostToolUse`
events, and is responsible for marking that value `"derived"` in its own data
model — `gb hook relay` is a one-shot process per hook invocation with no
cross-event state, so it can never emit `duration_source: "derived"` itself;
that value only ever shows up in whatever downstream store does the deriving.

### Stop

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "Stop",
  "ts": "2026-03-02T07:35:00.000Z",
  "last_assistant_message": "I've completed the refactoring..."
}
```

| Field | Type | Description |
|-------|------|-------------|
| `last_assistant_message` | string | Last assistant response text (truncated to 512 chars) |
| `stop_gate_decision` | string | `block` or `allow`; only on the Stop events `gb hook stop-gate` publishes |
| `reason` | string | With `stop_gate_decision`: the gate's reason (a block's server reason, or the allow path, e.g. `cooldown`, `job_mode`, `allowed`), redacted, at most 512 chars |

Agent pods run only `gb hook stop-gate` on Stop, not `gb hook relay`, so every Stop on
the bus comes from the gate, one per verdict (gd-fSdUbOmf3g). The gate redacts
`last_assistant_message` before the 512-char cut and publishes best-effort: it waits at
most 500 ms, and a bus failure never changes the verdict.

### PostToolUseFailure

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PostToolUseFailure",
  "ts": "2026-03-02T07:30:01.500Z",
  "tool_name": "Bash",
  "tool_input": { "command": "npm test" },
  "error": "command exited with code 1",
  "error_type": "tool_error"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `tool_name` | string | Tool that failed |
| `tool_input` | object | Tool parameters (truncated to 1KB) |
| `error` | string | Error message |
| `error_type` | string | Error classification |

### StopFailure

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "StopFailure",
  "ts": "2026-03-02T07:35:00.000Z",
  "error": "stop hook timed out"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `error` | string | Error message |

### Notification

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "Notification",
  "ts": "2026-03-02T07:40:00.000Z",
  "notification_type": "context_window_warning"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `notification_type` | string | Notification category |

### TaskCreated

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "TaskCreated",
  "ts": "2026-03-02T07:42:00.000Z",
  "task_id": "task-456",
  "task_subject": "Implement error handling"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | ID of the created task |
| `task_subject` | string | Subject/title of the task |

### SubagentStart

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "SubagentStart",
  "ts": "2026-03-02T07:30:02.000Z",
  "subagent_id": "agent-abc123",
  "subagent_type": "Explore"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `subagent_id` | string | Subagent instance ID |
| `subagent_type` | string | Agent type: `Bash`, `Explore`, `Plan`, etc. |

### SubagentStop

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "SubagentStop",
  "ts": "2026-03-02T07:30:05.000Z",
  "subagent_id": "agent-abc123",
  "subagent_type": "Explore",
  "last_assistant_message": "Found 3 matching files..."
}
```

| Field | Type | Description |
|-------|------|-------------|
| `subagent_id` | string | Subagent instance ID |
| `subagent_type` | string | Agent type: `Bash`, `Explore`, `Plan`, etc. |
| `last_assistant_message` | string | Last assistant response text (truncated to 512 chars) |

### SessionStart

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "SessionStart",
  "ts": "2026-03-02T07:25:00.000Z",
  "source": "startup"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `source` | string | `startup`, `resume`, `clear`, or `compact` |

### SessionEnd

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "SessionEnd",
  "ts": "2026-03-02T08:00:00.000Z",
  "reason": "other"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `reason` | string | `clear`, `logout`, `prompt_input_exit`, `bypass_permissions_disabled`, `other` |

### PreCompact

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PreCompact",
  "ts": "2026-03-02T07:45:00.000Z",
  "trigger": "auto"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `trigger` | string | `manual` or `auto` |

### PostCompact

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PostCompact",
  "ts": "2026-03-02T07:45:00.000Z",
  "trigger": "auto"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `trigger` | string | `manual` or `auto` |

### TeammateIdle

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "TeammateIdle",
  "ts": "2026-03-02T07:50:00.000Z",
  "teammate_id": "teammate-abc",
  "teammate_type": "research"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `teammate_id` | string | ID of the idle teammate session |
| `teammate_type` | string | Role/type of the teammate |

### TaskCompleted

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "TaskCompleted",
  "ts": "2026-03-02T07:52:00.000Z",
  "task_id": "task-123",
  "task_subject": "Fix login bug"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `task_id` | string | ID of the completed task |
| `task_subject` | string | Subject/title of the completed task |

### CwdChanged

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "CwdChanged",
  "ts": "2026-03-02T07:55:00.000Z",
  "old_cwd": "/home/agent/workspace",
  "new_cwd": "/home/agent/worktrees/ka-abc123"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `old_cwd` | string | Previous working directory |
| `new_cwd` | string | New working directory |

### FileChanged

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "FileChanged",
  "ts": "2026-03-02T07:56:00.000Z",
  "file_path": "go.mod",
  "change_type": "modified"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `file_path` | string | Path of the changed file |
| `change_type` | string | Type of change (e.g., `modified`, `created`, `deleted`) |

### PermissionRequest

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "PermissionRequest",
  "ts": "2026-03-02T07:55:00.000Z",
  "tool_name": "Bash",
  "request_id": "req-456"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `tool_name` | string | Tool requesting permission |
| `request_id` | string | Permission request ID |

### ConfigChange

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "ConfigChange",
  "ts": "2026-03-02T07:57:00.000Z",
  "config_source": "project_settings",
  "file_path": ".claude/settings.json"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `config_source` | string | Source: `user_settings`, `project_settings`, `local_settings`, `policy_settings`, `skills` |
| `file_path` | string | Path to the changed config file (optional) |

### InstructionsLoaded

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "InstructionsLoaded",
  "ts": "2026-03-02T07:25:01.000Z",
  "instruction_file_path": "/home/agent/workspace/CLAUDE.md",
  "memory_type": "Project",
  "load_reason": "session_start"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `instruction_file_path` | string | Path to the loaded instructions file |
| `memory_type` | string | Type: `User`, `Project`, `Local`, `Managed` |
| `load_reason` | string | Reason: `session_start`, `nested_traversal`, `path_glob_match`, `include`, `compact` |

### Elicitation

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "Elicitation",
  "ts": "2026-03-02T08:00:00.000Z",
  "mcp_server_name": "mezmo",
  "message": "Please provide your API key",
  "elicitation_id": "el-456"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `mcp_server_name` | string | MCP server requesting input |
| `message` | string | Elicitation prompt message (truncated to 512 chars) |
| `elicitation_id` | string | Elicitation request ID (optional) |

### ElicitationResult

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "ElicitationResult",
  "ts": "2026-03-02T08:00:05.000Z",
  "mcp_server_name": "mezmo",
  "elicitation_id": "el-456",
  "action": "accept"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `mcp_server_name` | string | MCP server that requested input |
| `elicitation_id` | string | Elicitation request ID (optional) |
| `action` | string | User action: `accept`, `decline`, `cancel` |

### WorktreeCreate

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "WorktreeCreate",
  "ts": "2026-03-02T07:30:00.000Z",
  "worktree_name": "fix/ka-abc123"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `worktree_name` | string | Name/branch of the created worktree |

### WorktreeRemove

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "WorktreeRemove",
  "ts": "2026-03-02T09:00:00.000Z",
  "worktree_path": "/home/agent/worktrees/ka-abc123"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `worktree_path` | string | Path to the removed worktree |

### Setup

```json
{
  "agent": "worker-1",
  "session_id": "abc-123",
  "event": "Setup",
  "ts": "2026-03-02T07:24:00.000Z",
  "trigger": "init"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `trigger` | string | Setup trigger: `init` or `maintenance` |

## Publishing Mechanism

The hook relay script reads JSON from stdin (Claude Code hook input),
extracts the relevant fields, and publishes to NATS using one of:

1. **HTTP to kbeads daemon** (preferred): `POST $BEADS_HTTP_ADDR/v1/hooks/publish`
   - New endpoint needed on kbeads daemon
   - Daemon handles NATS publishing
   - No NATS client needed in hook script

2. **Direct NATS publish** (fallback): Using `nats pub` CLI or HTTP gateway
   - Requires NATS tools in agent image
   - Uses `$BEADS_NATS_URL` env var

### Hook Configuration (settings.json)

```json
{
  "hooks": {
    "PreToolUse": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "PostToolUse": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "PostToolUseFailure": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "Stop": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "StopFailure": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "SubagentStart": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "SubagentStop": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "SessionStart": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "SessionEnd": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "PreCompact": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "PostCompact": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "TaskCompleted": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "TaskCreated": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "TeammateIdle": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "Notification": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "CwdChanged": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "FileChanged": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "PermissionRequest": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "ConfigChange": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "InstructionsLoaded": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "Elicitation": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "ElicitationResult": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "WorktreeCreate": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "WorktreeRemove": [{"type": "command", "command": "/usr/local/bin/hook-relay"}],
    "Setup": [{"type": "command", "command": "/usr/local/bin/hook-relay"}]
  }
}
```

A single `hook-relay` script handles all event types. It reads
`hook_event_name` from stdin JSON to determine the event type and
constructs the appropriate NATS subject.

## Size Limits

- `tool_input`: Truncated to 1024 bytes (avoids large file contents in Write/Edit)
- `tool_response`: Truncated to 1024 bytes
- Total message: Soft cap at 4KB

## Compatibility

The schema is forward-compatible with the existing `HOOK_EVENTS` stream:
- Uses the same `hooks.>` subject pattern
- Subject hierarchy `hooks.<agent>.<event>` allows filtering by agent
  or event type (e.g., `hooks.worker-1.>` or `hooks.*.PreToolUse`)
- Consumers can subscribe to `hooks.>` for all events

### Codex payloads

`gb hook relay` also accepts Codex hook input (`$CODEX_HOME/hooks.json`, see
[codex-hooks.md](codex-hooks.md)). The shape matches Claude's for the five core events, so the same
event fields are filled. Differences: `transcript_path` is a rollout file, `permission_mode` uses
Codex values, `tool_name` can be `apply_patch` or `mcp__server__tool`, and every turn event adds
`turn_id`, which is mapped to `prompt_id` on tool events. NATS subjects and consumers are unchanged.
