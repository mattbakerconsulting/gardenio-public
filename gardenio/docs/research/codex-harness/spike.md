# Spike: `codex exec` in a gardenio agent pod

Bead gd-JegnDXa3nE (Phase 0). Feeds the ADR step gd-IpXL2HevnY. Date 2026-10-14.
Binary: `codex-cli 0.162.1` (tag `rust-v0.162.1`), x86_64 musl, run **inside a live agent pod** (uid 1000,
no root, `CapEff=0`, `NoNewPrivs=1`, `Seccomp=2`).

## Scope change

The OpenAI-key escalation (gd-hp3ttX98cc) was closed as superseded: Codex will use a ChatGPT-subscription
`auth.json` (credential contract: gd-3gTWiNEhg1), so **Q2 (LiteLLM `/v1/responses`) and Q3 (direct API key)
are obsolete and were not run**. No real model turn succeeded in this spike, so **Q4/Q5 are only partly
answered**. Everything that does not need a model turn was answered for real.

To get past "no model", the spike used a ~40-line Python mock of the Responses API (SSE:
`response.created`, `response.output_item.added/done`, `response.completed{usage}`) as a custom
`model_provider`. Codex itself (event encoder, hooks, rollout writer, tool execution, resume, app-server)
is the real binary; only the model's replies are scripted. This is enough to drive tool calls and the Stop
hook, and to prove resume. It proves nothing about real model behaviour.

## Summary

| # | Question | Result |
|---|---|---|
| 1 | Sandbox in pod | **No bwrap, no userns.** `codex exec --yolo` runs clean with no warning. Default sandbox: every tool call fails with a panic string. |
| 2 | LiteLLM `/v1/responses` | Obsolete (no API key). Custom `http_headers` (`x-litellm-tags`) *do* reach the provider. |
| 3 | Direct API key | Obsolete. A bogus key proves egress to `api.openai.com` + `chatgpt.com` works from the pod, and gives a real `turn.failed` fixture. |
| 4 | `--json` JSONL fixture | Done against mock + real 401; **real successful stream pending** (needs subscription login). |
| 5 | Sessions / resume | **Works, including from the rollout file alone in a fresh `$CODEX_HOME`.** |
| 6 | Hooks | Skipped silently without trust; run with `--dangerously-bypass-hook-trust` **or with pre-seeded `[hooks.state]` trust hashes (better)**. Stop `decision:block` works. |
| 7 | Instructions | `project_doc_fallback_filenames=["CLAUDE.md"]` works; `$CODEX_HOME/AGENTS.md` works as the append-system-prompt stand-in. |
| 8 | `codex app-server` | **Starts in pod, full thread/turn/interrupt/hooks cycle works over stdio.** |

## Setup

```
$ gh api repos/openai/codex/releases/tags/rust-v0.162.1 --jq '.assets[]|select(.name=="codex-x86_64-unknown-linux-musl.tar.gz")|.digest'
sha256:86f268d81b898f3e144c5ecff2ad2fda2c5802e042d6195b72538fe0fa125057
$ curl -sSL -o codex.tgz https://github.com/openai/codex/releases/download/rust-v0.162.1/codex-x86_64-unknown-linux-musl.tar.gz && sha256sum codex.tgz
86f268d8...125057  codex.tgz                                   # matches GitHub's recorded digest
$ tar xzf codex.tgz && sha256sum codex-x86_64-unknown-linux-musl /usr/local/bin/codex
628e3548...d92e1  codex-x86_64-unknown-linux-musl
628e3548...d92e1  /usr/local/bin/codex                         # image's binary is byte-identical
$ codex --version
codex-cli 0.162.1
```

**Sigstore bundle NOT verified**: there is no `cosign` in the pod and `gh attestation verify` returns 404 for
this repo (no SLSA attestation published). The assurance here is the sha256 digest GitHub reports for the
asset plus the image binary being byte-identical. The ADR/image step should decide whether to pin the
digest in the Dockerfile (cheap) and verify the `.sigstore` bundle in CI where `cosign` exists.

