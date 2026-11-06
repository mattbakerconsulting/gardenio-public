# Coop vs Gastown

Coop provides the **session layer** (spawn, monitor, encode input).
Gastown provides the **orchestration layer** (polecats, witness, beads,
merge queue, multi-agent coordination).

```
Before:  Witness/Deacon → SessionManager → Tmux → tmux → Claude Code
                               ↓                    ↓
                        config beads → settings   pane-died hook
                        gt prime (SessionStart)   health check pings

After:   Witness/Deacon → CoopAdapter → HTTP/gRPC → coop → PTY → Claude Code
                                                       ↓
                                                multi-tier detection
```


## Session Management

| Capability           | GT | Coop |
| -------------------- | -- | ---- |
| Spawn                | ✓  | ✓    |
| Terminal rendering   | ✓  | ✓+   |
| Input injection      | ✓  | ✓    |
| Kill                 | ✓  | ✓    |
| Liveness / exit code | ✓  | ✓    |
| Output preservation  | ✓  | ✓    |
| Input serialization  | ✓  | ✓    |


## State Detection

| Signal                | GT | Coop |
| --------------------- | -- | ---- |
| Agent self-reporting  | ✓  | ✗    |
| Notification hook     | ✗  | ✓    |
| PreToolUse hook       | ✓  | ✓+   |
| PostToolUse hook      | ✓  | ✓    |
| Stop hook             | ✓  | ✓+   |
| SessionStart hook     | ✓  | ✓    |
| UserPromptSubmit hook | ✓  | ✓    |
| Session log watcher   | ✗  | ✓    |
| Stdout JSONL          | ✗  | ✓    |
| Process monitor       | ✓  | ✓    |
| Screen parsing        | ✗  | ✓    |
| Health check pings    | ✓  | ✗    |
| Idle detection        | ✓  | ✓+   |
| Active health pings   | ✓  | ✗    |


## Prompt Handling

Gastown agents run `--dangerously-skip-permissions` and don't encounter permission prompts during normal operation.
Coop supports that but is also designed to support scenarios where prompts need consumer approval.

| Prompt                    | GT | Coop |
| ------------------------- | -- | ---- |
| Permission detection      | ✗  | ✓    |
| Permission response       | ✗  | ✓    |
| AskUser detection         | ✗  | ✓    |
| AskUser response          | ✗  | ✓    |
| Plan detection            | ✗  | ✓    |
| Plan response             | ✗  | ✓    |
| Setup dialog detection    | ✗  | ✓    |
| Prompt context extraction | ✗  | ✓    |


## Startup Prompts

| Prompt             | GT | Coop |
| ------------------ | -- | ---- |
| Bypass permissions | ✓  | ✓    |
| Workspace trust    | ✗  | ✓    |
| Login/onboarding   | ✗  | ✓    |



## Input Encoding

| Action              | GT | Coop |
| ------------------- | -- | ---- |
| Nudge               | ✓  | ✓+   |
| Permission respond  | ✗  | ✓    |
| AskUser respond     | ✗  | ✓    |
| Plan respond        | ✗  | ✓    |
| Input debouncing    | ✓  | ✓    |


## Session Resume

| Aspect                | GT | Coop |
| --------------------- | -- | ---- |
| Resume conversation   | ✓  | ✓    |
| Predecessor discovery | ✓  | ✗    |
| Log offset recovery   | ✗  | ✓    |
| Credential switch     | ✗  | ✓+   |
| Multi-account         | ✓  | ✓    |


## Stop Gating

GT uses `bd decision stop-check` as a Claude hook — a per-turn guard that
creates a beads decision and blocks until resolved. The decision system is
external to the session manager.

Coop's `gate` mode provides the equivalent integration point:

1. Orchestrator configures `PUT /api/v1/config/stop` with `mode: "gate"` and a
   `prompt` referencing `bd decision create` (or any external resolution flow).
2. On each stop hook, coop returns the prompt verbatim as the block reason.
3. When the external decision resolves, the orchestrator calls
   `POST /api/v1/stop/resolve` to unblock.

GT can toggle `gate` mode on/off and resolve via HTTP as decisions are created
and resolved. Coop's `auto` mode is the batteries-included alternative that
generates `coop send` instructions for agents that self-resolve.


## Hooks & Settings Merging

GT passes hooks, permissions, env, plugins, and MCP servers via `--agent-config`.
Coop appends its detection hooks on top (GT first, coop second) and writes a single merged settings file.
