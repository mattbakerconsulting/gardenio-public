# WebSocket Protocol

Coop provides a WebSocket endpoint for real-time terminal output streaming,
agent state changes, and bidirectional control.


## Overview

- **URL**: `ws://localhost:{port}/ws`
- **Query parameters**: `subscribe` (comma-separated flags), `token` (auth token)
- **Protocol**: JSON text frames, one message per frame
- **Message format**: Internally-tagged JSON (`{"event": "...", ...}`)


## Authentication

WebSocket connections have two authentication paths:

1. **Query parameter** -- pass `?token=<token>` on the upgrade request
2. **Auth message** -- send a `{"event": "auth", "token": "..."}` message after connecting

When `--auth-token` is configured, the WebSocket upgrade always succeeds
(the `/ws` path skips HTTP auth middleware). If no token is provided in the
query string, the connection starts in an unauthenticated state.

**Per-connection auth state:**

| Token in query | Auth state | Read operations | Write operations |
|----------------|------------|-----------------|------------------|
| Valid | Authenticated | Allowed | Allowed |
| Missing | Unauthenticated | Allowed | Blocked until `auth` message |
| Invalid | Rejected | Connection refused (401) | -- |

Only `health:get`, `ready:get`, and `ping` are available without
authentication. All other operations require authentication.


## Subscription Modes

Set via the `subscribe` query parameter on the upgrade URL (comma-separated flags).

| Flag | Server pushes |
|------|---------------|
| `pty` | `pty` messages with base64-encoded PTY bytes (`output` accepted as alias) |
| `screen` | `screen` messages with rendered terminal state |
| `state` | `transition`, `exit`, `prompt:outcome`, `stop:outcome`, `start:outcome` messages |
| `hooks` | `hook:raw` messages with raw hook FIFO JSON |
| `messages` | `message:raw` messages with raw agent JSONL |
| `transcripts` | `transcript:saved` messages with transcript save events |
| `profiles` | `profile:switched`, `profile:exhausted`, `profile:rotation:exhausted` messages |

Default (no `subscribe` param) = no push events (request-reply only).

Example: `ws://localhost:8080/ws?subscribe=pty,state&token=mytoken`


## Server → Client Messages


### `pty`

Raw PTY output chunk. Sent when `pty` is subscribed.