`codex` is already on PATH in this pod (`/usr/local/bin/codex`, from the image step gd-HaGrzg53TS).

## Q1: Sandbox

```
$ cat /proc/sys/user/max_user_namespaces ; unshare -Ur id
27319
unshare: unshare failed: Operation not permitted
$ grep -E 'CapEff|NoNewPrivs|Seccomp:' /proc/self/status
CapEff: 0000000000000000   NoNewPrivs: 1   Seccomp: 2
$ which bwrap ; ls /etc/codex
(no output)  /  ls: cannot access '/etc/codex'
$ codex sandbox -- id
thread 'main' panicked at linux-sandbox/src/launcher.rs:60:13:
bubblewrap is unavailable: no system bwrap was found on PATH and no bundled codex-resources/bwrap binary was found next to the Codex executable
```

The kernel allows 27k user namespaces but the container runtime's seccomp filter denies `unshare(CLONE_NEWUSER)`
to the agent. The release ships a separate `bwrap-x86_64-unknown-linux-musl` asset; even with it on PATH the
userns call would be denied, so **bwrap is a dead end in this pod**.

Default sandbox (no `--yolo`), tool call against the mock: the model is handed this as the tool result and
no `command_execution` item is emitted at all (the stream goes straight to the next message):

```
Process exited with code 101
Output: thread 'main' (2151) panicked at linux-sandbox/src/launcher.rs:60:13:
bubblewrap is unavailable: ...
```

`codex exec --yolo ...` (= `sandbox_mode=danger-full-access` + `approval_policy=never`): runs the command
(`command_execution` item, `exit_code 0`) and prints **no sandbox warning**. The request metadata Codex sends
shows `"sandbox":"none","sandbox_mode":"danger-full-access"`. The pod is the sandbox. Use `--yolo`
(or the `thread/start` `sandbox:"danger-full-access"` equivalent) always.

