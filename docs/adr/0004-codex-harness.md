# ADR 0004 — Codex harness: how gardenio runs agents on the OpenAI Codex CLI

> Status: **Accepted** (implemented through Phase 4 and dogfooded end to end: [`gardenio/docs/harness-codex.md`](../../gardenio/docs/harness-codex.md); see §11 for what the operator may still overturn)
> Bead: gd-IpXL2HevnY (Phase 0 ADR)
> Plan: gd-8s8FdZu8UA "Bring Codex to gardenio"; epic gd-vvcIRCtyLl "0. Spike and design"
> Evidence:
> - Spike results, `codex-cli 0.162.1` run inside a live agent pod: [`gardenio/docs/research/codex-harness/spike.md`](../../gardenio/docs/research/codex-harness/spike.md) (gd-JegnDXa3nE, PR #388). Fixtures: `coop/tests/fixtures/codex/`.
> - Credential contract (ChatGPT-subscription `auth.json`, controller-side refresh keeper): the DECISION comment on gd-3gTWiNEhg1. It is normative for §5 and §6; this ADR summarises it and does not re-argue it.
> - Image step: gd-HaGrzg53TS (PR #361), pinned binary at `gardenio/images/agent/install/codex.sh`.
> Every later phase (gd-5Xmlxm5wiJ, gd-k3NLQhUim0, gd-tAdJlqrQsq, gd-J9Wkw5HjF6) cites this ADR by section.

Evidence tags used below: **[SPIKE]** observed in the spike (mock Responses server or real 401, real `codex` binary). **[BIN]** read from the pinned binary's strings or embedded schema, never executed. **[REPO]** read in this repo. **[UNVERIFIED]** inference or memory; each one has a row in §10 naming the step that closes it.

Placement note: the task text said `gardenio/docs/adr/`, but ADRs 0001-0003 live in `docs/adr/` at the repo root and the numbering continues there, so this is 0004 in that directory.

---

## 1. Context

Gardenio runs every agent as Claude Code inside a pod. `coop` (Rust, PTY multiplexer) wraps the process: its Claude driver scrapes the screen, tails the JSONL session log, and receives Claude hook events, then derives agent state (working / idle / waiting / error) and delivers nudges. `gb` (Go) sets up the pod (`gb setup`, `gb hook ...`), the controller builds the pod spec, and `account` beads select credentials.

The goal is to let a role run on OpenAI's Codex CLI instead, first for one dogfood role (`gardenio-codex`), without forking the rest of the platform: beads, Slack cards, decisions, escalations, spend and death-report tooling should keep working.

The spike answered the questions that did not need a successful real model turn. The headline findings that shape this ADR:

- No `bwrap`, no user namespaces in the pod: Codex's own sandbox cannot run; `--yolo` (`danger-full-access` + `approval_policy=never`) runs clean and the pod is the sandbox. [SPIKE Q1]
- `codex app-server` starts in the pod and provides a full thread / turn / interrupt / steer / hooks cycle over stdio JSON-RPC. [SPIKE Q8]
- `codex exec --json` plus `exec resume <id>` works, including from a lone restored rollout file. [SPIKE Q5]
- Hooks are **silently skipped** unless trusted; per-hook `[hooks.state]` hashes in `config.toml` make them run without the bypass flag. [SPIKE Q6]
- A Stop hook that prints `{"decision":"block","reason":...}` and exits 0 continues the turn, with `stop_hook_active` loop-guard semantics identical to Claude's. [SPIKE Q6]
- `$CODEX_HOME/AGENTS.md` plus `project_doc_fallback_filenames=["CLAUDE.md"]` injects both, as a user-role message. [SPIKE Q7]

Not established: any real-model turn (Q4/Q5 only on a mock), hook behaviour under app-server, the refresh-token flow, `/etc/codex` managed policy. These are carried as open risks in §10, not assumed away.

## 2. Decisions at a glance

| § | Decision |
|---|---|
| A | coop drives Codex through **`codex app-server`** (JSON-RPC over stdio), not a PTY. `codex exec --json` + `resume` is the documented fallback and the first slice if app-server fails its Phase 3 gate. |
| B | A `harness` field (`claude` \| `codex`) on role / project / agent beads, resolved agent > role > project > global default `claude`. Account `provider` must agree with `harness`; the controller refuses a mismatch at spawn. |
| C | Credential is a **ChatGPT-subscription `auth.json`** with a controller-side single-writer refresh keeper (gd-3gTWiNEhg1). No API key, no LiteLLM in v1. |
| D | No LiteLLM model groups. Model names are Codex's own; `--model` maps to the app-server `model` parameter, validated against a harness-scoped allowlist. |
| E | Context injection: `$CODEX_HOME/AGENTS.md`, `project_doc_fallback_filenames`, `config.toml` `mcp_servers`, `hooks.json` + pre-seeded `[hooks.state]`. Nothing baked into `/etc/codex`. |
| F | `gb hook stop-gate` grows a Codex output mode: JSON `decision:block` on **stdout**, exit **0**. |
| G | Persistence: the rollout file is the unit of persistence and S3 upload. coop normalises events; Go consumers read coop's normalised events, not Codex rollouts. |
| H | Explicit non-goals for v1. |

## 3. A. How coop drives Codex

**Decision: option (3), `codex app-server`.** coop spawns `codex app-server` as a child with piped stdio (no PTY), speaks newline-delimited JSON-RPC, and holds one thread per agent session. `codex exec --json` per turn (option 2) is kept as the fallback.

Options considered:

1. **TUI in a PTY with screen scraping.** Rejected. The Claude driver's screen classifier is the most fragile seam in coop today, and Codex gives us nothing to replace it with; its TUI is not the interface gardenio wants. It would also re-create the problem for a second, faster-moving UI.
2. **`codex exec --json` per turn; nudges become `codex exec resume <id> "<text>"`.** Works [SPIKE Q4/Q5], and is the simplest. Rejected as the primary because: each turn is a fresh process with a fresh hook-trust and config load; there is no interrupt short of killing the process; `exec --json` emits completed items only, with no reasoning or output deltas, so there is no live view [SPIKE Q4]; and a resume re-injects the `AGENTS.md` block as a new user message every time, so context grows with every turn rather than every restart [SPIKE Q5].
3. **`codex app-server` JSON-RPC client.** Chosen. [SPIKE Q8] showed, against the real binary in the pod:
   - `initialize` / `initialized`, `thread/start {cwd, approvalPolicy:"never", sandbox:"danger-full-access"}`, `turn/start {threadId, input:[{type:"text",text}]}`.
   - `turn/interrupt` returns promptly, the turn ends `interrupted`, and the same thread then accepts a new `turn/start`. This replaces "send Escape to the PTY".
   - `turn/steer` injects into a running turn; `thread/resume|fork|compact/start|inject_items` exist.
   - Streaming notifications (`item/agentMessage/delta`, `item/commandExecution/outputDelta`), `thread/tokenUsage/updated` and `account/rateLimits/updated` give live text, per-turn usage and limit signals as first-class messages.
   - Server-to-client requests the driver must answer: `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/permissions/requestApproval`, `item/tool/requestUserInput`, `item/tool/call`, `mcpServer/elicitation/request`, and `account/chatgptAuthTokens/refresh` (the host-owns-the-refresh hook used by §5).

**Driver behaviour (contract for gd-SIXiBKFtTM):**

- State is derived from protocol events, not the screen: `thread/status/changed` and `turn/started|completed` drive working/idle; `turn/completed{status:"failed"}` and error notifications drive error classification; `account/rateLimits/updated` and usage-limit errors raise the same rate-limit signal the Claude driver raises, so profile rotation and `auth_failover.go` work unchanged.
- Nudges: `turn/steer` if a turn is in flight, else `turn/start`. Interrupt is `turn/interrupt`. There is no Claude-style channels MCP server (§7).
- Approvals: under `danger-full-access` + `never` none are expected; the driver answers any that arrive by policy (deny and log by default) so a turn can never hang on an unanswered request. [UNVERIFIED: no approval request was ever exercised, everything ran `never`.]
- `error` items inside a turn are informational; only `turn.failed` / `turn/completed{failed}` fails a turn. [SPIKE Q4]
- Sandbox: always `danger-full-access` / `approval_policy=never`. The pod is the sandbox [SPIKE Q1]. The one stderr ERROR about bubblewrap that app-server logs is ignored. Do not try to ship `bwrap`: the seccomp filter denies `unshare(CLONE_NEWUSER)`, so it is a dead end [SPIKE Q1].
- `codex doctor` works offline and is the pod readiness probe (credentials, disk, `rg`, `git`). [SPIKE Q1]
- coop already has `AgentType::Codex` as a stub that bails with "driver is not yet implemented" (`coop/crates/cli/src/run.rs`); the driver replaces that arm.

**Fallback trigger.** If, at the Phase 3 gate (gd-SIXiBKFtTM), app-server cannot (a) run seeded hooks, or (b) survive a multi-hour thread with a real model, switch the driver to option (2) and keep the same normalised-event output (§9). The normalised event layer is the seam that makes this a driver swap and not a rewrite. Option (2) is the first shippable slice if only app-server is blocked.

**Consequence: no terminal screen for Codex pods.** The beads-viewer agent terminal and Slack screen snapshots render a PTY screen for Claude. A Codex pod has no TUI to show; those surfaces must render coop's normalised transcript for `harness=codex`. This is a Phase 4 item (gd-0D8aGbRwYp), listed so it is not discovered late.

## 4. B. Harness selection

**Decision.** New field `harness` with values `claude` | `codex`.

- Resolution order, most specific wins: **agent bead (spawn-time override) > role bead > project bead > global default `claude`**. The task text states "role > project > global" and "overridable on agent beads"; an explicit per-spawn override is the most specific so it sits first. A missing or empty value falls through; an unknown value is a hard error, never a silent fallback to `claude`.
- The field is resolved **once, at spawn**, by the controller, and written onto the agent bead so every downstream consumer (Slack card, metrics label, `gb init`, coop launch) reads the resolved value and none re-implements the resolution. The kbeads schema and the resolver are gd-6SezxhSEEk's job; this ADR fixes the semantics only.
- **Account validity.** An account bead carries `provider` [REPO: `gb account create --provider`]. The controller validates at spawn: `harness=codex` requires `provider=openai`; `harness=claude` requires a non-`openai` provider (`anthropic`, `bedrock`, litellm-fronted, ...). A mismatch fails the spawn with a message naming both values; it never falls back to another account. Account selection (`selectAccount`, `isHealthyAuthorized`) filters to the harness's providers before scoring, so a Codex role can never be handed a Claude token or the reverse.
- The `provider=openai` + `billing_type=subscription` account shape is defined in §5. A future metered `provider=openai` account is possible and uses the same validation.

## 5. C. Credentials and billing

**Decision: ChatGPT-subscription auth, as set by the owner direction and fixed in gd-3gTWiNEhg1.** This supersedes the task text's API-key-via-LiteLLM default and its "ChatGPT-plan auth is a non-goal" line. Summary of the contract (read the decision for the full text and the rejected alternatives):

- **Two Secrets per login.** `gardenio-codex-auth` (controller namespace, key `auth.json`, the exact file `codex login --device-auth` wrote, controller ServiceAccount only) and `gardenio-codex-auth-session` (agents namespace, same document with a placeholder `refresh_token`, mounted read-only into Codex pods). The refresh token never reaches an agent pod.
- **`gardenio-codex-auth` is operator-created, not ExternalSecret and not Helm-owned**, so nothing reverts the keeper's write-back to an already-rotated token. Recovery from loss is a new device login, not a restore.
- **Account bead:** `provider=openai`, `credential=secret:gardenio-codex-auth#auth.json`, `billing_type=subscription`, `model=<default>`, label `project:<p>`. No `base_url`, no `litellm_key`.
- **Single-writer refresh keeper in the controller** (modelled on `github_app_credentials.go`): refresh the access token before expiry via `auth.openai.com/oauth/token` [BIN], persist the rotated document to `gardenio-codex-auth` first, then the `-session` Secret, then push to coopmux `POST /api/v1/credentials/set` so live pods update without a restart. Never retry a refresh that returned a reuse/invalid error; mark the account `probe_auth_status=rejected` and file a re-login escalation.
- **In the pod:** the session file is copied to `$CODEX_HOME/auth.json` (0600, writable), `cli_auth_credentials_store = "file"` pinned so keyring is never tried [BIN]. A Codex that tries its own refresh with the placeholder fails harmlessly.
- **App-server mode adds a better path:** the `account/chatgptAuthTokens/refresh` server-to-client request and externally supplied ChatGPT tokens (`account/login/start`) are the upstream shape for "the host owns the refresh" [SPIKE Q8, BIN]. Decision: **the Phase 3 driver targets this**; the `auth.json` push is the fallback and the only path in `exec` mode. Which is primary is gated by §10 R2/R3.
- **Spend and limits.** No dollar spend: `billing_type=subscription`, `total_cost_usd` stays 0, and `reconcile_session_spend.go` plus the LiteLLM tag path skip `provider=openai` accounts. Limits come from the keeper reading `wham/usage` [BIN] (and `account/rateLimits/*` in app-server), mapped onto the existing `utilization_5h|7d`, `throttle_reset_*_at`, `representative_claim`, `throttle_status` fields so `selectAccount` works unchanged for a Codex pool; the read **fails open** (a failed read leaves the account eligible, only `limit_reached` or an auth rejection excludes it). Usage counts against the owner's ChatGPT plan limits.

**Trade-off, stated plainly.** Subscription auth buys plan credits and no per-token bill, and costs: no LiteLLM spend tracking (the reconciler's tag path is unused), a rotating single-use refresh token that makes Codex the first rotating credential in the fleet, a dependence on undocumented-to-us ChatGPT backend routes (`wham/usage`), and the plan's shared rate limit across every Codex pod. The crash window between OpenAI returning rotated tokens and the Secret write loses the login; mitigated, not eliminated (decision §2).

**Pinned in `config.toml`:** `cli_auth_credentials_store = "file"`. Also decide-now, set-in-Phase-2: `check_for_update_on_startup = false` (there is no env-var equivalent of `DISABLE_AUTOUPDATER`; this key is the only knob) [BIN], and consider disabling `analytics_enabled` in turn metadata, which the spike saw set `true` on every request [SPIKE Q2/3].

**Out of scope here, still real:** the Helm/RBAC change to let the controller `get/update` `gardenio-codex-auth*` (resourceNames-scoped) and `create/update` the `-session` Secrets is a privileged-lane `helm_values_change` escalation filed from the Phase 1 credential task (gd-XKWQYc7ZFh, to be re-scoped per the decision), not from this ADR.

## 6. D. Model names

- **No LiteLLM model groups for v1.** Codex talks to the ChatGPT backend directly (subscription auth), so the `model_providers.litellm` / `wire_api=responses` routing the task text proposed is not on the path. gd-slepeO88Y7 ("LiteLLM: Codex model groups") is therefore **out of v1 scope** and should be re-labelled or deferred by the plan gardener, not implemented speculatively. (LiteLLM remains the route for a future metered API-key account; the spike showed `x-litellm-tags` custom headers reach the provider verbatim, so that door stays open. [SPIKE Q2])
- **Model ids come from Codex's own model list, not from this ADR.** The ADR deliberately pins no `gpt-*` id: the subscription's available models are discovered, not assumed, and the spike could not run a real turn to learn them. The account bead's `model` field holds the per-account default; the concrete default is set in Phase 2 (gd-kmduHGcH5z) after reading the real list from a logged-in pod. Unknown ids are *accepted* by Codex with a degraded-metadata warning [SPIKE Q2/3], so validation must be ours.
- **`--model` mapping.** For `harness=codex`, `--model` resolves to the app-server `model` parameter on `thread/start`/`turn/start` and, for the `exec` fallback, `-m` / config `model`. It is never written to `ANTHROPIC_MODEL`.
- **Slack alias map** (`internal/bridge/bot_mentions_model.go`): today `opus`/`sonnet`/`glm` resolve to Claude alias keys and `isKnownFullModelID` is a Claude-only allowlist. Make it **harness-aware**: the alias table and allowlist are selected by the target role's resolved harness; a Claude alias on a Codex role (or the reverse) is rejected with a message, **not** passed through, because a Claude id on Codex would degrade silently per the unknown-model behaviour above. Add short alias(es) for the Codex default only once Phase 2 has fixed the real ids.
- **Context window.** `internal/contextwindow.WindowTokensForModel` is Claude-shaped (200k default, 1M for long-context). Codex models need entries or the occupancy percentage is wrong. Prefer taking the window from the app-server `thread/tokenUsage/updated` payload if it carries one [UNVERIFIED, §10 R7] over a hard-coded table.

## 7. E. Context injection mapping

One line each. All paths relative to `$CODEX_HOME` unless absolute; `$CODEX_HOME` is a per-pod writable dir (not the read-only Secret mount).

| Claude today | Codex | Status |
|---|---|---|
| `--append-system-prompt-file` (`gb prime` output) | `$CODEX_HOME/AGENTS.md`, written at pod init | [SPIKE Q7] works. Caveat: injected as a **user-role** message, not system, and re-sent as a new message on every resume. |
| `CLAUDE.md` in the workspace | `project_doc_fallback_filenames = ["CLAUDE.md"]` (top-level key, before any `[table]`) | [SPIKE Q7] works; found at the git root. Nested-dir merge, `project_doc_max_bytes` truncation, and `AGENTS.md`-vs-`CLAUDE.md` precedence are untested (§10 R6). |
| `.mcp.json` | `[mcp_servers.<name>]` tables in `config.toml` (stdio command/args/env) | Generated by gd-kmduHGcH5z from the same source of truth. Not exercised in the spike (§10 R6). |
| `~/.claude/settings.json` hooks (`defaultHookSettings`) | `$CODEX_HOME/hooks.json` **plus pre-seeded `[hooks.state."<hooks.json>:<event>:<g>:<h>"] trusted_hash`** in `config.toml`; **not** `--dangerously-bypass-hook-trust`, **not** `/etc/codex` | [SPIKE Q6] seeded trust runs all five tested events with no bypass flag; editing a hook flips it to `modified` and stops it, which is the property we want. See §7.1. |
| Claude channels MCP server (nudge delivery) | **none** | Nudges go through the driver (`turn/steer`/`turn/start`), §3. |
| `skills/` symlinks | `$CODEX_HOME/skills/<name>` symlinks, same role toggle | Added by gd-akab5xu9QQ, §13. |
| `agents/`, `commands/` symlinks | **non-goal** | §11. No Codex counterpart. |

### E.1 Hook trust: seeded `[hooks.state]`, and why not the alternatives

- `--dangerously-bypass-hook-trust` works [SPIKE Q6] but turns the trust model off for anything in `$CODEX_HOME`/project config, and prints a warning as two `error` items per `exec` run. Rejected as the default.
- `/etc/codex/managed_config.toml` / `requirements.toml` with `allow_managed_hooks_only` exist in the binary [BIN] and would make hooks `managed` (always trusted). **Not adopted for v1**, for two reasons: (1) it needs an image-baked root-owned file and the agent pod has no root, so it could not be tested [SPIKE]; (2) **its precedence over `$CODEX_HOME/config.toml` is unknown** (§10 R5), and Phase 2 writes per-agent model/provider config exactly there, so a baked policy could silently override per-agent settings. Decision: **bake nothing into `/etc/codex` in v1** (consistent with PR #361). Revisit only after R5 is answered by a test with the file present.
- Seeding procedure (from the spike, becomes gd-mYF3T8Qxnn's init step): write `hooks.json`; start `codex app-server`; `hooks/list`; for each hook write `key` and `currentHash` into `config.toml`; launch with no bypass flag. Costs one extra app-server start at pod init. The hash is content-derived and independent of the `$CODEX_HOME` path, only the key embeds the path [SPIKE Q6].
- **Implemented (gd-mYF3T8Qxnn), details in [`gardenio/docs/codex-hooks.md`](../../gardenio/docs/codex-hooks.md).** Seeding confirmed on `codex-cli 0.162.1`: `hooks/list` then `[hooks.state]`, re-list shows `trusted`, an edit shows `modified`. `PreCompact`, `PostCompact`, `SessionEnd`, `SubagentStart|Stop` and `PermissionRequest` are accepted in CamelCase and are reported as `pre_compact` etc. Relay hooks are `async: true`. Plain stdout from `PostToolUse` is ignored by Codex (`SessionStart` stdout is delivered), so `drain-queue.sh` and `prime --enqueue` are not installed and re-priming after compaction goes through `SessionStart` (`source=compact`).
- **Event names.** `hooks.json` accepts CamelCase (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Stop`) [SPIKE Q6]; hook keys and `hooks/list` use snake_case (`pre_tool_use`). The binary also knows `pre_compact`, `post_compact`, `session_end`, `subagent_start|stop`, `permission_request`, `interrupt` [BIN] but only the five above were exercised. Hook mapping is therefore a **translation** of `defaultHookSettings`, not a copy: Claude-only events (`PostToolUseFailure`, `StopFailure`, `TaskCompleted`, `TeammateIdle`, `CwdChanged`, `FileChanged`, `ConfigChange`, `InstructionsLoaded`, `Elicitation*`, `Worktree*`, `Notification`) have no confirmed Codex counterpart and are dropped for v1; the ones with a counterpart (`PreCompact`, `SubagentStart|Stop`) are mapped only after Phase 2 confirms their casing and payloads.
- **Payload compatibility.** `tool_name` arrives as the Claude-compatible `Bash` and `tool_input.command` has Claude's shape [SPIKE Q6], so `gb hook relay` / handlers keyed on tool name are shared. Common stdin fields: `session_id`, `transcript_path` (the rollout path), `cwd`, `hook_event_name`, `model`, `permission_mode`.

## 8. F. Stop gate

The gate is what keeps an agent alive until its wrap-up is complete, so it is the highest-stakes hook.

**Decision.** `gb hook stop-gate` gains a Codex output mode (`--format codex`; defaulted from the resolved harness env so the hook command in `hooks.json` need not differ).

- **Today (Claude):** `hook_stopgate.go` prints `{"decision":"block","reason":...}` to **stderr** and `os.Exit(2)` on both block paths (pending decision/yield gate, and wrap-up-completeness gate) [REPO].
- **Codex:** print the same JSON to **stdout** and exit **0**. This is the form verified in the spike: it continued the turn, injected the reason as a `<hook_prompt>` user message, and re-fired Stop with `stop_hook_active:true` [SPIKE Q6]. A hook that prints nothing and exits 0 lets the turn end. **Exit code 2 was not tested on Codex** [SPIKE Q6], so the JSON-on-stdout/exit-0 form is the contract and exit 2 must not be relied on.
- All gate logic (cooldown, block-count backoff, `stopGateMaxBlocks` escape hatch, yield marker, wrap-up check, job-mode bypass) is unchanged and shared; only the final emit differs. `stop_hook_active` in the payload is available as a belt-and-braces loop guard but the existing cooldown/escape-hatch stays authoritative.
- **Failure-safe default.** If the gate cannot determine the format it emits the Codex form only when the resolved harness is `codex` (env exported by `gb init`, gd-nSMOohBShB); otherwise Claude's. A wrong-format block that the harness ignores would fail **open** (agent exits without wrap-up), so the Phase 2 test (gd-mYF3T8Qxnn) must assert a Codex turn is actually continued by the real gate binary.
- **Fallback if hooks do not fire under app-server** (§10 R4): the driver itself implements the gate: on `turn/completed` it calls the same decision logic (`gb hook stop-gate --format codex` as a subprocess, reading its JSON) and, if blocked, issues `turn/start` with the reason. This keeps one source of truth for the policy.

## 9. G. Session persistence and Go consumers

**What is persisted.** The rollout file `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<local-ts>-<session-id>.jsonl`; **session id == `thread_id`** (UUIDv7); the same path is `transcript_path` in every hook payload [SPIKE Q5]. The `$CODEX_HOME` sqlite DBs (`state_5`, `thread_history_1`, `logs_2`, ...) are **not** needed for resume and are not uploaded.

**Restore.** Restore exactly the one rollout file into a fresh `$CODEX_HOME/sessions/...` plus a regenerated `config.toml`, then resume: `thread/resume {threadId}` in app-server mode, or `codex exec resume <id>` (repeat `--yolo`) in fallback. Verified against a mock for `exec`; the model request after restore contained full history [SPIKE Q5]. Each resume re-injects the `AGENTS.md` block as a new user message, so context grows slightly per restart (accepted).

**What coop uploads to S3.** Periodic and at-exit snapshots of the rollout file, using the same bucket/prefix/`<N>.jsonl` snapshot scheme as Claude transcripts so object lifecycle, naming and `COOP_S3_*` configuration are reused (`internal/deathtranscript` documents the layout [REPO]). coop **also** writes a normalised event stream next to it (below). Credentials (`auth.json`) are never uploaded.

**Go consumers.** Four consumers read Claude's session JSONL today: `cmd/gb/session_usage.go`, `internal/deathtranscript`, `internal/deathreport`, `internal/contextwindow`. **Decision: normalise in coop; do not teach each Go consumer a Codex parser.** coop emits its existing normalised events (`assistant_text`, `tool_use`, plus usage and error events) for every driver; Go consumers read those and stay format-agnostic. Rationale: one Codex parser in Rust next to the driver that already understands the protocol, versus four Go parsers that must each track Codex rollout schema drift (`rollout-sample.jsonl` shows several record types, some undocumented to us). Concretely:

- Usage: `token_usage_record` in the rollout and `thread/tokenUsage/updated` in app-server both carry per-response usage [SPIKE Q5/Q8]; coop maps either to the normalised usage event. `session_usage.go` consumes that, so `SessionUsage` fields populate with `total_cost_usd = 0` (subscription).
- `deathtranscript`/`deathreport` keep rendering "last N turns" from normalised `assistant_text`/`tool_use`; the raw rollout is still stored and downloadable (the deathtranscript raw-bytes accessor already exists for exactly this).
- Normalised events must be **versioned and schema-checked in coop tests** with the committed fixtures in `coop/tests/fixtures/codex/`, so drift fails CI, not production.
- The gd-MolQ67L3xX task ("Go transcript consumers") is thereby reduced from "write Codex parsers" to "make the Go side consume the normalised stream and tolerate `harness=codex` having no Claude-shaped JSONL"; the plan gardener should rescope it.

## 10. Open risks (UNVERIFIED), each with the step that closes it

None of these were answerable without a successful real-model turn or root in a pod. They are assumptions this ADR depends on; each names its closing step.

| # | Risk | Closed by |
|---|---|---|
| R1 | No successful real-model stream observed: reasoning items, real `usage`, real model ids, real `store`/`previous_response_id` interplay with resume. All fixtures except the 401 are mock-backed. | First task of Phase 1 pod wiring, once the `gardenio-codex-auth-session` Secret is mounted in a pod (the Secret exists; nothing mounts it; the spike pod had no `auth.json` and no k8s token). |
| R2 | Refresh flow: does Codex accept a placeholder `refresh_token` and surface a clean 401 rather than crashing; does the guarded reload adopt an externally rewritten `auth.json` (rename-replace) for the same `account_id`; refresh request body, rotation, real access-token lifetime; reuse revoking one token vs the whole family. | gd-3gTWiNEhg1 §6 items 1, 2, 4, 6 — run as part of R1's task. |
| R3 | App-server externally supplied tokens: does `account/login/start` with host-provided ChatGPT tokens plus `account/chatgptAuthTokens/refresh` round-trip; what does the server send near expiry. Decides primary vs fallback in §5. | gd-3gTWiNEhg1 §6 item 3; Phase 3 driver gate (gd-SIXiBKFtTM). |
| R4 | Hook behaviour under **app-server**: `hooks/list` showed `untrusted` and no `hook/started` notification fired in the spike's untrusted run; whether seeded `[hooks.state]` makes hooks run under app-server (it does under `exec`) was not tested. If not, the §8 driver-side gate fallback is mandatory. | gd-mYF3T8Qxnn / gd-SIXiBKFtTM first integration test. |
| R5 | `/etc/codex/{managed_config,requirements}.toml` precedence over `$CODEX_HOME/config.toml`, and whether `allow_managed_hooks_only` replaces seeded trust. Needs root or an image change. v1 bakes nothing, so v1 is not blocked. | A test image with the file present, if/when someone wants managed hooks; not on the v1 path. |
| R6 | Untested context mapping: nested-directory `CLAUDE.md` merging, `project_doc_max_bytes` truncation (this repo's `CLAUDE.md` plus `gb prime` output may exceed it), `AGENTS.md` vs `CLAUDE.md` precedence in one dir, `mcp_servers.*` stdio behaviour, PreToolUse deny and `additionalContext` return shapes. | gd-kmduHGcH5z acceptance tests. |
| R7 | Model ids available to the subscription, and whether app-server reports a context window. | Phase 2 config step after R1. |
| R8 | Approval round-trip never exercised (everything ran `approval_policy=never`); stop-gate exit-code-2 behaviour on Codex unknown. | gd-SIXiBKFtTM approval handler test; F mandates JSON/exit 0 so exit 2 is not depended on. |
| R9 | Sigstore bundle for the pinned binary not verified (no `cosign` in pod; `gh attestation verify` returned 404). Today assurance is the GitHub-recorded sha256 plus a byte-identical image binary. | CI step where `cosign` exists; pin the digest in `codex.sh`. |
| R10 | `wham/usage` is an internal ChatGPT backend route; auth headers and response shape unverified. Fail-open handling in §5 bounds the damage. | gd-3gTWiNEhg1 §6 item 5. |
| R11 | Websocket-first transport: with the default provider Codex tries `wss://` five times (~7 s) before falling back to HTTPS on a flaky path. | Measure on a real subscription turn; consider `supports_websockets=false` in generated config. |

## 11. H. Non-goals for v1

Written down so later phases do not drift into them:

- **Codex plugins**, and the `agents/`, `commands/` symlink machinery. No parity attempt. (Skills were a v1 non-goal and are now done, §13.)
- **Agent teams / subagent orchestration** (Claude-only hook events dropped, §7). Ultracode is decided in §13: refused on codex, not implemented.
- **claudemore** recording/replay for OpenAI traffic (claudemore records Anthropic-wire traffic). Designed afterwards in [ADR 0006](0006-codex-recording-proxy.md) (gd-sssOUVd1g6); still not built.
- **`lambda_microvm` execution target**: Codex pods run on the k8s pod target only. The controller now refuses the combination (addendum A1 below).
- **Gemini parity**: coop's Gemini driver is untouched and does not inform this work. (Taken up separately in [ADR 0005](0005-gemini-harness.md).)
- **LiteLLM spend tracking and model groups for Codex** (new vs the task text; follows from §5/§6). A metered API-key account is a possible later addition, not v1.
- **API-key (metered) OpenAI accounts.** `provider=openai` means subscription in v1.
- **Codex's own sandbox** (`bwrap`/Landlock); the pod is the sandbox (§3).
- **A Codex TUI / terminal-screen view** (§3 consequence).
- **Managed `/etc/codex` policy** (§7.1, R5).
- Cross-harness session migration: a thread started on Claude is not resumed on Codex or vice versa.

**What the operator may still overturn.** If the operator disagrees with a recommendation here, that is a `gb decision` filed from the authoring agent, not a silent edit. The three places most likely to draw disagreement: (1) A, app-server over `exec` as the primary; (2) §6, dropping the LiteLLM model-group work (gd-slepeO88Y7) from v1; (3) §7.1, seeded `[hooks.state]` over a managed policy.

## 12. Consequences and follow-up impact on the plan

Beads this ADR changes or constrains (the plan gardener owns the edits; listed so nothing is missed):

| Bead | Effect |
|---|---|
| gd-XKWQYc7ZFh (OpenAI credential account) | Re-scope to the subscription contract (`#auth.json`, `-session` Secret, `CODEX_HOME`, generalised credential guard); keeper becomes its own step. Helm/RBAC via escalation. |
| gd-slepeO88Y7 (LiteLLM Codex model groups) | Out of v1 (§6). Defer or relabel. |
| gd-6SezxhSEEk (harness field) | Implement the §4 semantics: precedence, spawn-time resolution onto the agent bead, provider validation. |
| gd-kmduHGcH5z (config injection) | Writes `AGENTS.md`, `config.toml` (`project_doc_fallback_filenames`, `cli_auth_credentials_store`, `check_for_update_on_startup`, `mcp_servers`, `[hooks.state]`), no `/etc/codex`. |
| gd-mYF3T8Qxnn (hooks) | Translate, not copy (§7.1); `stop-gate --format codex` (§8); seeding init step. |
| gd-SIXiBKFtTM (coop driver) | App-server JSON-RPC client per §3, with the exec fallback and the §8 gate fallback. |
| gd-hoYFy5n8mh (provider-aware credentials) | Add the rotating-credential path (`CredentialEvent::Refreshed` currently ignores `refresh_token`/`expires_in` for static keys). |
| gd-JeZ78WY3we (rollout persistence) | Rollout-file-only upload/restore per §9. |
| gd-MolQ67L3xX (Go consumers) | Rescope: consume normalised events, no Codex parsers. |
| gd-0D8aGbRwYp (observability/Slack) | Harness label; render the normalised transcript instead of a PTY screen. |

## 13. I. Agent teams and ultracode on Codex (addendum, gd-g2LbzbNEIY)

Bead: gd-g2LbzbNEIY, operator decision gd-MxiDBDi9UM ("start"). Scope-out from §11; evidence tags as above.

**What the Claude side is.** `BOAT_ULTRACODE` (`on`/`off`) is resolved by the controller across project < role < agent (`resolveUltracode`, `podspec.go`; the agent layer is the `--ultracode` Slack flag) and read only by `gb setup claude`, which writes `ultracode`, `CLAUDE_CODE_WORKFLOWS=1` and an effort level into `~/.claude/settings.json` [REPO]. It is a Claude Code client setting. Nothing in the pod, coop or the controller implements "teams" or "workflows"; they are Claude Code features switched on by that file.

**The Codex candidates.**

| Candidate | Verdict |
|---|---|
| Multi-thread app-server (several `thread/start` on one `codex app-server`, §3) | The transport supports it [BIN], but coop's Codex driver (§3) models one agent as one thread, and the Stop gate (§8), rollout persistence (§9) and the Slack transcript are all keyed to that single thread. Several threads would need a new session model in all of them. Not a flag; a feature. |
| Spawned `codex exec` workers (fan-out from inside the pod) | A worker is a second Codex process sharing the one ChatGPT-subscription `auth.json` and its refresh keeper (§5), outside coop's driver, the stop gate and the pod's observability. Fleet-level fan-out already exists and is the supported shape: `gb spawn` creates a separate agent (own pod, bead, credentials, gate). |
| Codex-native subagent hook events (`subagent_start\|stop`, §7.1) | Present in the binary [BIN], never exercised in the spike. They are lifecycle events, not an enabling switch. |

**Decision: `BOAT_ULTRACODE` is not implemented on codex; an explicit request is refused, not ignored.**

- Per-spawn `--ultracode=on` (agent metadata `ultracode=on`) on a spec whose resolved harness is `codex` sets `HarnessError` in `applyUltracode`. The spawn path already turns that into a permanent refusal on every door (events, reconciler, stuck-restart recreate), so no pod is created and the agent bead fails with: `invalid agent harness: --ultracode=on is not supported on harness "codex": ultracode is a Claude Code workflow/subagent setting and Codex has no equivalent; drop --ultracode, or spawn with --harness claude`. Silently dropping it would let an operator believe an ultracode run is under way.
- `--ultracode=off` is accepted as a no-op: it asks for what a codex pod already is.
- A project- or role-level `ultracode` config is **ignored** on codex specs (not refused), so a mixed project that opts its Claude roles in does not break its codex roles. `BOAT_ULTRACODE` is never set on a codex pod.
- Claude specs are unchanged. The check keys on the resolved harness, so the bare `--ultracode` flag still works when `--harness claude` (or a claude role) is in effect.

**Revisit when.** A real need for in-pod parallelism on Codex appears. The route is the app-server multi-thread model, designed against §8/§9 first (the Stop gate must hold for the worst thread, and rollouts must be per thread); `codex exec` workers are not recommended. Until then the Codex equivalent of a team is several agents spawned with `gb spawn`.

## 13. Addendum: skills parity (gd-akab5xu9QQ)

Claude pods link the primary and sibling repos' `.claude/skills/*` into `~/.claude/skills`, filtered by the role bead's `enabled_skills` / `disabled_skills` ([`agent-skill-toggle.md`](../../gardenio/docs/agent-skill-toggle.md)). Codex pods now get the same set.

- **Mechanism: skill symlinks, not plugins and not AGENTS.md fragments.** Codex discovers `<CODEX_HOME>/skills/<dir>/SKILL.md` as user-scope skills, through symlinked directories. [SPIKE-style check, `codex-cli 0.162.1`: an app-server `skills/list` returned a symlinked copy of this repo's `video` skill with its name and description.] It reads the same `name`/`description` front matter and ignores Claude-only keys such as `allowed-tools`. So `.claude/skills` stays the single source and nothing is converted or copied. AGENTS.md fragments were rejected because they would load every skill body into context up front, defeating progressive disclosure; plugins were rejected because they need a marketplace/manifest layer and `plugins`/`marketplaces` are denied keys in a codex-config bead (§ `codexDeniedKeys`) for good reason.
- **Same toggle by construction.** `gb setup codex` calls `linkExtensionSubdir`, the function the Claude path uses, with the `skills` filter from `fetchRoleExtensionFilters`. Naming (primary bare, sibling `{repo}-` prefixed), primary-wins collisions, deny-wins, multi-role union and fail-open when the daemon is down are therefore identical. A test asserts the Claude and Codex linked sets match for several role shapes.
- **Restart hygiene.** `$CODEX_HOME` can outlive one start, so setup first prunes symlinks that point into a `.claude/skills/` tree, then relinks. A tightened toggle takes effect on the next start. Real directories (Codex's `.system` skills, skill-installer output) and foreign symlinks are untouched.
- **Out of scope still:** sub-agents (`agents/`) and slash commands (`commands/`) have no Codex counterpart; skills whose body assumes Claude-only tools (e.g. `allowed-tools: Agent`) link but may not work as written.
- **Code:** `gardenio/controller/cmd/gb/setup_codex_skills.go`, wired from `runSetupCodex` / `runSetupCodexDefaults`. Tests: `setup_codex_skills_test.go`.

## Addendum A1 — `lambda_microvm` with Codex: refuse, do not support (gd-jyideibol0)

Operator decision gd-MxiDBDi9UM started this scope-out. Two options were weighed: make the microvm lifecycle harness-aware, or refuse `harness=codex` on `execution_target=lambda_microvm`.

**Evidence [REPO]** that full support is large:

- `images/microvm-agent/Dockerfile` installs Claude Code only (no `codex` binary, no `codex.sh` step) and pre-creates `~/.claude` and `.state/claude`.
- `images/microvm-agent/entrypoint.py` materialises exactly one credential file, `~/.claude/.credentials.json` (`CLAUDE_CREDENTIALS_PATH`), from the per-run secret's `claudeCredentials`. There is no `CODEX_HOME`, no `auth.json`, no hooks seeding.
- Controller side, `handleNonK8sSpawn` resolves only a Claude OAuth token (`resolveMicrovmClaudeOAuthToken`) and pushes it into the per-run Secrets Manager secret. The Codex path instead depends on the keeper-minted `gardenio-codex-auth-session` Kubernetes Secret (§5), which a Lambda MicroVM cannot mount; it would need a second delivery channel and a rotation story.
- Session persistence (§9) uploads the rollout file via the pod path; the microvm S3 session-credential vendor is Claude-state shaped.

**Decision.** Refuse. `refuseHarnessOnTarget` (`gardenio/controller/cmd/controller/podspec_harness.go`) runs right after `resolveExecutionTarget` in both spec builders and sets `HarnessError` when `harness=codex` and the target is `lambda_microvm`. That reuses the existing harness refusal on all three spawn doors (event, recreate, reconciler): permanent, no escalation, reason names the fix (`harness=claude` or `execution_target=kubernetes`). It is a fail-closed change: nothing that worked before is refused, since Codex on microvm never booted a working agent.

**Follow-up (not done here), if the owner wants Codex on microvm:** (1) add the pinned Codex install step to the microvm Dockerfile; (2) teach `entrypoint.py` to write `$CODEX_HOME/auth.json`, `config.toml`, `AGENTS.md` and seed `[hooks.state]` when `GARDENIO_HARNESS=codex`; (3) deliver the session `auth.json` through the per-run secret and refresh it, since the keeper's `-session` Secret is not reachable; (4) extend the S3 session vendor to the rollout file; (5) drop `refuseHarnessOnTarget`. That is a separate epic, not a phase-4 chore.
