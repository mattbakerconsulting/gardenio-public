# Codex hooks (`$CODEX_HOME/hooks.json`)

How a `harness=codex` agent gets the gardenio workflow plumbing (prime, stop gate, tool relay, mail). Design: [ADR 0004](../../docs/adr/0004-codex-harness.md) §E.1 and §8. Task: gd-mYF3T8Qxnn. Verified against `codex-cli 0.162.1`.

## Pipeline

`gb setup codex` runs this after it writes `config.toml` (it regenerates that file wholesale), in `cmd/gb/setup_codex_hooks.go`:

1. **Source.** The resolved `claude-hooks` config bead, else `defaultHookSettings()`, plus the JIRA hooks when JIRA credentials are set. One definition feeds both harnesses.
2. **Translate** (`buildCodexHooks`) into `$CODEX_HOME/hooks.json`.
3. **Seed trust.** Start `codex app-server`, call `hooks/list`, write `[hooks.state."<key>"] trusted_hash = "<currentHash>"` for each of *our* hooks into `config.toml`, then list again and require `trusted`.

A failure is logged (`[setup] warning: codex hooks: ... Codex will skip every gardenio hook`) and never aborts pod boot.

## Translation

| Claude event | Codex | Notes |
|---|---|---|
| `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop` | same | |
| `PreCompact`, `SubagentStart`, `SubagentStop`, `PermissionRequest` | same | CamelCase accepted; `hooks/list` reports `pre_compact`, `subagent_start`, ... |
| `PostToolUseFailure`, `StopFailure`, `TaskCreated`, `TaskCompleted`, `TeammateIdle`, `CwdChanged`, `FileChanged`, `ConfigChange`, `InstructionsLoaded`, `Elicitation*`, `Worktree*`, `Notification`, `Setup` | dropped | No Codex event. Logged at setup. |

- Claude's empty `matcher` is omitted (Codex: all). Non-empty matchers (the JIRA `Bash` hooks) pass through; Codex matches them against `tool_name`.
- **`async: true`** on every `gb hook relay` handler so observability never delays a turn. Prime, load-state, check-mail and the stop gate stay synchronous.
- The stop gate command becomes `gb hook stop-gate --format codex`.
- **Dropped handlers:** `/hooks/drain-queue.sh` (PostToolUse) and `gb hook prime --enqueue` (PreCompact). Codex ignores plain stdout from `PostToolUse` (probed: the text never reached the model), so drain-queue would delete the queue and lose the text. Re-priming after compaction is delivered by `SessionStart` `gb hook prime`, which is unmatched and so also fires for `source=compact`.
- Stdout from `SessionStart` reaches the model (probed), which is what `gb hook prime` and `check-mail` rely on.

Golden: `cmd/gb/testdata/codex_hooks.golden.json` (`go test ./cmd/gb -run CodexHooks -update-codex-hooks-golden` rewrites it).

### Command allowlist

The hook source can be the `claude-hooks` config bead, which any agent token can
write, and Codex pods hold the ChatGPT token. `buildCodexHooks` therefore
translates only commands matching `isAllowedCodexHookCommand`: the fixed `gb hook
<name>` subcommands gardenio registers, `gb workspace audit`, and the fixed
`/hooks/<script>.sh` files the agent image ships, each optionally followed by
exactly ` 2>/dev/null || true`. Flags are allowlisted **per command**
(`codexHookCommands`: `prime --enqueue`, `stop-gate --format claude|codex`,
`drain-queue.sh --quiet`, nothing else), so gb's root persistent flags
(`--http-url`, `--token`, `--operated-by`, `--actor`, `--ziti-*`), which would
send the pod bearer token to another host, never pass. Positional arguments are
refused. Anything else (`sh -c curl ...`, `gb hook relay; ...`, `$(...)`,
`gb hook relay --http-url=evil`, other paths) is dropped
and logged as `[setup] warning: dropped non-allowlisted codex hook`. The trust
seeder applies the same check to the `command` that `hooks/list` reports, so a
hook edited into hooks.json after we wrote it is never given a `trusted_hash`.