`codex app-server` logs one stderr ERROR when the sandbox is not danger-full-access ("Codex could not find
bubblewrap on PATH ... will use the bundled bubblewrap in the meantime") even though no bundled one exists. Harmless
under danger-full-access; ignore it.

Useful extra: `codex doctor` works offline and gives a readiness probe
(`✗ auth no Codex credentials were found`, `⚠ websocket ...`, `✓ disk`, `✓ search (rg)`, `✓ git`).

## Q2/Q3: LiteLLM and direct (obsolete, partial evidence only)

Not run with a real key. What was learned:

- A custom provider works with a TOML table and `wire_api = "responses"`; set `supports_websockets = false` on a non-OpenAI provider (used throughout the mock runs; the real-OpenAI 401 run
  shows what happens otherwise: 5 websocket reconnect attempts, ~7 s, before the HTTPS fallback).
- `http_headers` values arrive verbatim on every `/v1/responses` request (mock saw
  `x-litellm-tags: agent:x,role:crew,project:gardenio,model:mock`), alongside `originator: codex_exec`,
  `session-id`, `x-codex-turn-metadata` (JSON incl. `installation_id`, `thread_id`, `turn_id`, `sandbox_mode`,
  `model`) and `user-agent: codex_exec/0.162.1`. `x-codex-turn-metadata` has `analytics_enabled:true`; check
  whether the ADR wants to turn that off.
- Egress from the pod works to `api.openai.com` (https and wss) and `chatgpt.com/backend-api/codex/models`.
  With the default provider Codex tries `wss://api.openai.com/v1/responses` first and falls back to HTTPS after
  5 failures, so a subscription pod's first turn on a flaky path can be slow.
- Unknown model names are accepted with a warning item: `Model metadata for <m> not found. Defaulting to
  fallback metadata; this can degrade performance and cause issues.`

## Q4: `--json` event stream

Fixtures under `coop/tests/fixtures/codex/` (see its README for provenance). Event shapes seen:

```
{"type":"thread.started","thread_id":"<uuidv7>"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/bash -lc 'echo hello-from-tool'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution",...,"aggregated_output":"hello-from-tool\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"first done"}}
{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0}}
```

Failure (real api.openai.com, 401): `thread.started`, `item.completed{type:"error", message:"Model metadata ... not found"}`,
`turn.started`, a run of `{"type":"error","message":"Reconnecting... N/5 (...)"}`, an `item.completed{type:"error"}`
("Falling back from WebSockets to HTTPS transport"), more `error`, then `{"type":"turn.failed","error":{"message":...}}`
and process exit code 1.

Driver-relevant points: `error` items are **informational** and appear mid-turn (the turn is only failed by
`turn.failed`); `--dangerously-bypass-hook-trust` emits its warning as two `item.completed{error}` items;
`usage` is per turn and cumulative across the stop-gate continuation (300/60 after two model calls in one
`exec`); no reasoning or `command_execution` stream deltas in `exec --json` (completed items only), so a live
"typing" view needs `app-server` (`item/agentMessage/delta`, `item/commandExecution/outputDelta`).
**Pending**: a successful real-model stream (reasoning items, real usage). Needs the subscription login (gd-3gTWiNEhg1).

## Q5: Sessions and resume

```
$CODEX_HOME/sessions/2026/10/14/rollout-2026-10-14T23-18-47-01a13cb6-121e-7250-9e9f-c6cf6dda9e9d.jsonl
```

- Path is `sessions/YYYY/MM/DD/rollout-<local-ts>-<session-id>.jsonl`; **session id == `thread_id`** from
  `thread.started` (UUIDv7). The path is also in every hook payload as `transcript_path`.
- Rollout lines: `{timestamp, ordinal, type, payload}` with types `session_meta`, `event_msg`
  (`task_started`, `item_completed`, `token_count`, `task_complete`, `thread_settings_applied`),
  `response_item` (message / function_call / function_call_output), `turn_context`, `token_usage_record`,
  `world_state`. `token_usage_record` carries per-response usage (consumer for spend/context-window). See
  `rollout-sample.jsonl` (long prompts trimmed; the real `session_meta` embeds the full base prompt).
- `$CODEX_HOME` also holds several sqlite DBs (`state_5`, `thread_history_1`, `logs_2`, `goals_1`, `queue_1`,
  `memories_1`); none is needed for resume (below).

```
$ codex exec resume 01a13cb6-... --yolo --json "second prompt"        # same CODEX_HOME, after process exit
{"type":"thread.started","thread_id":"01a13cb6-121e-7250-9e9f-c6cf6dda9e9d"} ... turn.completed
$ mkdir -p home4/sessions/2026/10/14; cp <only the rollout .jsonl> home4/sessions/2026/10/14/; cp home3/config.toml home4/
$ CODEX_HOME=home4 codex exec resume 01a13cb6-... --yolo --json "third prompt after restore"
(same thread id, turn.completed)
```

The model request after the restore contained the full history (first prompt, the tool call, the stop-hook
message, "second prompt", "third prompt"), so **the S3 restore path is just: restore the one rollout file
under `$CODEX_HOME/sessions/...`, then `codex exec resume <id>`**. Flags such as `--yolo` must be repeated on
`resume`. Note a resume re-injects the `# AGENTS.md instructions` block as a new user message each time
(visible in the history), so context grows slightly per restart.

## Q6: Hooks

`$CODEX_HOME/hooks.json`, CamelCase event names (`SessionStart`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `Stop`) with `{"hooks":{"<Event>":[{"matcher":"*","hooks":[{"type":"command","command":"..."}]}]}}`
work. (`app-server` reports `eventName` camelCase, e.g. `preToolUse`, and uses snake_case in hook keys, e.g. `pre_tool_use`.)

Trust matrix (tool call + mock; hook scripts append stdin to a log):

| How | SessionStart | UserPrompt | PreToolUse | PostToolUse | Stop |
|---|---|---|---|---|---|
| `exec --yolo` (no trust) | skipped | skipped | skipped | skipped | skipped |
| `exec --yolo --dangerously-bypass-hook-trust` | ran | ran | ran | ran | ran |
| `exec --yolo`, **with `[hooks.state]` seeded** | ran | ran | ran | ran | ran |

Skipped means silent: no warning item, no stderr, `hooks/list` shows `trustStatus:"untrusted"` (enum:
`managed|untrusted|trusted|modified`). This confirms openai/codex#46210.

**Better than the bypass flag**: trust is a per-hook hash in `config.toml`:

```toml
[hooks.state."$CODEX_HOME/hooks.json:pre_tool_use:0:0"]
trusted_hash = "sha256:879fd939..."     # = hooks/list .currentHash
```

The key is `<hooks.json path>:<snake_event>:<group index>:<handler index>`; the hash is content-derived
(identical when the same hook moved between two `$CODEX_HOME` dirs, only the key's path changed). Procedure: write
`hooks.json`, start `codex app-server`, call `hooks/list`, write each `key`/`currentHash` into `config.toml`,
then run with no bypass flag. Editing a hook flips it to `modified` and it stops running, which is the property
we want from a trust model. The init-time cost is one app-server start; the alternative is the bypass flag with
a vetted hooks file. Not tested: `/etc/codex` managed hooks / `allow_managed_hooks_only` (`/etc/codex` is
unwritable by the agent and absent).

Exact stdin payloads (full copies in `hook-payloads.jsonl`). Common: `session_id`, `transcript_path`, `cwd`,
`hook_event_name`, `model`, `permission_mode` (`bypassPermissions` under `--yolo`); turn events add `turn_id`.

```text
SessionStart:     {..., "source":"startup"}
UserPromptSubmit: {..., "prompt":"run the tool"}
PreToolUse:       {..., "tool_name":"Bash", "tool_input":{"command":"echo hello-from-tool"}, "tool_use_id":"call_3ccb6d62"}
PostToolUse:      {..., "tool_name":"Bash", "tool_input":{...}, "tool_response":"hello-from-tool\n", "tool_use_id":"..."}
Stop:             {..., "stop_hook_active":false, "last_assistant_message":"first done"}
```

Note `tool_name` is the Claude-compatible `Bash` (not Codex's `exec_command`) and `tool_input.command` matches
Claude's shape, so `gb hook` handlers keyed on tool name can be shared.

**Stop gate**: a Stop hook that printed `{"decision":"block","reason":"GATE: you have unfinished beads, run gb stop"}`
(exit 0) made Codex **continue the turn**: the next model request ends with a user message
`<hook_prompt hook_run_id="stop:4:$CODEX_HOME/hooks.json">GATE: ...</hook_prompt>`, and the second Stop fires
with `"stop_hook_active": true` and the new `last_assistant_message`. Same loop-guard semantics as Claude. A hook
that prints nothing and exits 0 lets the turn end. (Exit code 2 was not tested; the task assumes JSON is required.)

PreToolUse deny and `additionalContext` return shapes were not exercised.

## Q7: Instructions

`config.toml`: `project_doc_fallback_filenames = ["CLAUDE.md"]` (must be a top-level key, before any `[table]`)
and `$CODEX_HOME/AGENTS.md`. The first model request carried one user message:

```
# AGENTS.md instructions for /workspace

<INSTRUCTIONS>
GLOBAL-AGENTS-MARKER: gb prime output stand-in

--- project-doc ---

WORKSPACE-CLAUDE-MD-MARKER

</INSTRUCTIONS>
```

So `$CODEX_HOME/AGENTS.md` is injected first, then the workspace's `CLAUDE.md` (found in the git root via the
fallback list), as a **user-role** message (not system). That is a fine stand-in for
`--append-system-prompt-file`, with the caveat that it is user-role context, and gets re-sent as a new message on
each resume. Untested: nested-directory `CLAUDE.md` merging, `project_doc_max_bytes` truncation,
`AGENTS.md` winning over `CLAUDE.md` in the same dir.

## Q8: `codex app-server`

Starts in the pod and works over stdio JSON-RPC (newline-delimited, `"jsonrpc":"2.0"` field present).
`codex app-server generate-json-schema --out DIR` emits the full protocol (used here for method names).
Transcript: `app-server-session.jsonl`.

```
> initialize {clientInfo:{name,version}}          -> {userAgent, codexHome, platformFamily, platformOs,...}
> initialized (notification)
> thread/start {cwd, approvalPolicy:"never", sandbox:"danger-full-access"}   -> {thread:{id,...}}
> turn/start {threadId, input:[{type:"text",text:"..."}]}                    -> {turn:{id,status:"inProgress"}}
< thread/status/changed, turn/started, item/started, item/completed (userMessage, agentMessage),
  thread/tokenUsage/updated, account/rateLimits/updated, thread/status/changed, turn/completed{turn.status}
> turn/interrupt {threadId, turnId}               -> {}      (mock model was mid-response)
< thread/status/changed, turn/completed{status:"interrupted", durationMs:3021}   # within ~0 s
> turn/start (nudge after interrupt)               -> normal turn on the SAME thread
> hooks/list {cwds:[...]}                          -> hooks with eventName, source, currentHash, trustStatus
```

Everything the driver needs is first-class: `turn/interrupt` (prompt, no process restart, thread survives),
`turn/steer` (inject into a running turn), `thread/resume|fork|list|read|compact/start|inject_items`,
`thread/tokenUsage/updated` and `account/rateLimits/updated` notifications (usage for free),
`account/usage/read`, and `hooks/list`. Server-to-client requests the driver must answer:
`item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`,
`item/tool/requestUserInput`, `item/tool/call`, `mcpServer/elicitation/request`, and
**`account/chatgptAuthTokens/refresh`**: the app-server asks its client for fresh ChatGPT tokens, which is the natural hook
for a credential broker (relevant to gd-3gTWiNEhg1's rotating refresh-token problem; not exercised here, needs a
real login). `account/login/start`, `account/read`, `account/logout` also exist.

Recommendation to the ADR: **driver shape (3), `codex app-server`**, with `exec --json` + `resume` as the
fallback. Hooks in app-server mode: `hooks/list` loads the same `hooks.json` and reports `trustStatus:"untrusted"`, and no
`hook/started` notification was emitted during the turns (so untrusted hooks are skipped there too). Whether seeded
`[hooks.state]` makes them run under app-server was not tested.

Untested: running a real turn through a hook-trust-seeded app-server, approvals round-trip (everything was
`never`), `--listen unix://` / `ws://` transports, daemon mode (`codex app-server daemon`).

## Pending (needs the ChatGPT-subscription login, gd-3gTWiNEhg1)

Update: the login escalation gd-6MspYn8R0l executed (Secret `gardenio-codex-auth`, key `auth.json`, namespace
`gardenio`), but nothing mounts that Secret into agent pods yet (the keeper and pod wiring are Phase 1 work), and
an agent pod has no k8s token to read it. A probe in a crew pod found no `auth.json` anywhere on the filesystem,
no `~/.codex`, and no `/var/run/secrets`. So the items below stay pending until a pod (or an operator-run
context) has the Secret mounted at `$CODEX_HOME/auth.json`; they should be re-run as the first task of Phase 1
pod wiring.

- Q4: a real successful `exec --json` stream (reasoning items, real `usage`), and `app-server` reasoning/delta
  events with a real model; replace or add to the mock-backed fixtures.
- Q5: confirm resume against a real server with `store`/`previous_response_id` behaviour for subscription auth
  (the mock ignores server-side state; Codex sent the full history, so the file-only restore path is expected to hold).
- `account/chatgptAuthTokens/refresh` flow and concurrent-pod refresh-token rotation behaviour.
- Q2/Q3 were dropped (no API key).
- Verify the `.sigstore` bundle where `cosign` is available.
- `/etc/codex/{managed_config,requirements}.toml` precedence and `allow_managed_hooks_only` (needs root or an image change).
