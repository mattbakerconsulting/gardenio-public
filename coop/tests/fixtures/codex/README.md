# Codex fixtures

Captured from `codex-cli 0.162.1` (rust-v0.162.1, x86_64 musl) inside a gardenio agent pod by spike
gd-JegnDXa3nE; findings in `gardenio/docs/research/codex-harness/spike.md`. Paths are scrubbed
(`$CODEX_HOME`, `/workspace`).

| File | What it is | Upstream |
|---|---|---|
| `exec-turn-failed-401.jsonl` | `codex exec --json` stream for a turn that fails (`thread.started`, `item.completed{error}`, `turn.started`, `error` x N, `turn.failed`) | REAL api.openai.com, bogus key |
| `exec-tool-stop-gate.jsonl` | `codex exec --json` stream with a tool call (`command_execution` started/completed), two `agent_message`s (the Stop hook blocked once) and `turn.completed{usage}` | local mock Responses server |
| `app-server-session.jsonl` | `codex app-server` stdio JSON-RPC transcript: initialize, thread/start, turn/start, notifications, turn/interrupt, hooks/list | local mock Responses server |
| `hook-payloads.jsonl` | Exact stdin JSON each hook received: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop (x2, `stop_hook_active` false then true) | local mock Responses server |
| `rollout-sample.jsonl` | A `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` file with the long prompt text trimmed | local mock Responses server |

The mock-backed files use the real Codex event encoder; only the model's replies are scripted. A
successful real-model stream (including reasoning items and a real `usage` block) is still
pending, see the spike's pending list.
