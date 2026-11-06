# Gemini harness phase 0 spike

> Bead: gd-llfzgEJZNN (plan gd-8s8FdZu8UA, scope-out gd-lWkvvnWMBE). Closes the phase 0 gate of [ADR 0005](../../adr/0005-gemini-harness.md) §7.
> Pinned binary: `@google/gemini-cli` **0.64.0**, GitHub release asset `gemini-cli-bundle.zip`, sha256 `9d4ad33d15dcf35098919c69eb9d98b6dc640567f141d6fc4f8f053c8cbd8dae`.
> Fixtures: `coop/tests/fixtures/gemini/`. Reproduce: `docs/research/gemini-harness/regen.sh` (no API key needed).

## Read this first: what this spike did and did not measure

The real pinned binary was run in an agent pod (Node 24, uid 1000, no sandbox, no root). **No Gemini API key was available** to this crew: the pod has no Google credential and no `provider=gemini` account exists to borrow. So the CLI was pointed at a local scripted stand-in for the Gemini API (`GOOGLE_GEMINI_BASE_URL`, `tools/mock-api/mock_gemini_api.mjs`).

- **Measured for real (the CLI's own behaviour):** flags, exit codes, `json`/`stream-json` event shapes, hook events and payloads, the hook output contract, settings loading and precedence, `GEMINI.md` placement, `mcpServers`, the on-disk session record and resume, ACP frames, PTY screens, first-run gates, outbound hosts.
- **Not measured (needs a live key):** which model ids a real API-key account can call, what the real API does with an unknown id, real usage numbers, real 429/quota error bodies, and whether the real model obeys the tool declarations. Every fixture whose content depends on those says so below, and they are collected in "Carried to a live pass". The `usageMetadata` the mock returns is invented; the *places the CLI puts it* are real.

Status words: **Closed** = answered with evidence from the pinned binary. **Closed (CLI side)** = the CLI half is answered, a live-key half is carried.

## Result in one table

| Risk | Status | Answer |
|---|---|---|
| R1 structured interface | Closed | `gemini --acp` (ACP, JSON-RPC over stdio) is a long-lived structured protocol with cancel, per-turn usage, resume and permission requests. Headless `-p -o stream-json` is a viable per-turn fallback. Recommend ACP (ADR §4 rule 1) with caveats below. |
| R2 hooks | Closed | Hooks fire in every mode, but **ACP fires only `BeforeAgent`/`AfterAgent`/`PreCompress`/`SessionEnd`**. The stop gate works via `AfterAgent`. **The current coop shim `{"continue":true}` does not continue a turn.** The working output is `{"decision":"block"\|"deny","reason":...}` or exit 2. |
| R3 settings, GEMINI.md, mcp | Closed | **`GEMINI_CLI_SYSTEM_SETTINGS_PATH` is ignored unless its parent directory is root-owned** (so coop's injection is a silent no-op in an agent pod). Inject through `~/.gemini/settings.json` + `~/.gemini/GEMINI.md` instead. |
| R4 approvals | Closed | Headless default mode does not even register the shell/write tools; `--approval-mode yolo` is required. ACP asks via `session/request_permission`. |
| R5 session record | Closed | `~/.gemini/tmp/<slug>/chats/session-*.jsonl` alone is enough to resume in a fresh HOME; carries per-message tokens and tool calls. `--session-id <uuid>` lets coop pick the id. |
| R6 models | Closed (CLI side) | Default ids and context window read from the binary; availability and unknown-id behaviour **need a live key**. |
| R7 first-run / update / telemetry | Closed | Three gates found (auth type, workspace trust, usage statistics); the keys that clear them are listed. |
| R8 quota errors | Closed (CLI side) | 429 -> 10 retries over ~3 min -> `result.status=error`, exit 173. Real error body needs a live key (Phase 3 already owns it). |
| R9 provenance | Closed | No checksum file and no attestation upstream; pin = GitHub asset digest, cross-checked to the npm tarball. Residual risk accepted and recorded. |

## R1. Which structured interface exists

Candidate modes run against 0.64.0 (flags from `gemini --help`):

| Mode | Command | Verdict |
|---|---|---|
| ACP | `gemini --acp` (older `--experimental-acp` is deprecated) | **Best.** Long-lived. See below. Fixtures `acp/*.jsonl`. |
| Headless JSON | `gemini -p <prompt> -o json` | One object `{session_id, response, stats}` at exit. `stats.models.<m>.tokens` and `stats.models.<m>.roles.{main,utility_router}` give usage. `headless/json-text.json`. |
| Headless stream | `gemini -p <prompt> -o stream-json` | JSONL: `init{session_id,model}`, `message{role,content,delta}`, `tool_use{tool_name,tool_id,parameters}`, `tool_result{tool_id,status,output,error?}`, `error{severity,message}`, `result{status,stats}`. `headless/stream-json-*.jsonl`. Exit 0 on success. |
| PTY TUI | `gemini` on a pty | Works (with the R7 gates cleared); renders `> ` input, a `Allow execution of [Shell]?` selection, footer with model and sandbox. `pty/turn-with-permission.{txt,raw}`. |

**ACP** (`protocolVersion` 1, `agentInfo` gemini-cli 0.64.0). Verified frames, all in `acp/`:

- `initialize` -> `authMethods` (`gemini-api-key`, `oauth-personal`, `vertex-ai`, `gateway`) and `agentCapabilities {loadSession:true, mcpCapabilities {http,sse}, promptCapabilities {image,audio,embeddedContext}}`. `authenticate {methodId:"gemini-api-key"}` returns `{}` and uses `GEMINI_API_KEY` from the environment.
- `session/new {cwd, mcpServers}` -> `sessionId`, `modes` (`default`, `autoEdit`, `yolo`, `plan`), `models` (ids in R6).
- `session/prompt` streams `session/update` notifications: `agent_message_chunk`, `tool_call` / `tool_call_update` (status, title, kind, content), `usage_update {used, size}`; the response carries `stopReason` plus `usage {inputTokens,outputTokens,totalTokens}` and `_meta.quota.model_usage[]` (per model). That is the per-turn usage ADR §4 asks for, without scraping.
- **Interrupt:** `session/cancel` (notification) ends the in-flight prompt with `stopReason:"cancelled"` (`acp/cancel.jsonl`; latency not measured, the stalled stream returned as soon as the cancel was read).
- **Steer / queue:** a second `session/prompt` sent while one is running **cancels the first** (`stopReason:"cancelled"`) and runs the second. There is no queue. coop must serialise its own input queue (`acp/queue.jsonl`).
- **Resume:** `session/load {sessionId, cwd, mcpServers}` in a new process replays history (verified, same HOME).
- Process noise on stderr (`[STARTUP] Phase 'cli_startup' was started but never ended`, `Could not find promptId in context for classifier-router`) is harmless but must not be parsed as errors.

Against ADR §4's selection rule: rule 1 asks for interrupt, steer-or-queue, per-turn usage and seeded hooks. ACP gives interrupt (yes), steer (replace-on-new-prompt, not queue), usage (yes) and hooks (partial, R2). **Decision for Phase 3: ACP is the primary driver mode; headless `stream-json` per turn is the fallback; the PTY driver stays until ACP is proven in a pod with a real key.** The loss versus the PTY is the live TUI view.

Two operational facts to carry into Phase 3:

1. The CLI relaunches itself as a child (`node --max-old-space-size=3908 ...gemini.js`, visible in `ps`). Killing only the launcher orphaned the worker (ppid 1) in this spike. coop must kill the **process group**. `GEMINI_CLI_NO_RELAUNCH=true` exists in the bundle; whether it avoids the child (and the heap flag) was not shown, and signal delivery in headless mode against a stalled stream was inconclusive (SIGINT was confounded by the shell ignoring it for background jobs; SIGTERM did not end the run within 6 s). Use ACP `cancel` for turn interruption.
2. The heap flag is derived from host memory, not the 2 GiB pod limit (see the OOM advice gd-QLhMjEsBsR). Phase 3 should pin `NODE_OPTIONS=--max-old-space-size=...` and measure RSS in a pod.

## R2. Hooks

Config shape (`settings/user-settings-example.json` shows a minimal user file): `hooks.<Event>: [{matcher?, hooks:[{type:"command", command, timeout?}]}]`. coop's existing `"matcher": "*"` is accepted (all events fired with it).

**Events and payloads** (`hooks/*.json`). Common fields on every payload: `session_id`, `transcript_path` (the session JSONL from R5), `cwd`, `hook_event_name`, `timestamp`. The hook process also sees `GEMINI_SESSION_ID`, `GEMINI_PROJECT_DIR` and, for compatibility, `CLAUDE_PROJECT_DIR`.

| Event | Extra fields | Headless `-p` | PTY | ACP |
|---|---|---|---|---|
| `SessionStart` | `source` (`startup`) | yes | yes | **no** |
| `BeforeAgent` | `prompt` | yes | yes | yes |
| `BeforeTool` | `tool_name`, `tool_input` | yes | yes | **no** |
| `AfterTool` | `tool_name`, `tool_input`, `tool_response{llmContent,returnDisplay}` | yes | yes | **no** |
| `AfterAgent` | `prompt`, `prompt_response`, `stop_hook_active` | yes | yes | yes |
| `SessionEnd` | `reason` | yes | yes | yes |
| `Notification` | `notification_type:"ToolPermission"`, `message`, `details{type,title,command,rootCommand}` | n/a (no prompts) | yes | **no** (the protocol carries it) |
| `PreCompress` | `trigger` | yes | yes | yes |
| `BeforeModel` / `AfterModel` / `BeforeToolSelection` | `llm_request` / `llm_response` | yes (fires per model call) | not checked | not checked |

Consequence: in ACP mode tool visibility must come from `tool_call` updates, not hooks. The relay's canonical tool events would be synthesised from ACP frames.

**Discriminator for `relayHarness` payload sniffing:** `hook_event_name` values `BeforeAgent`, `AfterAgent`, `BeforeTool`, `AfterTool` do not exist in Claude Code (`PreToolUse`, `PostToolUse`, `Stop`), so the event name alone is a stable discriminator. `GARDENIO_HARNESS` stays the primary source.

**The stop-gate contract (the load-bearing finding).** `AfterAgent` output and its effect, measured by whether a second model call happened (`headless/stopgate-*.jsonl` and `.meta`):

| Hook output | Result |
|---|---|
| `{"decision":"block","reason":"R"}` or `{"decision":"deny","reason":"R"}` | **Turn continues**: `R` is sent to the model as a new user prompt; stream shows `error severity=warning "Agent execution blocked: R"`; the hook fires again with `stop_hook_active:true`. |
| exit code 2, `R` on stderr | Same as above. |
| `{"continue":true}` | **No effect.** One model call, the turn ends. |
| `{"continue":false,"stopReason":...}` | Stops, no retry. |

coop's current shim (`coop/crates/cli/src/driver/gemini/hooks.rs:47-64`) prints `{"continue":true}` to block a stop. On 0.64.0 that does not hold a turn open, so **today's Gemini stop gate fails open**. The same contract held in ACP (`acp` run with `AfterAgent` block: two `agent_message_chunk`s, one prompt). `gb hook stop-gate --format gemini` must emit `{"decision":"block","reason":...}` (exit 0), matching ADR §6, and the Phase 2 real-binary test must assert a second model call, not just the output string.

Not tested: whether hooks defined in project (`.gemini/settings.json`) settings need workspace trust to run (the spike trusted the workspace via env). Phase 2 injects at user level, so it is not on the critical path.

## R3. Settings, `GEMINI.md`, `mcpServers`

- **`GEMINI_CLI_SYSTEM_SETTINGS_PATH` is silently ignored in an agent pod.** With the file in a directory owned by uid 1000 the CLI prints `Security Warning: Skipping system settings file ... Parent directory ... is insecure: Directory ... is not owned by root (uid 0)` (`settings/system-settings-insecure-dir.txt`) and loads nothing: neither its hooks nor its `model.name` took effect. The bundle's check (`checkPosixStatsSecurity`) requires uid 0 and no group/other write bit. coop's `prepare_fresh`/`prepare_pristine` write the file into `coop_session_dir`, which the agent owns, so **the existing driver's hook and MCP injection never reaches the CLI as uid 1000**. The positive case (a root-owned directory) was not run: the pod cannot chown. Do not rely on it; it also could not be rewritten per agent at runtime.
- **Inject at user level instead:** `~/.gemini/settings.json` (or `$GEMINI_CLI_HOME/.gemini/`). Verified: hooks, `mcpServers`, `security.auth.selectedType`, `general.*`, `privacy.*` all load from it.
- **Precedence observed:** workspace `.gemini/settings.json` > user for scalars (`model.name` took the workspace value); hook arrays from user and workspace **both run** (merged, not replaced).
- **`GEMINI.md`:** user-level `~/.gemini/GEMINI.md` is injected into the request's **`systemInstruction`** inside `<loaded_context>`; a workspace `GEMINI.md` goes into the first user turn under `<project_context>`. For `gb prime` injection the user-level file is the right slot (system-prompt strength, equivalent to `--append-system-prompt-file`). Size limits were not probed.
- **`mcpServers`:** `{ "<name>": {command, args, trust} }` in settings; `gemini mcp list` reports `Connected`; tools are exposed as `mcp_<server>_<tool>` (`settings/mcp-list.txt`, `settings/main-request-tools.json`). A `.mcp.json` file was not tested, so the Claude-style `.mcp.json` translation remains a gardenio job (ADR §5 E stands).
- **Default model routing adds a request.** With `model` unset the CLI sends an extra `utility_router` classifier call (`gemini-3.5-flash-lite`) before every main call. Pin `model.name` / `-m` to avoid the extra call and the `[Routing] ... failed` noise that appears if the classifier answer is malformed.

## R4. Approvals in a pod with no sandbox

- **Headless, default mode:** the declared tool list is read-only (`read_file`, `list_directory`, `glob`, `grep_search`, `google_web_search`, `update_topic`, `enter_plan_mode`, `invoke_agent`); `run_shell_command` and `write_file` are absent. A call to one is answered with `tool_result status=error error.type=tool_not_registered` (`headless/stream-json-tool-default-mode.jsonl`). An agent that must run commands needs `--approval-mode yolo` (`stream-json-tool-yolo.jsonl`: tool runs, banner `YOLO mode is enabled` on stderr, no prompt).
- **ACP:** a read-only-looking command (`echo hi`) ran without a prompt; a mutating one (`touch x`) sent `session/request_permission` with options `proceed_always` / `proceed_once` / `cancel` (`acp/perm.jsonl`). `setSessionMode` can switch to `yolo`. ADR §8's "approval policy is never-ask; the driver answers" fits ACP directly: answer `proceed_once` (or `cancel` to deny and log).
- **PTY:** `Allow execution of [Shell]?` with `1. Allow once` preselected, plus a `Notification` hook with the command (`pty/`, `hooks/pty-Notification.json`).
- No sandbox was used (`-s` off; the CLI reports `no sandbox`), running as an unprivileged user: shell, file writes and MCP all worked. No prompt appeared in yolo mode.

## R5. Session record and resume

`~/.gemini/tmp/<slug>/chats/session-<ts>-<id8>.jsonl`, where `<slug>` is the workspace directory's basename (`~/.gemini/projects.json` maps path -> slug). Format (`session/session-record-tool.jsonl`):

- Line 1 header: `{sessionId, projectHash, startTime, lastUpdated, kind:"main"}`.
- Then `{id, timestamp, type:"user"|"gemini", content, ...}` records. `gemini` records carry `tokens {input,output,cached,thoughts,tool,total}`, `model`, `thoughts`, and `toolCalls[]` once a call completes.
- `{"$set":{"lastUpdated":...}}` patch lines. **The same record `id` is appended again with more fields** (the tool-call record appears twice); a reader must dedupe by `id`, last wins.
- A hook-injected retry (`AfterAgent` block) is stored as a `user` record with the hook text in `content` and the original prompt in `displayContent`.

**Resume from the file alone:** copying only that one file into an otherwise empty HOME and running `gemini --resume <sessionId> -p ...` restores the full history including function call/response turns (`session/resume-fresh-home.{jsonl,history.json}`); no `projects.json` is needed. `--resume latest|<index>`, `--list-sessions` and `--session-id <uuid>` (caller-chosen id, verified) exist; ACP has `session/load`. So coop can upload this file with the existing snapshot scheme and restore it at the same `<slug>` path, and can choose the session id up front. Usage is available per message in this file and per turn in ACP/`stream-json`, so no Go parser is needed beyond coop's normalised events (ADR §6 holds).

## R6. Models

From the pinned binary (ACP `session/new` and request URLs, `acp/basic.jsonl`):

- `auto` (default; routes between `gemini-3.1-pro-preview` and `gemini-3.8-flash`), `gemini-3.1-pro-preview-customtools`, `gemini-3-flash-preview`, `gemini-2.5-pro`, `gemini-3.8-flash`, `gemini-3.5-flash-lite` (router/utility).
- Context window reported by the CLI: `usage_update.size = 1048576`.
- The init event reports `"model":"auto"` unless `-m` / `model.name` is set. `-m` does **no local validation**: `-m totally-bogus-model` produced a request to `.../models/totally-bogus-model:streamGenerateContent` and an init event with that name.

Not answered without a live key: which of these a given API-key account may call, the real API's reply to an unknown id (hard 404 vs silent fallback, the Codex lesson), and real window sizes per model. ADR §5 D stands: no short Slack aliases until a live pass reads real behaviour.

## R7. First-run, update and telemetry gates

Three gates stop an unconfigured headless pod; the keys that clear them:

| Symptom | Exit | Fix |
|---|---|---|
| `Please set an Auth method ...` / `Invalid auth method selected.` even with `GEMINI_API_KEY` set | 41 | `security.auth.selectedType: "gemini-api-key"` in user settings (`headless/exit-no-auth.txt`) |
| `Gemini CLI is not running in a trusted directory ...` | 55 | `GEMINI_CLI_TRUST_WORKSPACE=true` (or `--skip-trust`) (`headless/exit-untrusted.txt`) |
| (silent) usage statistics sent | 0 | default config contacted `play.googleapis.com:443` (observed with a logging CONNECT proxy). `privacy.usageStatisticsEnabled: false` + `general.enableAutoUpdate: false` + `general.enableAutoUpdateNotification: false` produced no outbound connection other than the API base URL. |

No update-check request was observed headless in either configuration, so auto-update is unproven rather than shown harmless; disable it anyway. `GEMINI_CLI_HOME` relocates `~/.gemini`. On API errors the CLI writes `/tmp/gemini-client-error-*.json` report files; set a writable `TMPDIR` and expect them. The pod `startup` cost is ~3.7 s per `gemini -p` (`node` boot plus bundle load), which argues for ACP over per-turn headless.

All of these belong in the `gb setup gemini` writer and the pod env (ADR §5 E), not in the image: `gemini.sh` records the list in a comment.

## R8. Quota / rate-limit errors

CLI side (mock returned HTTP 429 `RESOURCE_EXHAUSTED` in the documented Google error envelope): the CLI retries **10 attempts** with backoff (~3 minutes in total), printing `Attempt N failed with status 429. Retrying with backoff...`, then emits `result {status:"error", error:{type:"unknown", message:"[API Error: Resource has been exhausted (e.g. check quota).]\nPlease wait and try again later. ..."}}` and exits **173** (both `json` and `stream-json`). Note the retry window: a quota-exhausted agent looks hung for ~3 minutes before any error event, and `error.type` is `unknown`, so `error_category.rs` must classify on the message text/exit code. The committed run script can capture the full stream with `WITH_429=1` (omitted from the default fixtures because it takes ~3 min). The **real** 429 body (quota metric details, `retryDelay`) needs a live key; ADR already assigns R8 to Phase 3.

## R9. Binary provenance

- Upstream publishes the CLI as an npm package and as GitHub release assets: a platform-independent `gemini-cli-bundle.zip` (21 MB, Node >=20) plus unsigned macOS binaries. **No Linux binary, no `SHA256SUMS`, no sigstore bundle.**
- `gh attestation verify` for the asset: HTTP 404 (no SLSA provenance). npm shows registry ECDSA signatures only, no package attestations.
- The pin is therefore the GitHub asset `digest` field (`9d4ad33d...`), re-hashed locally from a fresh download (matched), and cross-checked: the zip's contents equal the npm tarball's `bundle/` file-for-file except five `examples/*/.gitignore` files. Two independent distribution channels agreeing is the strongest check available; it is not a signature check. `images/agent/install/gemini.sh` fails closed on digest mismatch and asserts the reported version; a bump must change `GEMINI_VERSION` and `GEMINI_SHA256` together (`gemini_test.sh` covers both failure modes). **Residual risk, accepted:** trust-on-first-use of the release asset; revisit if upstream ships attestations.
- The image impact: +96 MB unpacked, Node required (so it runs after `node.sh`), extracted with `python3 -m zipfile` because `unzip` is not in the `base` stage.

## Corrections to ADR 0005 text

The ADR's recollected shapes that the spike overturned or refined (applied in the ADR amendment):

1. §6 "the current shim's `{"continue":true}` is a recollection to test": tested, it does not continue a turn; the contract is `decision: block|deny` or exit 2.
2. §5 E "if it merges unpredictably the injection moves to the user settings file": it is worse than that, the system file is skipped in non-root pods. Injection is user-level from the start.
3. §4: the structured candidate that wins is ACP, not headless JSON.
4. §9 R2 "whether hooks fire in headless/protocol modes": yes in headless and PTY; ACP fires a subset.

## Carried to a live pass (needs a Gemini API key account)

Filed as a follow-up bead with an escalation for the key (see the bead trail on gd-llfzgEJZNN). It only needs: a `GEMINI_API_KEY` for an AI Studio / Gemini Developer API project in a pod, then run `regen.sh` with `GOOGLE_GEMINI_BASE_URL` unset and `WITH_429` off, and record:

1. R6: which of the six ids above a key can call; the reply to an unknown id; whether `auto` works with a key that lacks the preview models.
2. R8: a real quota error body (a free-tier key at its limit is enough) and how the CLI surfaces it.
3. Real `usageMetadata` -> `tokens` mapping, including `thoughts` and `cached`.
4. Whether the real model emits tool calls under `--approval-mode yolo` the way the mock does (the CLI side is proven).
5. Pod RSS and startup of `gemini --acp` under the 2 GiB limit (R1 caveat 2).

None of these changes the driver-mode decision; they size Phase 3.

## Files

- `regen.sh`: reproduces all fixtures (`WITH_429=1` adds the retry run).
- `tools/mock-api/mock_gemini_api.mjs`: scripted Gemini API (`MOCK:TOOL <cmd>`, `MOCK:429`, `MOCK:SLOW`).
- `tools/hook.sh`, `tools/acp_client.py`, `tools/pty_run.py`, `tools/mini_mcp.mjs`: probes used by the script.
- `coop/tests/fixtures/gemini/`: `headless/`, `hooks/`, `session/`, `acp/`, `pty/`, `settings/`. Fixture content produced with the mock is mock content: the shapes are real, the model text, token counts and the model id string `mock-model`/`gemini-3.8-flash` are not evidence about the live API.
