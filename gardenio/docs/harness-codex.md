# Codex harness: what runs where, and how to debug a Codex agent

Operator and developer guide for `harness=codex` agents. Design and decisions: [ADR 0004](../../docs/adr/0004-codex-harness.md). Evidence from the first end-to-end run: [codex-dogfood-evidence.md](codex-dogfood-evidence.md). Task: gd-0D8aGbRwYp.

A Codex agent is the same pod, the same beads, Slack card, decisions, escalations and stop gate as a Claude agent; only the agent CLI, its credentials and its translated config differ.

## What runs where

| Piece | Where | Claude agent | Codex agent |
|---|---|---|---|
| Harness resolution | controller, once at spawn (`resolveHarness`, `cmd/controller/podspec_harness.go`) | `claude` | agent bead `harness` > role > project > `AGENT_DEFAULT_HARNESS`; result stamped on the agent bead as `resolved_harness` |
| Account | controller (`selectAccount`) | non-`openai` provider | `provider=openai`, `billing_type=subscription`, pinned on the role |
| Credentials | keeper in the controller ([codex-subscription-keeper.md](codex-subscription-keeper.md)) | OAuth token / LiteLLM key | `auth.json` session copy (placeholder `refresh_token`) mounted read-only, copied to `$CODEX_HOME/auth.json` |
| Pod launch | `gb init` / `gb agent start` (`internal/harness`) | `claude --dangerously-skip-permissions` | `codex app-server`, driven by coop over JSON-RPC (`coop --agent codex`) |
| Config | `gb setup codex` ([codex-config-injection.md](codex-config-injection.md)) | `~/.claude/settings.json` | `$CODEX_HOME/config.toml`, `AGENTS.md`, `hooks.json` |
| Hooks | `gb hook relay` ([codex-hooks.md](codex-hooks.md)) | Claude hook events | translated `hooks.json`, same relay, `harness: "codex"` on every event |
| State dir | workspace PVC subPath | `~/.claude` | `~/.codex` (= `$CODEX_HOME`) |
| Screen | beads-viewer / Slack | PTY screen | no TUI; coop's normalised transcript |

## Env vars in a Codex pod

| Var | Set by | Meaning |
|---|---|---|
| `GARDENIO_HARNESS=codex` | controller (`shapeHarnessEnv`) | Read by `gb` (`internal/harness.FromEnv`), the stop gate and the hook relay. An unknown value is an error, never a silent fallback to claude. |
| `CODEX_HOME` | controller | State dir; default `~/.codex`. |
| `CODEX_MODEL` | controller, from the resolved model | Becomes `model` in `config.toml`; empty means Codex picks its default. `ANTHROPIC_MODEL`, `CLAUDE_MODEL` and the `ANTHROPIC_DEFAULT_*_MODEL` pins are removed. |
| `CODEX_REASONING_EFFORT` | role/agent env | `minimal|low|medium|high|xhigh` -> `model_reasoning_effort`. |
| `CODEX_CA_CERTIFICATE` | optional | Custom CA bundle for the ChatGPT backend. |

Every other `CODEX_*` / `OPENAI_*` variable is stripped from coop's child environment. There is deliberately no `OPENAI_API_KEY`: an inherited key would silently switch Codex to per-token billing.

## The `config.toml` we write

Regenerated wholesale at each pod start by `gb setup codex`. Built-in defaults: `approval_policy = "never"`, `sandbox_mode = "danger-full-access"`, the workspace as a trusted project, `project_doc_fallback_filenames = ["CLAUDE.md"]`, `cli_auth_credentials_store = "file"`, `check_for_update_on_startup = false`, history saved, `[features] hooks = true`, `[shell_environment_policy] inherit = "all"`, plus `[mcp_servers.*]` translated from `.mcp.json`. Override with `codex-config` / `codex-instructions` beads. Full reference: [codex-config-injection.md](codex-config-injection.md). The golden is `cmd/gb/testdata/codex_config_crew.golden.toml`.

## Choosing Codex

- **Role/project default:** `kd create '<role>' --type=role -l project:<p> -f harness=codex -f account=<openai-account>`.
- **One CLI spawn:** `gb spawn --harness codex`.
- **One Slack thread:** `@bot --harness codex <prompt>` or the shorthand `@bot --model codex <prompt>`; add a model with `--harness codex --model <gpt-id>`.
  - The channel's role must allow it: a role allows its own resolved harness plus anything in its `allowed_harnesses` field (comma list, e.g. `codex`). A default role is claude-only and the bot answers with the allowed set and how to widen it; it does not spawn.
  - Claude aliases (`opus`, `sonnet`, `fable`, `glm`, full `claude-*` ids) are refused on Codex, and Codex ids (`gpt-*`, `o3`, `codex-*`) are refused without `--harness codex`. A Claude id on Codex would degrade silently (Codex accepts unknown ids), so it is never passed through. There are no short Codex aliases yet: ADR 0004 pins no concrete id until the real list is read from a logged-in pod.
  - The override skips the warm pool (the pod bakes its harness at create time), is stamped on the agent bead, and survives restart and resume. A resumed thread cannot switch harness; start a new thread.

