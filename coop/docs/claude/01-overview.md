# Claude Code Support

Coop provides first-class support for Claude Code via the `--agent claude` flag.
The Claude driver activates five detection tiers, hook-based event ingestion,
prompt response encoding, startup prompt handling, and session resume.

```
coop --agent claude --port 8080 -- claude --dangerously-skip-permissions
```


## Detection Tiers

When `--agent claude` is set, coop activates up to five detection tiers. Lower
tier numbers are higher confidence; the composite detector always prefers the
most-confident source.

| Tier | Source | Confidence | How it works |
|------|--------|------------|--------------|
| 1 | Hook events | Highest | Named pipe receives push events from Claude's hook system |
| 2 | Session log | High | File watcher tails `~/.claude/projects/<hash>/*.jsonl` |
| 3 | Stdout JSONL | Medium | Parses JSONL when Claude runs with `--print --output-format stream-json` |
| 4 | Process monitor | Low | Universal fallback: process alive, PTY activity, exit status |
| 5 | Screen parsing | Lowest | Terminal screen heuristics: setup dialogs, workspace trust, idle prompt |

Tier 5 detects interactive dialogs (onboarding, workspace trust, OAuth login)
and the idle prompt (`❯`). Tool permission dialogs are suppressed at this tier
since Tier 1 hooks handle them with higher confidence.

### Tier 1: Hook Events

Coop creates a named FIFO pipe before spawning Claude and writes a settings
file containing the hook configuration. Claude loads this via `--settings`.

Six hooks are registered:

- **SessionStart** (matcher: `""`) -- fires on session lifecycle events (startup, resume, clear, compact); curls `$COOP_URL/api/v1/hooks/start` for context injection
- **PostToolUse** (matcher: `""`) -- fires after each tool call, writes the tool name and payload
- **Stop** (matcher: `""`) -- fires when the agent stops; writes to FIFO then curls `$COOP_URL/api/v1/hooks/stop` for stop gating
- **Notification** (matcher: `"idle_prompt|permission_prompt"`) -- fires on idle and permission notifications
- **PreToolUse** (matcher: `"ExitPlanMode|AskUserQuestion|EnterPlanMode"`) -- fires before specific prompt-related tools
- **UserPromptSubmit** (matcher: `""`) -- fires when the user submits a prompt; used as a Working signal

The hooks execute shell commands that write JSON to `$COOP_HOOK_PIPE`:

```json
{
  "hooks": {
    "SessionStart": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf ... > \"$COOP_HOOK_PIPE\"; response=$(curl -sf $COOP_URL/api/v1/hooks/start ...); [ -n \"$response\" ] && eval \"$response\""
      }]
    }],
    "PostToolUse": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf '{\"event\":\"post_tool_use\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
      }]
    }],
    "Stop": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf ... > \"$COOP_HOOK_PIPE\"; curl -sf $COOP_URL/api/v1/hooks/stop ..."
      }]
    }],
    "Notification": [{
      "matcher": "idle_prompt|permission_prompt",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf '{\"event\":\"notification\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
      }]
    }],
    "PreToolUse": [{
      "matcher": "ExitPlanMode|AskUserQuestion|EnterPlanMode",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf '{\"event\":\"pre_tool_use\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
      }]
    }],
    "UserPromptSubmit": [{
      "matcher": "",
      "hooks": [{
        "type": "command",
        "command": "input=$(cat); printf '{\"event\":\"user_prompt_submit\",\"data\":%s}\\n' \"$input\" > \"$COOP_HOOK_PIPE\""
      }]
    }]
  }
}
```

State mapping:

| Hook event | Agent state |
|------------|-------------|
| `SessionStart` | (no state change — context injection only) |
| `TurnEnd` / `SessionEnd` | `Idle` |
| `ToolAfter` | `Working` |
| `Notification("idle_prompt")` | `Idle` |
| `Notification("permission_prompt")` | `Prompt(Permission)` |
| `ToolBefore("AskUserQuestion")` | `Prompt(Question)` with extracted context |
| `ToolBefore("ExitPlanMode")` | `Prompt(Plan)` |
| `ToolBefore("EnterPlanMode")` | `Working` |
| `TurnStart` | `Working` |