```json
{
  "event": "pty",
  "data": "SGVsbG8gV29ybGQ=",
  "offset": 1024
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | string | Base64-encoded raw bytes |
| `offset` | int | Byte offset in the output stream |


### `screen`

Rendered terminal screen snapshot. Sent when `screen` is subscribed on each
screen update, or in response to a `screen:get`.

```json
{
  "event": "screen",
  "lines": ["$ hello", "world", ""],
  "cols": 120,
  "rows": 40,
  "alt_screen": false,
  "mouse_tracking": 0,
  "mouse_sgr": false,
  "cursor": { "row": 2, "col": 0 },
  "seq": 42
}
```

| Field | Type | Description |
|-------|------|-------------|
| `lines` | string[] | One string per terminal row |
| `cols` | int | Terminal width |
| `rows` | int | Terminal height |
| `alt_screen` | bool | Whether the alternate screen buffer is active |
| `mouse_tracking` | int | The mouse tracking mode the app has set (9, 1000, 1002 or 1003), 0 for none |
| `mouse_sgr` | bool | Whether the app asked for SGR mouse reports (mode 1006) |
| `cursor` | CursorPosition or null | Cursor position |
| `seq` | int | Monotonic screen sequence number |


### `transition`

Agent state transition. Sent when `state` is subscribed.

```json
{
  "event": "transition",
  "prev": "working",
  "next": "prompt",
  "seq": 15,
  "prompt": {
    "type": "permission",
    "subtype": "tool",
    "tool": "Bash",
    "input": "{\"command\":\"rm -rf /tmp/test\"}",
    "options": ["Yes", "Yes, and don't ask again for this tool", "No"],
    "options_fallback": false,
    "questions": [],
    "question_current": 0,
    "ready": true
  },
  "error_detail": null,
  "error_category": null,
  "cause": "tier1_hooks",
  "last_message": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `prev` | string | Previous agent state |
| `next` | string | New agent state |
| `seq` | int | State sequence number |
| `prompt` | PromptContext or null | Prompt context (when `next` is `"prompt"`) |
| `error_detail` | string or null | Error text (when `next` is `"error"`) |
| `error_category` | string or null | Error classification (when `next` is `"error"`) |
| `cause` | string | Detection source that triggered this transition |
| `last_message` | string or null | Last message extracted from agent output |


### `exit`

Agent process exited. Sent when `state` is subscribed.
This replaces `transition` for the terminal `exited` state.

```json
{
  "event": "exit",
  "code": 0,
  "signal": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `code` | int or null | Process exit code |
| `signal` | int or null | Signal number that killed the process |


### `prompt:outcome`

Prompt action event.
Sent when `state` is subscribed, when a prompt is responded to via the API.

```json
{
  "event": "prompt:outcome",
  "source": "api",
  "type": "permission",
  "subtype": "tool",
  "option": 1
}
```

| Field | Type | Description |
|-------|------|-------------|
| `source` | string | Source of the action (e.g. `"api"`) |
| `type` | string | Prompt type that was responded to |
| `subtype` | string or null | Prompt subtype |
| `option` | int or null | Option number that was selected |


### `stop:outcome`

Stop hook verdict event.
Sent when `state` is subscribed, whenever a stop hook check occurs.

```json
{
  "event": "stop:outcome",
  "type": "blocked",
  "signal": null,
  "error_detail": null,
  "seq": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Verdict type (see table below) |
| `signal` | JSON or null | Signal body (when `type` is `"signaled"`) |
| `error_detail` | string or null | Error details (when `type` is `"error"`) |
| `seq` | int | Monotonic stop event sequence number |

**Stop types:**

| Type | Description |
|------|-------------|
| `signaled` | Signal received via resolve endpoint; agent allowed to stop |
| `error` | Agent in unrecoverable error state; allowed to stop |
| `safety_valve` | Claude's safety valve triggered; must allow |
| `blocked` | Stop was blocked; agent should continue working |
| `allowed` | Mode is `allow`; agent always allowed to stop |


### `start:outcome`

Start hook event.
Sent when `state` is subscribed, whenever a session lifecycle event fires.

```json
{
  "event": "start:outcome",
  "source": "resume",
  "session_id": "abc123",
  "injected": true,
  "seq": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `source` | string | Lifecycle event type (e.g. `"start"`, `"resume"`, `"clear"`) |
| `session_id` | string or null | Session identifier if available |
| `injected` | bool | Whether a non-empty script was injected |
| `seq` | int | Monotonic start event sequence number |


### `hook:raw`

Raw hook FIFO JSON event. Sent when `hooks` is subscribed.

```json
{
  "event": "hook:raw",
  "data": { "event": "BeforeTool", "tool_name": "Bash" }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | object | The full JSON object from the hook FIFO pipe |


### `message:raw`

Raw agent JSONL message from stdout or log. Sent when `messages` is subscribed.

```json
{
  "event": "message:raw",
  "data": { "type": "assistant", "message": "..." },
  "source": "stdout"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | object | The full JSON object from stdout or the session log |
| `source` | string | Origin: `"stdout"` (Tier 3) or `"log"` (Tier 2) |


### `transcript:saved`

Transcript save event. Sent when `transcripts` is subscribed.

```json
{
  "event": "transcript:saved",
  "number": 1,
  "timestamp": "2026-02-10T14:35:00Z",
  "line_count": 150,
  "seq": 42
}
```

| Field | Type | Description |
|-------|------|-------------|
| `number` | int | Transcript number |
| `timestamp` | string | Timestamp when the snapshot was taken |
| `line_count` | int | Number of lines in the transcript |
| `seq` | int | Monotonic sequence number |


### `profile:switched`

Active profile changed. Sent when `profiles` is subscribed.

```json
{
  "event": "profile:switched",
  "from": "profile-a",
  "to": "profile-b"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `from` | string or null | Previous profile name (null on first activation) |
| `to` | string | New active profile name |


### `profile:exhausted`

A single profile became rate-limited. Sent when `profiles` is subscribed.

```json
{
  "event": "profile:exhausted",
  "profile": "profile-a"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `profile` | string | Name of the exhausted profile |


### `profile:rotation:exhausted`

All profiles are on cooldown. Sent when `profiles` is subscribed.

```json
{
  "event": "profile:rotation:exhausted",
  "retry_after_secs": 300
}
```

| Field | Type | Description |
|-------|------|-------------|
| `retry_after_secs` | int | Seconds until the earliest profile becomes available again |


### `health`

Health check response. Sent in reply to `health:get`.

```json
{
  "event": "health",
  "status": "running",
  "session_id": "a1b2c3d4-...",
  "pid": 12345,
  "uptime_secs": 120,
  "agent": "claude",
  "terminal_cols": 120,
  "terminal_rows": 40,
  "ws_clients": 2,
  "ready": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `status` | string | Always `"running"` |
| `session_id` | string | Agent session ID (UUID) |
| `pid` | int or null | Child process PID |
| `uptime_secs` | int | Seconds since coop started |
| `agent` | string | Agent type (`"claude"`, `"codex"`, `"gemini"`, `"unknown"`) |
| `terminal_cols` | int | Terminal width |
| `terminal_rows` | int | Terminal height |
| `ws_clients` | int | Connected WebSocket clients |
| `ready` | bool | Whether the session is ready |


### `ready`

Readiness probe response. Sent in reply to `ready:get`.

```json
{
  "event": "ready",
  "ready": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `ready` | bool | Whether the session is ready |


### `agent`

Agent state response. Sent in reply to `agent:get`.

```json
{
  "event": "agent",
  "agent": "claude",
  "session_id": "a1b2c3d4-...",
  "state": "prompt",
  "since_seq": 15,
  "screen_seq": 42,
  "detection_tier": "tier1_hooks",
  "detection_cause": "hook:permission",
  "prompt": { "type": "permission", "..." : "..." },
  "error_detail": null,
  "error_category": null,
  "last_message": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `agent` | string | Agent type |
| `session_id` | string | Agent session ID (UUID) |
| `state` | string | Current agent state |
| `since_seq` | int | Sequence number when this state was entered |
| `screen_seq` | int | Current screen sequence number |
| `detection_tier` | string | Which detection tier produced this state |
| `detection_cause` | string | Freeform cause string from the detector |
| `prompt` | PromptContext or null | Prompt context (when state is `"prompt"`) |
| `error_detail` | string or null | Error text (when state is `"error"`) |
| `error_category` | string or null | Error classification (when state is `"error"`) |
| `last_message` | string or null | Last message extracted from agent output |


### `status`

Session status summary. Sent in response to `status:get`.

```json
{
  "event": "status",
  "session_id": "a1b2c3d4-...",
  "state": "running",
  "pid": 12345,
  "uptime_secs": 120,
  "exit_code": null,
  "screen_seq": 42,
  "bytes_read": 8192,
  "bytes_written": 256,
  "ws_clients": 2
}
```

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | string | Agent session ID (UUID) |
| `state` | string | `"starting"`, `"running"`, or `"exited"` |
| `pid` | int or null | Child process PID |
| `uptime_secs` | int | Seconds since coop started |
| `exit_code` | int or null | Exit code if exited |
| `screen_seq` | int | Current screen sequence number |
| `bytes_read` | int | Total bytes read from PTY |
| `bytes_written` | int | Total bytes written to PTY |
| `ws_clients` | int | Connected WebSocket clients |


### `replay`

Replay response. Sent in reply to a `replay:get` request.

```json
{
  "event": "replay",
  "data": "SGVsbG8gV29ybGQ=",
  "offset": 0,
  "next_offset": 1024,
  "total_written": 4096
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | string | Base64-encoded raw bytes |
| `offset` | int | Starting byte offset |
| `next_offset` | int | Byte offset after the returned data |
| `total_written` | int | Total bytes written to the ring buffer |


### `input:sent`

Confirmation that input was written to the PTY.
Sent in reply to `input:send`, `input:send:raw`, `keys:send`, and `mouse:send`.

```json
{
  "event": "input:sent",
  "bytes_written": 6
}
```

| Field | Type | Description |
|-------|------|-------------|
| `bytes_written` | int | Number of bytes written to the PTY |


### `nudged`

Result of a nudge request.

```json
{
  "event": "nudged",
  "delivered": true,
  "state_before": "idle",
  "reason": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `delivered` | bool | Whether the nudge was written to the PTY |
| `state_before` | string or null | Agent state at the time of the request |
| `reason` | string or null | Why the nudge was not delivered |


### `response`

Result of a respond request.

```json
{
  "event": "response",
  "delivered": true,
  "prompt_type": "permission",
  "reason": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `delivered` | bool | Whether the response was written to the PTY |
| `prompt_type` | string or null | Prompt type at the time of the request |
| `reason` | string or null | Why the response was not delivered |


### `signal:sent`

Confirmation that a signal was sent. Sent in reply to `signal:send`.

```json
{
  "event": "signal:sent",
  "delivered": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `delivered` | bool | Whether the signal was delivered to the process |


### `resized`

Confirmation that the PTY was resized. Sent in reply to `resize`.

```json
{
  "event": "resized",
  "cols": 120,
  "rows": 40
}
```

| Field | Type | Description |
|-------|------|-------------|
| `cols` | int | New column count |
| `rows` | int | New row count |


### `transcript:list`

Transcript list response. Sent in reply to `transcript:list`.

```json
{
  "event": "transcript:list",
  "transcripts": [
    {
      "number": 1,
      "timestamp": "2026-02-10T14:35:00Z",
      "line_count": 150,
      "byte_size": 8192
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `transcripts` | TranscriptMeta[] | List of transcript metadata |


### `transcript:content`

Single transcript content. Sent in reply to `transcript:get`.

```json
{
  "event": "transcript:content",
  "number": 1,
  "content": "{\"type\":\"assistant\",...}\n"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `number` | int | Transcript number |
| `content` | string | Full JSONL content of the transcript |


### `transcript:catchup`

Transcript catchup response. Sent in reply to `transcript:catchup`.

```json
{
  "event": "transcript:catchup",
  "transcripts": [],
  "live_lines": ["{\"type\":\"tool\",...}"],
  "current_transcript": 2,
  "current_line": 45
}
```

| Field | Type | Description |
|-------|------|-------------|
| `transcripts` | object[] | Completed transcripts since the cursor |
| `live_lines` | string[] | Lines from the current (unsaved) transcript |
| `current_transcript` | int | Current transcript number |
| `current_line` | int | Current line offset |


### `session:switched`

Session switch confirmation. Sent in reply to `session:switch`.

```json
{
  "event": "session:switched",
  "scheduled": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `scheduled` | bool | Whether the switch was scheduled |


### `stop:config`

Stop hook configuration. Sent in reply to `stop:config:get`.

```json
{
  "event": "stop:config",
  "config": { "mode": "auto", "prompt": "wait" }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `config` | object | Current StopConfig as JSON |


### `stop:configured`

Confirmation that stop config was updated. Sent in reply to `stop:config:put`.

```json
{
  "event": "stop:configured",
  "updated": true
}
```


### `stop:resolved`

Confirmation that stop was resolved. Sent in reply to `stop:resolve`.

```json
{
  "event": "stop:resolved",
  "accepted": true
}
```


### `start:config`

Start hook configuration. Sent in reply to `config:start:get`.

```json
{
  "event": "start:config",
  "config": { "text": "hello", "shell": ["echo hi"] }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `config` | object | Current StartConfig as JSON |


### `start:configured`

Confirmation that start config was updated. Sent in reply to `config:put:get`.

```json
{
  "event": "start:configured",
  "updated": true
}
```


### `error`

Error response to a client message.

```json
{
  "event": "error",
  "code": "BAD_REQUEST",
  "message": "unknown key: badkey"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `code` | string | Error code (same codes as HTTP API) |
| `message` | string | Human-readable error description |


### `pong`

Response to a client `ping`.

```json
{
  "event": "pong"
}
```


## Client → Server Messages


### `ping`

Keepalive ping. No auth required.

```json
{
  "event": "ping"
}
```

Server replies with `pong`.


### `auth`

Authenticate an unauthenticated connection. No auth required (this is the
auth mechanism itself).

```json
{
  "event": "auth",
  "token": "my-secret-token"
}
```

On success: no response (connection is now authenticated).
On failure: `error` message with code `UNAUTHORIZED`.


### `health:get`

Request the health check. No auth required.

```json
{
  "event": "health:get"
}
```

Server replies with a `health` message.


### `ready:get`

Request the readiness probe. No auth required.

```json
{
  "event": "ready:get"
}
```

Server replies with a `ready` message.


### `screen:get`

Request the current screen snapshot. Requires auth.

```json
{
  "event": "screen:get"
}
```

Server replies with a `screen` message.


### `agent:get`

Request the current agent state. Requires auth.

```json
{
  "event": "agent:get"
}
```

Server replies with an `agent` message.


### `status:get`

Request the current session status. Requires auth.

```json
{
  "event": "status:get"
}
```

Server replies with a `status` message.


### `replay:get`

Request raw output from a specific byte offset. **Requires auth.**

```json
{
  "event": "replay:get",
  "offset": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `offset` | int | Byte offset to start reading from |
| `limit` | int or null | Maximum bytes to return |

Server replies with a `replay` message containing the buffered data.


### `input:send`

Write UTF-8 text to the PTY. **Requires auth.**

```json
{
  "event": "input:send",
  "text": "hello",
  "enter": true
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `text` | string | required | Text to write to the PTY |
| `enter` | bool | `false` | Append carriage return (`\r`) after text |

Server replies with an `input:sent` message. Error on auth failure.


### `input:send:raw`

Write base64-encoded raw bytes to the PTY. **Requires auth.**

```json
{
  "event": "input:send:raw",
  "data": "SGVsbG8="
}
```

| Field | Type | Description |
|-------|------|-------------|
| `data` | string | Base64-encoded bytes |

Server replies with an `input:sent` message.


### `keys:send`

Send named key sequences to the PTY. **Requires auth.**

```json
{
  "event": "keys:send",
  "keys": ["ctrl-c", "enter"]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `keys` | string[] | Key names (see HTTP API key table for supported names) |

Server replies with an `input:sent` message. Error with `BAD_REQUEST`
if a key name is unrecognized.


### `mouse:send`

Send one mouse press or release. **Requires auth.** The fields are those of
`POST /api/v1/input/mouse` (see the HTTP API); coop encodes the report.

```json
{
  "event": "mouse:send",
  "button": "left",
  "col": 10,
  "row": 5
}
```

Server replies with an `input:sent` message (`bytes_written` 0 when the app
is not tracking the mouse). Error with `BAD_REQUEST` if the cell is off the
screen.


### `resize`

Resize the PTY. **Requires auth.**

```json
{
  "event": "resize",
  "cols": 120,
  "rows": 40
}
```

| Field | Type | Description |
|-------|------|-------------|
| `cols` | int | New column count (must be > 0) |
| `rows` | int | New row count (must be > 0) |

Server replies with a `resized` confirmation message. Error with
`BAD_REQUEST` if dimensions are zero.


### `nudge`

Send a follow-up message to the agent. **Requires auth.**
Only succeeds when the agent is in `idle` state.

```json
{
  "event": "nudge",
  "message": "Please continue"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `message` | string | Text message to send to the agent |

Server replies with a `nudged` message. Error on auth failure or if no
agent driver is configured.


### `respond`

Respond to an active prompt. **Requires auth.** Behavior depends on the current
agent state.

```json
{
  "event": "respond",
  "accept": true,
  "option": null,
  "text": null,
  "answers": []
}
```

| Field | Type | Description |
|-------|------|-------------|
| `accept` | bool or null | Accept/deny (permission and plan prompts). Overridden by `option` when set |
| `option` | int or null | 1-indexed option number for permission/plan/setup prompts |
| `text` | string or null | Freeform text (plan feedback) |
| `answers` | QuestionAnswer[] | Structured answers for multi-question dialogs |

See the HTTP API `POST /api/v1/agent/respond` for per-prompt behavior.

Server replies with a `response` message. Error on auth failure or if no
agent driver is configured.


### `signal:send`

Send a signal to the child process. **Requires auth.**

```json
{
  "event": "signal:send",
  "signal": "SIGINT"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `signal` | string | Signal name or number (see HTTP API signal table) |

Server replies with a `signal:sent` confirmation message. Error with
`BAD_REQUEST` if the signal is unrecognized.


### `shutdown`

Initiate graceful shutdown of the coop process. **Requires auth.**

```json
{
  "event": "shutdown"
}
```

Server replies with a `shutdown` confirmation message. The connection will
close as the server shuts down.


### `transcript:list`

List all transcript snapshots. **Requires auth.**

```json
{
  "event": "transcript:list"
}
```

Server replies with a `transcript:list` message.


### `transcript:get`

Get a single transcript's content. **Requires auth.**

```json
{
  "event": "transcript:get",
  "number": 1
}
```

| Field | Type | Description |
|-------|------|-------------|
| `number` | int | Transcript number to retrieve |

Server replies with a `transcript:content` message.


### `transcript:catchup`

Catch up from a cursor (transcript number + line offset). **Requires auth.**

```json
{
  "event": "transcript:catchup",
  "since_transcript": 0,
  "since_line": 0
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `since_transcript` | int | `0` | Return transcripts after this number |
| `since_line` | int | `0` | Return live lines after this offset |

Server replies with a `transcript:catchup` message.


### `session:switch`

Switch credentials and restart the agent process. **Requires auth.**

```json
{
  "event": "session:switch",
  "credentials": {
    "ANTHROPIC_API_KEY": "sk-ant-..."
  },
  "force": false
}
```

| Field | Type | Description |
|-------|------|-------------|
| `credentials` | object or null | Credential env vars to merge into the new child process |
| `force` | bool | Skip waiting for idle — SIGHUP immediately |

Server replies with a `session:switched` message. Error with
`SWITCH_IN_PROGRESS` if a switch is already in progress.


### `stop:config:get`

Read the current stop hook configuration. **Requires auth.**

```json
{
  "event": "stop:config:get"
}
```

Server replies with a `stop:config` message.


### `stop:config:put`

Update the stop hook configuration. **Requires auth.**

```json
{
  "event": "stop:config:put",
  "config": { "mode": "auto", "prompt": "wait" }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `config` | object | New StopConfig as JSON |

Server replies with a `stop:configured` message.


### `stop:resolve`

Resolve a pending stop gate so the agent is allowed to stop. **Requires auth.**

```json
{
  "event": "stop:resolve",
  "body": { "done": true }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `body` | JSON | Freeform signal body |

Server replies with a `stop:resolved` message.


### `config:start:get`

Read the current start hook configuration. **Requires auth.**

```json
{
  "event": "config:start:get"
}
```

Server replies with a `start:config` message.


### `config:put:get`

Update the start hook configuration. **Requires auth.**

```json
{
  "event": "config:put:get",
  "config": { "text": "hello", "shell": ["echo hi"] }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `config` | object | New StartConfig as JSON |

Server replies with a `start:configured` message.


## Shared Types


### CursorPosition

```json
{
  "row": 0,
  "col": 0
}
```

| Field | Type | Description |
|-------|------|-------------|
| `row` | int | 0-indexed row |
| `col` | int | 0-indexed column |


### PromptContext

```json
{
  "type": "permission",
  "subtype": "tool",
  "tool": "Bash",
  "input": "{\"command\":\"ls\"}",
  "options": ["Yes", "Yes, and don't ask again for this tool", "No"],
  "options_fallback": false,
  "questions": [],
  "question_current": 0,
  "ready": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Prompt type: `"permission"`, `"plan"`, `"question"`, `"setup"` |
| `subtype` | string or null | Further classification (see HTTP API for known subtypes) |
| `tool` | string or null | Tool name (permission prompts) |
| `input` | string or null | Truncated tool input JSON (permission prompts), or OAuth URL (setup `oauth_login` prompts) |
| `options` | string[] | Numbered option labels parsed from the terminal screen |
| `options_fallback` | bool | True when options are fallback labels (parser couldn't find real ones) |
| `questions` | QuestionContext[] | All questions in a multi-question dialog |
| `question_current` | int | 0-indexed current question; equals `questions.len()` at confirm phase |
| `ready` | bool | True when all async enrichment (e.g. option parsing) is complete |


### QuestionContext

```json
{
  "question": "Which database should we use?",
  "options": ["PostgreSQL", "SQLite", "MySQL"]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `question` | string | The question text |
| `options` | string[] | Available option labels |


### QuestionAnswer

```json
{
  "option": 1,
  "text": null
}
```

| Field | Type | Description |
|-------|------|-------------|
| `option` | int or null | 1-indexed option number |
| `text` | string or null | Freeform text (used when selecting "Other") |