## Observability

- `gardenio_tool_latency_seconds` and `gardenio_bash_cmd_latency_seconds` carry `harness="claude|codex|unknown"` (from the `harness` field on every relayed hook event; events from a `gb` that predates the field count as `claude`).
- `gardenio_tool_harness_overhead_seconds` (pushed by `gb init`) carries `harness`.
- beads-viewer shows a `codex` chip on agent list rows and a `harness` badge on the agent detail card, from `resolved_harness`; role hubs already list the role's `harness`.
- Dashboard split: the per-harness panels need the `harness` label in the Grafana JSON (helm lane; see the escalation on gd-0D8aGbRwYp).

## Debugging a Codex agent

Start with the bead: `kd show <agent-bead>` -> `resolved_harness`, `harness`, `model`, `agent_state`, `last_session_id`. A spawn refused for a harness/account mismatch fails the bead with the reason (`invalid agent harness: ...`); nothing is escalated, fix the role/account.

In the pod (you have no kubectl; `gb spawn --role infra --escalate` gets read-only cluster access):

1. **Binary and version:** `codex --version`. The image pins the version in `images/agent/install/codex.sh`; the dogfood ran `codex-cli 0.162.1`.
2. **Auth:** `ls -l $CODEX_HOME/auth.json` (0600, present). A Codex that attempts its own refresh with the placeholder token fails harmlessly; a real 401 means the keeper has not pushed a fresh session, check the keeper runbook and the account's `probe_auth_status`.
3. **Config as Codex sees it:** `cat $CODEX_HOME/config.toml $CODEX_HOME/AGENTS.md`; `codex --strict-config` rejects unknown keys.
4. **Codex's own log:** `$CODEX_HOME/log/` (the CLI writes its TUI/app-server log there).
5. **Rollout (the session record):** `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local-ts>-<session-id>.jsonl`. The session id equals the `thread_id` coop reports. The rollout alone is enough to resume in a fresh `$CODEX_HOME`, which is what the persistence upload relies on.
6. **Hooks:** `cat $CODEX_HOME/hooks.json` and the seeded `[hooks.state]` in `config.toml`; an untrusted hook is skipped silently by Codex. Setup logs `[setup] warning: codex hooks: ...` when translation failed.
7. **Relay payloads:** `gb hook relay` is a one-shot process fed the hook JSON on stdin. Replay a captured payload with `echo '<payload>' | gb hook relay` and compare to the fixtures in `cmd/gb/hook_relay_codex_test.go`. On the bus, subscribe to `hooks.<agent>.>`; Codex events have `harness: "codex"`, `session_id` = thread id, `turn_id` mapped to `prompt_id` on tool events.
8. **coop's view:** `curl localhost:<port>/api/v1/agent` shows driver state (working/idle/error); an approval request is declined and reported as an `error` state.
9. **Stop gate:** `gb hook stop-gate --format codex` emits JSON on stdout with exit 0 (Claude uses exit 2 + stderr); the default follows `GARDENIO_HARNESS`.

## Skills

`gb setup codex` links the repo's `.claude/skills/*` into `$CODEX_HOME/skills/`, honouring the role's `enabled_skills` / `disabled_skills` exactly as for Claude ([ADR 0004 §13](../../docs/adr/0004-codex-harness.md)). Check with `ls -l $CODEX_HOME/skills`; the setup log prints `linked ...` / `skipping skills ...` lines.

## Not in v1 (follow-up beads)

ChatGPT-plan auth via device code in-pod; a recording proxy for OpenAI traffic (designed, not built: [ADR 0006](../../docs/adr/0006-codex-recording-proxy.md)); Codex on the `lambda_microvm` execution target (refused at spawn today, with a permanent, readable `invalid agent harness` reason; see ADR 0004 addendum A1 for the follow-up shape); a Gemini driver to the same bar (built; see [harness-gemini.md](harness-gemini.md) and [ADR 0005](../../docs/adr/0005-gemini-harness.md)). Each is tracked as a bead under the Codex rollout epic.

## Ultracode on Codex

There is no Codex equivalent of the Claude `BOAT_ULTRACODE` knob. `--ultracode=on` on a codex agent is refused at spawn with a message naming the fix (`--harness claude`); `--ultracode=off` and project/role-level ultracode config are ignored on codex. For parallel work on Codex, spawn more agents with `gb spawn`. Rationale: ADR 0004 section 13.