### Tier 2: Session Log Watching

Coop watches Claude's session log file for new JSONL entries. Each line is
parsed by `parse_claude_state()` to classify the agent's state.

Log discovery order:

1. `CLAUDE_CONFIG_DIR` environment variable
2. Default: `~/.claude/projects/<workspace-hash>/`
3. Watch for a new `.jsonl` file after spawn
4. Or: pass `--session-id <uuid>` to Claude for a known log path

When resuming a session, the log watcher starts from the byte offset where the
previous session left off, avoiding re-processing old entries.

### Tier 3: Structured Stdout

When Claude runs with `--print --output-format stream-json`, its stdout is a
JSONL stream. Coop feeds the raw PTY bytes through a JSONL parser and
classifies each entry with the same `parse_claude_state()` function. This tier
requires both flags to be present.

### Tier 4: Process Monitor

Universal fallback with no Claude-specific knowledge. Detects whether the
process is alive, whether the PTY has recent activity, and reports the exit
status. Provides coarse working-vs-idle detection.

### Tier 5: Screen Parsing

Polls the rendered terminal screen to detect interactive dialogs and the idle
prompt. Uses signal-phrase matching (2+ phrases must match) to classify known
dialog types:

| Dialog | Subtype | Classification |
|--------|---------|----------------|
| Workspace trust | `"trust"` | `Prompt(Permission)` |
| Theme picker | `"theme_picker"` | `Prompt(Setup)` |
| Terminal setup | `"terminal_setup"` | `Prompt(Setup)` |
| Security notes | `"security_notes"` | `Prompt(Setup)` |
| Login success | `"login_success"` | `Prompt(Setup)` |
| Login method | `"login_method"` | `Prompt(Setup)` |
| OAuth login | `"oauth_login"` | `Prompt(Setup)` |
| Settings error | `"settings_error"` | `Prompt(Setup)` |
| Fable usage-credit consent | `"fable_consent"` / `"fable_credits"` | `Prompt(Setup)` |
| Tool permission | -- | Suppressed (Tier 1 handles) |

The idle prompt is detected by scanning for the `❯` (U+276F) character at the
start of a non-empty line. Polls fast during startup, then backs off to a
slower steady-state cadence.


### Composite Detector

The `CompositeDetector` runs all active tiers concurrently and resolves
conflicts with these rules:

- **Terminal state (`Exited`)**: accepted immediately from any tier
- **Duplicate state**: suppressed (updates tier tracking only)
- **Same or higher confidence tier**: accepted immediately
- **Prompt supersedes**: Plan/Question/Setup prompts are not overwritten by a Permission prompt from the same tier (Claude fires both specific and generic hooks for the same moment)
- **Lower confidence tier, escalation**: accepted only if the new state has higher priority than the current state
- **Lower confidence tier, downgrade**: silently rejected

State priority (lowest to highest):

```
Starting/Unknown(0) < Idle(1) < Error/Parked(2) < Working(3) < Prompt(4) < Restarting/Exited(5)
```


## State Classification

Claude session log entries (Tiers 2 and 3) are classified into `AgentState`
values by `parse_claude_state()`:

```
parse_claude_state(json) ->
  error field present          => Error { detail }
  non-assistant message type   => Working
  assistant message with:
    tool_use "AskUserQuestion" => Prompt { Question + context }
    other tool_use             => Working
    thinking block             => Working
    text-only content          => Idle
    empty content              => Idle
```

The full set of agent states:

| State | Wire name | Meaning |
|-------|-----------|---------|
| `Starting` | `starting` | Initial state before first detection |
| `Working` | `working` | Executing tool calls or thinking |
| `Idle` | `idle` | Idle, ready for a nudge |
| `Prompt` | `prompt` | Presenting a prompt (permission, plan, question, or setup) |
| `Error` | `error` | Error occurred (rate limit, auth, etc.) |
| `Parked` | `parked` | Rate-limited and waiting; carries `reason` and `resume_at_epoch_ms` |
| `Restarting` | `restarting` | Credential switch or restart in progress (PTY respawn) |
| `Exited` | `exited` | Child process exited |
| `Unknown` | `unknown` | State cannot be determined |

