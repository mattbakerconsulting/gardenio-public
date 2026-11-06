# Coop vs Oddjobs Claude Adapter

Coop provides the **session layer** (spawn, monitor, encode input).
Oddjobs provides the **orchestration layer** (jobs, decisions, stuck recovery).

```
Before:  Engine → ClaudeAgentAdapter → TmuxAdapter → tmux → Claude Code
                          ↓
                   Watcher (file)  →  session log JSONL

After:   Engine → CoopAdapter → HTTP/gRPC → coop → PTY → Claude Code
                                               ↓
                                        multi-tier detection
```


## Session Management

| Capability           | OJ | Coop | Notes             |
| -------------------- | -- | ---- | ----------------- |
| Spawn                | ✓  | ✓    |                   |
| Terminal rendering   | ✓  | ✓+   | VTE-parsed screen |
| Input injection      | ✓  | ✓    |                   |
| Kill                 | ✓  | ✓    |                   |
| Liveness / exit code | ✓  | ✓    |                   |
| Output preservation  | ✓  | ✓    |                   |


## State Detection

| Signal              | OJ | Coop |
| ------------------- | -- | ---- |
| Notification hook   | ✓  | ✓    |
| PreToolUse hook     | ✓  | ✓    |
| PostToolUse hook    | ✗  | ✓    |
| UserPromptSubmit    | ✗  | ✓    |
| Stop hook           | ✓  | ✓    |
| SessionStart hook   | ✓  | ✓    |
| Session log watcher | ✓  | ✓    |
| Stdout JSONL        | ✗  | ✓    |
| Process monitor     | ✓  | ✓    |
| Screen parsing      | ✗  | ✓    |
| Idle detection      | ✓  | ✓+   |



## Prompt Handling

| Prompt                    | OJ | Coop | Notes                        |
| ------------------------- | -- | ---- | ---------------------------- |
| Permission detection      | ✓  | ✓    |                              |
| Permission response       | ✓  | ✓    |                              |
| AskUser detection         | ✓  | ✓    |                              |
| AskUser response          | ✓  | ✓+   | Adds multi-question encoding |
| Plan detection            | ✓  | ✓    |                              |
| Plan response             | ✓  | ✓    |                              |
| Setup dialog detection    | ✗  | ✓    | Tier 5 screen classification |
| Setup dialog response     | ✗  | ✓    |                              |
| Prompt context extraction | ✗  | ✓    |                              |


## Startup Prompts

| Prompt             | OJ | Coop | Notes                                    |
| ------------------ | -- | ---- | ---------------------------------------- |
| Bypass permissions | ✓  | ✓    |                                          |
| Workspace trust    | ✓  | ✓    |                                          |
| Login/onboarding   | ✗  | ✓    | Extracts login link, exposes via API     |


## Input Encoding

| Action           | OJ | Coop | Notes                              |
| ---------------- | -- | ---- | ---------------------------------- |
| Nudge            | ✓  | ✓    |                                    |
| Delay scaling    | ✗  | ✓    |                                    |
| Nudge retry      | ✗  | ✓    | Resend `\r` if no state transition |
| Input clearing   | ✓  | ✗    |                                    |
| Input debouncing | ✗  | ✓    | 200ms min gap between deliveries   |


## Session Resume

| Aspect               | OJ | Coop |
| -------------------- | -- | ---- |
| Resume conversation  | ✓  | ✓    |
| Session ID tracking  | ✓  | ✓    |
| Log offset recovery  | ✗  | ✓    |
| Daemon reconnect     | ✓  | ✓    |
| Suspend/resume       | ✓  | ✗    |
| Credential switch    | ✗  | ✓    |
| Transcript snapshots | ✗  | ✓    |

Oddjobs has job-level suspension (`StepStatus::Suspended`) that pauses state
processing while keeping the tmux session alive.

Coop has no equivalent; consumers ignore events to achieve the same effect.


## Hooks & Settings Merging

OJ passes hooks, permissions, and MCP servers via `--agent-config`.
Coop appends its detection hooks on top (OJ first, coop second).