## Stop gate

Claude: block = exit 2 with `{"decision":"block","reason":...}` on stderr. Codex: that JSON as the **only** stdout and exit **0**; plain stdout on Stop is invalid for Codex and exit 2 is untested there. The checkpoint prose Claude gets on stdout (`stop-gate-text.md`, system-reminders) is folded into the JSON `reason`, which Codex returns to the model as a `<hook_prompt>` message.

`--format claude|codex`, default from `GARDENIO_HARNESS`, on both `gb hook stop-gate` (`renderStopGateBlock`) and `images/agent/hooks/stop-gate.sh`. All gate logic (yield marker, cooldown, escape hatch, claim guard, wrap-up gate) is shared; only the final emit differs. Tests: `hook_stopgate_codex_test.go`, `images/agent/hooks/stop-gate_test.sh`.

## Relay and session ids

`gb hook relay` accepts the Codex payload (`session_id`, `turn_id`, `transcript_path` = rollout file, `tool_name` `Bash`/`apply_patch`/`mcp__server__tool`, `permission_mode`) and stamps `harness: "codex"` (from `GARDENIO_HARNESS`, else sniffed from `turn_id` / a `rollout-*` transcript). `turn_id` fills `prompt_id` on tool events. `session_id` was already the relay's field, so NATS consumers (`bridge/hookstream.go`, tool latency) are unchanged; see [hook-event-schema.md](hook-event-schema.md).

`POST /v1/hooks/emit` (stop gate, `gb bus emit`) now also sends `session_id` and `harness`. kbeads prefers `session_id` and falls back to `claude_session_id`, which is still sent, so old and new gb/kbeads interoperate in either order. Test: `TestBuildRelayEvent_CodexPayloads` over the spike capture `cmd/gb/testdata/codex_hook_payloads.jsonl`.

## Trust: seeded `[hooks.state]`, not the bypass flag

Hooks outside a managed policy are skipped **silently** under `codex exec` / app-server until trusted (openai/codex#46210). Decision (ADR §E.1, confirmed here): seed per-hook trust hashes.

- `--dangerously-bypass-hook-trust` disables trust for everything in `CODEX_HOME` and project config, and emits two error items per run. Rejected.
- `/etc/codex/requirements.toml` / `allow_managed_hooks_only` needs a root-owned image file and its precedence over the per-agent `config.toml` is unknown (ADR R5). Not v1.
- Seeded trust: editing a hook flips it to `modified` and it stops running (`TestInstallCodexHooks_RealCodex` asserts this against the real binary). Only hooks whose `sourcePath` is our `hooks.json` are trusted; project `.codex/hooks.json` and plugin hooks are not.

The key is `<hooks.json path>:<snake_event>:<group>:<handler>`; Claude's one-handler-per-group layout makes the last two indexes the position within the event. The hash is content-derived, so keys come from `hooks/list` rather than being computed.

## Verified end to end

Mock Responses server + the generated `hooks.json`/`config.toml` + a fake `gb` on `PATH`, `codex exec` with no bypass flag: SessionStart (prime, load-state, check-mail, relay, workspace audit), UserPromptSubmit, PreToolUse and PostToolUse relays and the stop gate all ran with payloads as in the spike; a `{"decision":"block"}` from the gate produced a second model request in the same turn; `2>/dev/null || true` is interpreted by a shell.

## Open

- ADR R4: hooks under **app-server** (not `exec`) with seeded trust. `hooks/list` reports them `trusted` there; whether they *fire* during an app-server turn is for the driver's first integration test (gd-SIXiBKFtTM). If not, the driver runs the gate itself (ADR §8 fallback).
- A real compaction: `SessionStart` with `source=compact` after a model-driven compaction was not exercised (mock model), so the drain-queue replacement above rests on the documented source matcher.
- `SessionEnd` and `PostCompact` exist in Codex but Claude's defaults register nothing for them, so nothing is installed.