The `Prompt` state carries a `PromptContext` with a `kind` field:

| PromptKind | Meaning |
|------------|---------|
| `permission` | Tool permission or workspace trust |
| `plan` | Plan mode exit dialog |
| `question` | `AskUserQuestion` multi-question dialog |
| `setup` | Onboarding/setup dialog |


## Prompt Context

When the agent enters a `Prompt` state, coop extracts structured context from
the session log, hooks, or screen.

`PromptContext` fields:

| Field | Type | Description |
|-------|------|-------------|
| `type` | `PromptKind` | Prompt kind: permission, plan, question, setup |
| `subtype` | `string?` | Further classification (see below) |
| `tool` | `string?` | Tool name (e.g. `"Bash"`, `"AskUserQuestion"`) |
| `input` | `string?` | Truncated tool input preview (~200 chars), or OAuth URL (setup `oauth_login`) |
| `options` | `string[]` | Numbered option labels parsed from the screen |
| `options_fallback` | `bool` | True when options are fallback labels |
| `questions` | `QuestionContext[]` | All questions in a multi-question dialog |
| `question_current` | `int` | 0-indexed active question; `== questions.len()` means confirm phase |
| `ready` | `bool` | True when async enrichment (option parsing) is complete |

**Permission prompts** start with `ready: false`; enriched asynchronously from
the terminal screen to populate `options`. Subtype `"trust"` for workspace
trust (detected via screen); no subtype for tool permissions (detected via
Notification hook).

**Question prompts** are `ready: true` immediately. Context is extracted from
the `AskUserQuestion` tool input (via PreToolUse hook or session log), which
provides the `questions` array with question text and option labels.

**Plan prompts** start with `ready: false`; enriched from the screen to
populate `options`.

**Setup prompts** are `ready: true` immediately. Detected entirely via Tier 5
screen classification. Interactive dialog subtypes: `theme_picker`,
`terminal_setup`, `security_notes`, `login_success`, `login_method`,
`oauth_login`, `settings_error`, `fable_consent`, `fable_credits`.
Text-based startup subtypes:
`startup_trust`, `startup_bypass`, `startup_login`.


## Encoding

Coop encodes nudge messages and prompt responses as PTY byte sequences written
to Claude's terminal input.

### Nudge

Sends a plain-text message followed by carriage return:

```
{message}\r
```

Only succeeds when the agent is in `Idle`.

The delay between the message and `\r` scales with message length:

```
delay = base + max(0, len - 256) * per_byte_factor
capped at max_delay
```

Defaults: base 200ms, per-byte 1ms, max 5s. After delivery, a background
monitor waits for a state transition to `Working`. If none arrives within
`COOP_NUDGE_TIMEOUT_MS` (default 4s), `\r` is resent once. The retry is
cancelled by any state transition, PTY input activity, or the next delivery.

### Prompt Responses

| Prompt type | Action | Bytes |
|-------------|--------|-------|
| Permission | Select option N | `{n}\r` |
| Plan | Select option 1-3 | `{n}\r` |
| Plan | Option 4 (feedback) | `{n}\r` + 100ms delay + `{feedback}\r` |
| Question | Single question, one answer | `{n}\r` |
| Question | Multi-question, single answer | `{n}` (TUI auto-advances, no Enter) |
| Question | Multi-question, all answers | `{n1}` + 100ms + `{n2}` + 100ms + ... + `\r` |
| Question | Freeform text | `{text}\r` |
| Setup | Select option N | `{n}\r` |


## Startup Prompts

Claude may present blocking prompts during startup before reaching the idle
state. Coop handles these through Tier 5 screen detection:

**Text-based prompts** (workspace trust y/n, permission bypass y/n, login)
are detected via broad phrase matching and reported as `Prompt(Setup)` states
with `startup_*` subtypes. These are checked after interactive dialog
classification since some phrases overlap (e.g. "trust this folder" appears
in both the y/n prompt and the workspace trust picker).

| Prompt | Subtype | Detection pattern |
|--------|---------|-------------------|
| Workspace trust | `"startup_trust"` | "trust the files", "do you trust" |
| Permission bypass | `"startup_bypass"` | "skip permissions", "allow tool use without prompting" |
| Login required | `"startup_login"` | "please sign in", "login required" |

Coop does **not** auto-respond to text-based prompts (no reliable keystroke
encoding). With `--groom auto`, interactive dialogs are auto-dismissed; text
prompts still require the orchestrator to respond via the API.

**Interactive dialogs** (theme picker, terminal setup, OAuth login, workspace
trust picker, etc.) are classified by Tier 5 signal-phrase matching and
reported as `Prompt(Setup)` or `Prompt(Permission)` states with subtypes.
The orchestrator responds via the API (or `--groom auto` auto-dismisses).


## Session Resume

When coop restarts, it can reconnect to a previous Claude conversation. The
`--resume` flag triggers session discovery:

1. **Discover** the most recent `.jsonl` log in `~/.claude/projects/<workspace-hash>/`
2. **Derive** the session ID from the log file stem (filename without extension)
3. **Parse** the log to recover the last agent state and byte offset
4. **Append** `--resume <id>` to Claude's command-line arguments
5. **Append** `--settings <path>` so hooks are active in the new process
6. **Start** the log watcher from the recovered byte offset

This spawns a new Claude process that loads the previous conversation history,
then resumes log watching from where the previous coop session left off.


## Stop Hook Gating

The Stop hook serves dual purposes: detection and gating.

1. **Detection**: writes `{"event":"stop","data":...}` to the FIFO pipe → Tier 1 idle signal
2. **Gating**: curls `$COOP_URL/api/v1/hooks/stop` for a verdict

The gating endpoint returns either an empty response (allow) or a block
verdict with a reason message. When blocked, the hook outputs the reason to
Claude, which continues working. When allowed, the hook exits normally and
Claude stops.

Stop gating is configured via `StopConfig`:
- **Mode `allow`** (default): always allow the agent to stop
- **Mode `auto`**: block until signaled, with generated `coop send` instructions
- **Mode `gate`**: block until signaled, with prompt returned verbatim (orchestrator-driven)

Both `/api/v1/hooks/stop` and `/api/v1/stop/resolve` are auth-exempt
since they are called from inside the PTY.


## Start Hook (Context Injection)

The SessionStart hook fires on session lifecycle events (startup, resume,
clear, compact). The hook script writes to the FIFO pipe for detection, then
curls `$COOP_URL/api/v1/hooks/start` for context injection.

The start endpoint composes a shell script from the `StartConfig`:
- **`text`**: static context delivered as `printf '%s' '<base64>' | base64 -d`
- **`shell`**: commands appended line-by-line
- **`event`**: per-source overrides (e.g. different injection for "clear" vs "resume")

Lookup: match `event[source]` first → fall back to top-level `text`/`shell` →
empty means no injection. The response is plain text (shell script), not JSON.
The hook `eval`s the response.

Start config is set via `--agent-config` JSON file (key: `start`) or at
runtime via `PUT /api/v1/config/start`.

`/api/v1/hooks/start` is auth-exempt since it is called from inside the PTY.


## Programmatic Settings & MCP Configuration

Orchestrators can inject agent-level configuration — hooks, permissions, MCP
servers, plugins — via the `--agent-config` JSON file. Two fields control this:

### `settings` (Agent Settings)

Orchestrator settings (hooks, permissions, env, plugins) are merged with coop's
generated hook configuration. Orchestrator hooks form the **base layer**; coop's
detection hooks are **appended on top**.

```json
{
  "settings": {
    "hooks": {
      "SessionStart": [{"matcher": "", "hooks": [{"type": "command", "command": "gt-prime-context"}]}],
      "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "gt-sandbox-guard"}]}]
    },
    "permissions": { "allow": ["Bash", "Read", "Write"] },
    "env": { "GT_WORKSPACE_ID": "ws-123" }
  }
}
```

Merge rules:
- **`hooks`**: per hook type, arrays are concatenated (orchestrator first, coop second)
- **All other keys** (`permissions`, `env`, `enabledPlugins`, etc.): pass through unchanged

### `mcp` (MCP Server Definitions)

A map of server name to server config. Coop handles the agent-specific plumbing:

- **Claude**: writes `{"mcpServers": ...}` to the session dir and passes it
  via `--mcp-config` (avoids polluting the project root)
- **Gemini**: merges as `mcpServers` into the settings file delivered via
  `GEMINI_CLI_SYSTEM_SETTINGS_PATH`

```json
{
  "mcp": {
    "my-server": {
      "command": "node",
      "args": ["server.js"],
      "env": {}
    }
  }
}
```

Both fields are structural — the agent reads them once at startup from files.
There are no runtime API endpoints for settings or MCP; changes require a
session restart.


## Environment Variables

Coop sets the following environment variables on the Claude child process:

| Variable | Purpose |
|----------|---------|
| `COOP=1` | Marker that the process is running under coop |
| `COOP_HOOK_PIPE` | Path to the named FIFO for hook events |
| `COOP_URL` | Base URL of coop's HTTP server (used by stop hook) |
| `TERM=xterm-256color` | Terminal type for the child PTY |


## Transcript Snapshots

When Claude compacts its context window, the full conversation history in the
session log would be overwritten. Coop preserves it by snapshotting the session
log before each compaction.

**Trigger**: The `SessionStart` hook fires with `source="compact"`. Coop's
start hook handler spawns an async task that copies the session log to
`sessions/<id>/transcripts/{N}.jsonl` (N increments from 1).

**Storage**: Each snapshot is an immutable copy of the full session JSONL at
that point in time. On session resume, existing snapshots are discovered and
numbering continues.

**API**: Transcripts are served over all three transports:

| Endpoint | HTTP | gRPC | WebSocket |
|----------|------|------|-----------|
| List snapshots | `GET /api/v1/transcripts` | `ListTranscripts` | `transcript:list` |
| Get content | `GET /api/v1/transcripts/{N}` | `GetTranscript` | `transcript:get` |
| Catchup | `GET /api/v1/transcripts/catchup` | `CatchupTranscripts` | `transcript:catchup` |
| Live events | — | `StreamTranscriptEvents` | `transcript:saved` (subscription) |

**Catchup**: Clients track position with a `(since_transcript, since_line)`
cursor. The catchup endpoint returns all transcripts after `since_transcript`
and all live session log lines after `since_line`, enabling incremental sync.

**Broadcast**: Each snapshot emits a `TranscriptEvent` with the transcript
number, timestamp, line count, and a monotonic sequence number.


## CLI Flags

Flags relevant to Claude sessions:

| Flag | Default | Description |
|------|---------|-------------|
| `--agent claude` | -- | Enable Claude-specific detection and encoding |
| `--resume HINT` | -- | Discover and resume a previous session |
| `--groom LEVEL` | `auto` | Prompt handling: `auto` (auto-dismiss), `manual`, `pristine` (no hooks) |
| `--agent-config PATH` | -- | JSON file with orchestrator settings, hooks, and MCP servers |
| `--socket PATH` | -- | Unix domain socket for HTTP transport |
| `--port-grpc PORT` | -- | gRPC port (separate from HTTP) |


## Source Layout

```text
crates/cli/src/driver/claude/
├── mod.rs           # ClaudeDriver: wires up detectors and encoders
├── stream.rs        # HookDetector (T1), LogDetector (T2), StdoutDetector (T3)
├── screen.rs        # ClaudeScreenDetector (T5): dialog classification, idle prompt
├── parse.rs         # parse_claude_state() — JSONL → AgentState
├── hooks.rs         # Hook config generation, environment setup
├── setup.rs         # Pre-spawn session preparation (FIFO, settings, args)
├── prompt.rs        # PromptContext extraction, option parsing from screen
├── encoding.rs      # ClaudeNudgeEncoder, ClaudeRespondEncoder
└── resume.rs        # Session log discovery, state recovery, --resume args
```
