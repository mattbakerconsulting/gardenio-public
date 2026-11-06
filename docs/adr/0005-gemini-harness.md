# ADR 0005 — Gemini harness: bringing the Gemini driver to the ADR 0004 bar

> Status: **Accepted** (2026-10-16, phase 0 spike gd-llfzgEJZNN, [spike.md](../research/gemini-harness/spike.md)). The Gemini CLI 0.64.0 has been run in a pod against a scripted API stand-in; items needing a live API key are carried (see the amendment at the end and §9). Phase 3 (the coop ACP driver) is implemented; see the second amendment at the end.
> Bead: gd-lWkvvnWMBE. Parent epic: gd-J9Wkw5HjF6; plan: gd-8s8FdZu8UA. Operator decision gd-MxiDBDi9UM chose "start".
> Builds on: [ADR 0004](0004-codex-harness.md) (read it first; this ADR reuses its sections, tags and vocabulary and only records where Gemini differs).
> Audit: the gap list is the first comment on gd-lWkvvnWMBE (G1-G18). The table in §3 is the same list, grouped by ADR 0004 section.

Evidence tags as in ADR 0004: **[REPO]** read in this repo. **[UNVERIFIED]** inference or memory; each has a row in §9 naming the step that closes it. **[SPIKE]** (added by the phase 0 amendment): observed on the pinned Gemini CLI 0.64.0 in an agent pod; evidence in [spike.md](../research/gemini-harness/spike.md) and `coop/tests/fixtures/gemini/`. The spike had no Gemini API key, so [SPIKE] here means the CLI's own behaviour against a scripted API stand-in; the live-API half is tagged **[LIVE]** and carried. The sections below are left as originally proposed; the amendment at the end records what the spike overturned.

## 1. Context

coop has an `AgentType::Gemini` driver (`coop/crates/cli/src/driver/gemini/`, ~1.2k lines including tests) that predates the harness work [REPO]. It drives a Gemini CLI in a PTY: Tier 1 hooks written into a settings file passed through `GEMINI_CLI_SYSTEM_SETTINGS_PATH`, Tier 4 process monitor, a Tier 3 stdout classifier for a `{"type":"init|message|tool_use|tool_result|result|error"}` stream that nothing currently feeds with a fixture, and screen-scraped permission/plan encoders. It has no usage or last-message extraction, no session log, no resume, and a stop gate that is a curl shim inside the hook command rather than `gb hook stop-gate` [REPO].

Everything gardenio built for Codex in phases 1-4 is a two-valued `claude | codex` assumption: the `harness` enum in kbeads and `beadsapi.ParseHarness`, the controller's provider/account switch, `internal/harness`, `gb setup codex`, `relayHarness`, `stop-gate --format`, the Slack alias table, the bounded `harness` metric label, `deathtranscript`/`contextwindow`/session usage [REPO]. The existing tests assert that `gemini` is **rejected** as an unknown harness, so adding the value is a deliberate flip of those tests, not a gap that happens to be open.

Goal: a role can run on Gemini with the same guarantees as a Codex role (spawn-time resolution, account/provider safety, injected config, working hooks and stop gate, usable Slack and observability), without forking the platform.

## 2. Decisions at a glance

| § | Decision |
|---|---|
| A | **Spike first.** Pin and install a Gemini CLI in the agent image, then answer §9 R1-R6 against the real binary. The driver mode is chosen from the spike, in this order: (1) a long-lived structured protocol if one works, (2) headless per-turn JSON (`-p` with a JSON output mode), (3) the existing PTY + hooks driver, hardened. Until then the PTY driver is the baseline and is not removed. |
| B | `harness` gains the value `gemini`. Provider validation becomes **table-driven** (`harness -> allowed providers`), replacing the codex/non-codex `if`, so a Claude role can never be handed a Gemini account and the reverse. |
| C | v1 credential is a **Gemini API key** account (`provider=gemini`, which `coop/crates/mux/src/credential` already maps to `GEMINI_API_KEY` [REPO]). Google-account OAuth/subscription login is a non-goal until a keeper-shaped contract is decided separately. |
| D | Model names are Gemini's own, validated against a harness-scoped allowlist; no LiteLLM model groups in v1; Slack aliases only after the spike fixes real ids. |
| E | Context injection by `gb setup gemini` (moves config ownership from coop to gardenio, as with Codex): `GEMINI.md`, settings file with `mcpServers` and hooks, pinned update/telemetry keys. |
| F | `gb hook stop-gate` gains `--format gemini`; the in-driver curl shim is retired once the contract is verified. |
| G | Persistence and Go consumers follow ADR 0004 §9: coop normalises, Go consumers stay format-agnostic. |
| H | Non-goals for v1 (§8). |

## 3. Gap map (audit, grouped by ADR 0004 section)

| ADR 0004 § | Codex has | Gemini today [REPO] | Gap |
|---|---|---|---|
| A driver | app-server + exec backends, mode detection, Tier 3 protocol parse with usage + last message | PTY only; Tier 3 classifier with no extractors; `screen_classifier: None` | G1, G2, G6 |
| B harness | enum in kbeads + `ParseHarness`; controller resolver; provider switch; `selectAccount` filter | tests assert `gemini` is rejected; provider logic is `openai` vs not-`openai` | G10, G11 |
| C credentials | keeper, `auth.json`, env allowlist | mux maps `gemini`/`google` to `GEMINI_API_KEY`; run.rs allowlists it; no keeper, no env allowlist in `internal/harness` | G8, G12 |
| D models | harness-scoped allowlist, Slack harness-aware aliases | none | G16 |
| E config | `gb setup codex` + golden, `AGENTS.md`, `[hooks.state]` trust | coop writes `coop-gemini-settings.json`; no `gb setup gemini`; no `gb prime` injection; no `.mcp.json` translation | G4, G5, G13 |
| F stop gate | `--format codex`, JSON on stdout, exit 0 | AfterAgent shell shim prints `{"continue":true}` when coop says block | G4, G15 |
| G persistence | rollout upload/restore, normalised events | `session_log_path: None`, no resume, no upload | G3 |
| observability | `harness` label on tool-latency + overhead; viewer chip | label collapses `gemini` to `unknown` | G17 |
| image | pinned `codex.sh` + test | no installer, no pin, no spike | G9 |
| docs | `harness-codex.md` | none | G18 |

## 4. A. How coop drives Gemini

The Codex lesson (ADR 0004 §3) was to stop scraping a TUI where the CLI offers a structured interface. Whether Gemini offers one that is good enough is exactly what is unknown [UNVERIFIED]: the Gemini CLI has a non-interactive prompt mode and a JSON/stream-JSON output mode, and an editor-integration protocol, but their flags, event shapes, hook behaviour inside them, and stability are recalled from memory, not read from a pinned binary. The existing `parse_gemini_state` shape is a guess of the same kind.

Decision: do not pick a mode in this ADR. The spike (§7 phase 0) runs each candidate against the pinned binary and records fixtures under `coop/tests/fixtures/gemini/`. Selection rule:

1. A mode that supports interrupt, steer-or-queue, per-turn usage and seeded hooks wins (the Codex app-server criteria).
2. Else per-turn headless JSON + resume by session id (the Codex exec analogue), accepting no live view.
3. Else keep the PTY driver and harden it: real fixtures for every encoder, a Tier 3 or Tier 2 source for usage and last message, and a screen classifier or an explicit decision to live without one.

Whatever mode wins, coop emits the same normalised events as for Codex (§6); the mode is a driver detail.

## 5. B-E. Harness, credentials, models, config

**B. Harness selection.** Add `gemini` to the three kbeads enum sites (`kbeads/internal/server/config.go`: role, project/agent `harness`, `resolved_harness`, and the role `allowed_harnesses` string), to `beadsapi.ParseHarness`, and to the bounded metric label (`internal/metrics/tool_latency.go`). Resolution order is unchanged (ADR 0004 §4). Replace the `codexProvider` switch in `cmd/controller/podspec_harness.go` and the openai/non-openai split in `account_spread.go` with one map, `harness -> providers` (`claude` -> anthropic/bedrock/litellm-fronted, `codex` -> openai, `gemini` -> gemini), used by both the spawn-time check and `selectAccount`'s pre-scoring filter. A mismatch fails the spawn naming both values and never falls back to another account, as in ADR 0004. The tests that assert `gemini` is unknown flip to assert it is accepted, and add a new unknown value (`"gpt"`) in their place so the "unknown is a hard error" property keeps its coverage.

**C. Credentials.** `provider=gemini`, `credential=secret:<name>#api-key`, `billing_type=metered`, `model=<default>`, label `project:<p>`; coop already pushes it as `GEMINI_API_KEY` and the Claude/Codex-specific keys are not pushed to a Gemini account [REPO: `broker.rs`, `broker_codex_tests.rs`]. Spend: ADR 0004 skips the LiteLLM reconciler for `provider=openai`; for Gemini the reconciler skip is the default too, and spend tracking is a separate bead, because routing Gemini through LiteLLM changes the endpoint contract and has not been decided [UNVERIFIED]. `internal/harness.IsDeniedEnv` gains a Gemini allowlist (`GEMINI_API_KEY` is *not* on it: the key arrives through coop's credential push, and an inherited key from a pod spec must not win) with `GOOGLE_*`/`GEMINI_*` stripped by default, for the same reason ADR 0004 strips `OPENAI_*`.

**D. Models.** `internal/harness.ModelEnv` returns `GEMINI_MODEL`; `WithModel` maps to the CLI's model flag (flag name verified in the spike). `bot_mentions_harness.go` selects its alias table and full-id allowlist by the target role's resolved harness; a Claude or Codex id on Gemini is rejected with a message, not passed through. No short aliases until the spike reads the real model list. `contextwindow` gets Gemini entries or reads the window from the driver's usage event.

**E. Config injection.** New `gb setup gemini` (`cmd/gb/setup_gemini*.go`, golden file like `codex_config_crew.golden.toml`) owns what coop's `prepare_fresh` writes today, so the config is regenerated wholesale at pod start and overridable by beads like `codex-config`:

| Claude today | Gemini |
|---|---|
| `--append-system-prompt-file` (`gb prime`) | `GEMINI.md` in the Gemini state dir [UNVERIFIED: lookup order and merge] |
| `.mcp.json` | `mcpServers` in the settings file (coop already inserts it [REPO]) |
| `settings.json` hooks | the hook table in the settings file, **translated** not copied: `BeforeAgent`/`BeforeTool`/`AfterTool`/`AfterAgent`/`SessionStart`/`SessionEnd`/`Notification` -> the relay's canonical events; Claude-only events dropped (ADR 0004 §7.1 rule) |
| state dir `~/.claude` | `~/.gemini` (`internal/harness.StateDir`) [UNVERIFIED] |

`relayHarness` (`cmd/gb/hook_relay.go`) returns a third value from `GARDENIO_HARNESS`, and the payload sniffing fallback gains a Gemini rule only if the spike finds a stable discriminator. Whether `GEMINI_CLI_SYSTEM_SETTINGS_PATH` overrides or merges with user settings is R3; if it merges unpredictably the injection moves to the user settings file in the state dir.

## 6. F-G. Stop gate and persistence

**Stop gate.** `stop-gate --format gemini` (default from `GARDENIO_HARNESS`, failure-safe default as in ADR 0004 §8: emit the Gemini form only when the resolved harness is `gemini`). The shape is whatever the spike observes continuing a turn; the current shim's `{"continue":true}` is a recollection to test, not a contract [UNVERIFIED]. Gate logic (cooldown, backoff, `stopGateMaxBlocks`, yield marker, wrap-up check) stays shared and unchanged, and the Phase 2 test must assert a Gemini turn is actually continued by the real gate binary, because a wrong-format block fails open. The coop-side curl shim is removed once this works so there is one source of gate policy.

**Persistence.** coop uploads Gemini's own session record if one exists on disk and is enough to resume [UNVERIFIED, R5], plus the normalised event stream, using the existing snapshot scheme. Go consumers (`session_usage`, `deathtranscript`, `deathreport`, `contextwindow`) read normalised events and gain no Gemini parser. Normalised events are schema-checked against the committed fixtures so drift fails CI.

## 7. Phases (each a small reviewable PR; none touches `deploy-values/` or helm RBAC)

| Phase | Content | Gate |
|---|---|---|
| 0. Spike | `images/agent/install/gemini.sh` (pinned version + checksum, like `codex.sh` and its test); run candidates from §4 in a pod with an API-key account; commit fixtures and `docs/research/gemini-harness/spike.md`; answer R1-R6; amend this ADR to Accepted | image builds; R1-R6 each marked closed or carried |
| 1. Enum + validation | kbeads enum, `ParseHarness`, table-driven provider check, `selectAccount` filter, metric label, flip the "gemini is unknown" tests | `make test`; spawn with mismatched account is refused |
| 2. Config + hooks + gate | `gb setup gemini` + golden, hook translation, `relayHarness`, `stop-gate --format gemini`, `internal/harness` launch/model/state/env | real-binary test that the gate continues a turn |
| 3. coop driver | chosen mode, usage + last-message extraction, session log/resume, retire the curl shim | coop tests on fixtures |
| 4. Slack, observability, docs | harness-aware aliases, viewer chip, `harness-gemini.md`; Grafana panels via a helm-lane escalation | UX acceptance on the Slack change |

Phase 1 can start in parallel with Phase 0 only if the enum value is gated off at spawn (a new, proposed controller flag; no such setting exists today) so no role can select an unverified harness; otherwise it waits.

## 8. Non-goals for v1

- Google-account OAuth / subscription login and any keeper for it.
- Gemini skills, extensions, subagents and agent teams (same posture as ADR 0004 §11).
- Gemini's own sandbox; the pod is the sandbox. Approval policy is "never ask", so the driver answers any prompt that arrives by policy (deny and log) rather than hanging, as for Codex.
- claudemore recording, `lambda_microvm`, LiteLLM model groups and spend tracking for Gemini.
- Cross-harness session migration.
- Removing or rewriting the PTY driver before the spike says what replaces it.

## 9. Open risks (UNVERIFIED), each with the step that closes it

| # | Risk | Closed by |
|---|---|---|
| R1 | Which structured interface exists and is stable: flags, output event shapes, resume, interrupt, queued input. All current parsing is a guess with no fixture. | Phase 0 |
| R2 | Hook behaviour: event names and casing, payload fields, whether hooks fire in headless/protocol modes, any trust or approval requirement, exit-code and output contract for continuing a turn. | Phase 0, feeds §6 |
| R3 | `GEMINI_CLI_SYSTEM_SETTINGS_PATH` precedence vs user/workspace settings; `GEMINI.md` lookup, merge and size limits; `mcpServers` shape. | Phase 0 |
| R4 | Approval/permission behaviour with an auto-approve flag in a pod without a sandbox; prompts that still appear. | Phase 0 |
| R5 | What session record is on disk, whether it alone is enough to resume in a fresh state dir, and what usage it carries. | Phase 0, feeds §6 |
| R6 | Model ids and context windows available to an API-key account; unknown-id behaviour (silent degrade like Codex, or hard error). | Phase 0 |
| R7 | Auto-update, telemetry and first-run prompts that block a headless pod; the keys that disable them. | Phase 0 |
| R8 | Quota/rate-limit error shapes for `error_category.rs` and profile rotation. | Phase 3 |
| R9 | Binary provenance: no pinned digest or attestation exists yet. | Phase 0 (`gemini.sh`) |

## 10. Consequences

Adding a third harness is the forcing function to make the two-way assumptions table-driven now (§5 B) rather than a third copy of `if IsCodex`. `internal/harness`, `relayHarness`, the stop-gate format switch and the Slack alias table each get one more case and a shared lookup, which is the maintenance cost the spike-first order is meant to avoid paying twice. ADR 0004 §11's "Gemini parity" non-goal is superseded by this ADR for the items above; its other non-goals still hold.

## 11. Amendment: phase 0 outcome (2026-10-16)

Status moves **Proposed -> Accepted**. Decisions A-H stand except where changed here. Evidence is in [spike.md](../research/gemini-harness/spike.md); `images/agent/install/gemini.sh` (pinned 0.64.0, sha256 digest, hermetic test) is merged with this amendment.

**Changed by the spike**

1. **Driver mode (§2 A, §4): ACP first.** `gemini --acp` (JSON-RPC over stdio) is the structured protocol: long-lived, `session/cancel` interrupt, per-turn usage, `session/load` resume, `session/request_permission` for approvals. A second prompt during a turn *replaces* it (no queue), so coop serialises input. Per-turn headless `stream-json` is the fallback; the PTY driver stays until ACP is proven live. [SPIKE]
2. **Config injection (§5 E): user level, from the start.** `GEMINI_CLI_SYSTEM_SETTINGS_PATH` is skipped unless its parent directory is root-owned, which an agent pod's session dir is not, so the existing coop injection of hooks and `mcpServers` reaches the CLI as nothing. `gb setup gemini` writes `~/.gemini/settings.json` and `~/.gemini/GEMINI.md` (the latter lands in `systemInstruction`). [SPIKE]
3. **Stop gate (§6): the contract is `{"decision":"block","reason":...}` (or exit 2), not `{"continue":true}`.** The latter does not continue a turn; the current coop shim therefore fails open on 0.64.0. [SPIKE]
4. **Hooks in ACP (§5 E, §6):** only `BeforeAgent`, `AfterAgent`, `PreCompress`, `SessionEnd` fire; tool events come from `tool_call` updates. Headless and PTY fire all of them. [SPIKE]
5. **Pod env the writer must set (§5 E):** `security.auth.selectedType=gemini-api-key`, `GEMINI_CLI_TRUST_WORKSPACE=true`, `privacy.usageStatisticsEnabled=false`, `general.enableAutoUpdate=false`, a pinned `model.name`. Approval: `yolo` headless; answer `session/request_permission` in ACP. [SPIKE]
6. **Process model (Phase 3):** the CLI re-spawns itself as a child; coop kills the process group and pins the Node heap for the 2 GiB pod limit. [SPIKE]
7. **Persistence (§6):** the session JSONL is sufficient to resume in a fresh state dir; records are re-appended by `id` (dedupe, last wins); `--session-id` lets coop choose the id. [SPIKE]

**Risk table (§9) after the spike**

| # | Status |
|---|---|
| R1, R2, R3, R4, R5, R7, R9 | Closed [SPIKE] |
| R6 | Closed for the CLI-reported ids and context window; availability and unknown-id behaviour **carried to a live pass** [LIVE] |
| R8 | CLI side closed (10 retries over ~3 min, then `result.status=error`, exit 173); real quota body carried to Phase 3 [LIVE] |

**Gate for the Phase 1+ work.** Phase 1 (enum + validation) may start: nothing in it depends on a live key. Before Phase 3 declares the ACP driver done, the live pass in spike.md ("Carried to a live pass") must run once with a real `provider=gemini` account.

## 12. Amendment: phase 3 outcome, the coop driver (2026-10-16)

Bead gd-76ezzkYGHN. Every item below is covered by tests over the phase 0 fixtures (`coop/tests/fixtures/gemini/`) and a fake `gemini --acp` (`fake-gemini.sh`, `coop/crates/cli/tests/gemini_integration.rs`). No live Gemini key was available, so nothing here has run against the real API; the [LIVE] items from §11 remain open.

**Delivered**

| ADR item | Implementation |
|---|---|
| §4 driver mode: ACP | `gemini --acp` selects `GeminiMode::Acp` (`driver/gemini/mod.rs`). `driver/gemini/acp.rs` is a sans-IO ACP client (the Codex `rpc.rs` shape); `backend/gemini.rs` is the PTY-less backend. Handshake `initialize` -> `authenticate(gemini-api-key)` -> `session/new` or `session/load`, then `session/set_mode yolo` (best effort) and `session/set_model` when `GEMINI_MODEL` is set. Anything else is still the PTY driver, unchanged. |
| §11.1 input | ACP replaces a running prompt when a second arrives, so coop **serialises** input: text typed mid-turn waits for the turn to end. Escape / Ctrl-C is `session/cancel`; queued text still runs afterwards. |
| §8 approvals | `session/request_permission` is answered by policy so a turn cannot hang: allow once by default (the pod is the sandbox), or reject with `COOP_GEMINI_APPROVAL=deny`. Each answer surfaces as `coop/permission`. |
| usage | Per-turn `usage` from the `session/prompt` response (`coop/turnCompleted`) and `stream-json` `result.stats` feed `UsageState`. Cached tokens are split out of input; output is `max(outputTokens, totalTokens - inputTokens)` so thinking tokens count whether or not the API folds them into the output figure. Cost stays 0 (§5 C). |
| last message | The client assembles `agent_message_chunk`s per turn and tees `coop/agentMessage`; a cancelled turn's fragment is not offered. |
| state | ACP has no turn start/end notification, so the client injects `coop/ready`, `coop/turnStarted`, `coop/turnCompleted` (no Idle dip when another prompt is queued) and `coop/error`. The Tier 4 process monitor is not used in ACP mode (no screen). `result.status=error` is now an Error state (was Idle); a `severity: warning` error (blocked stop) is ignored. |
| errors (R8) | Messages are tagged `[gemini:quotaExhausted\|unauthorized\|contextWindowExceeded]` and mapped in `error_category.rs` (quota -> `RateLimited`, so profile rotation applies). The tags key on the text seen in the spike; the real 429 body is [LIVE]. |
| §6 persistence | `driver/gemini/session.rs`: find the `~/.gemini/tmp/<slug>/chats/session-<minute>-<id8>.jsonl` record by session id (header checked, not just the name), publish it to the session-log slot so snapshots and the S3 uploader follow it, restore it from S3 under the same layout, retire it (`.stale`). `--resume <session-id>` (ACP only; rejected for the PTY modes) drives `session/load`; the history `session/load` replays is swallowed so it is not read as activity or as the last message. Credential switches hand the session id to `gb init`. |
| process model | The Node heap is capped (`--max-old-space-size=1024` unless `NODE_OPTIONS` already sets one) and the whole process group is signalled at exit, because the CLI re-spawns itself and killing the launcher orphans the worker. |
| §6 stop gate | The coop-generated `AfterAgent` hook now echoes coop's `{"decision":"block","reason":...}` verdict, the form Gemini honours; the old `{"continue":true}` failed open (test: `stop_hook_echoes_a_block_verdict_in_the_form_gemini_honours`). |

**Not done, and why**

1. **Retiring the curl stop shim is blocked on phase 2.** ADR §6 removes it once `gb hook stop-gate --format gemini` exists; `gb` has no `gemini` format yet (`hook_stopgate.go` knows `claude` and `codex`), and deleting the shim first would leave no gate at all. The shim's contract is fixed above; delete it in the phase 2 change that adds the format and `gb setup gemini`.
2. **Hook injection still needs phase 2.** coop still writes hooks to `GEMINI_CLI_SYSTEM_SETTINGS_PATH`, which the CLI ignores in a non-root pod (§11.2). Until `gb setup gemini` writes `~/.gemini/settings.json`, an ACP pod has Tier 3 detection (state, message, usage) but no hooks, so no stop gate. Tier 3 does not depend on hooks.
3. **Headless `stream-json` per-turn backend** (the fallback in §11.1) is not built: the parser understands the stream, but no backend spawns `gemini -p` per turn. ACP is the supported mode.
4. **[LIVE], needs a Gemini API key:** whether `session/set_mode`/`session/set_model` and `session/load` behave as assumed (the wire names are from the ACP spec and the spike's `setSessionMode` note; failures are tolerated, so a mismatch degrades to the CLI's defaults rather than hanging); whether the chats directory slug is always the workspace basename (restore places the record there); real usage shape (`thoughtTokens`, `cachedReadTokens` are read if present); real 429 body; pod RSS under the 2 GiB limit with the 1024 MiB heap cap.

## 13. Amendment: phase 1 landed (gd-clUoq1aEza)

`gemini` is now a valid `harness` value (kbeads enum at the role, project/agent and `resolved_harness` sites, `beadsapi.ParseHarness`, the bridge's bootstrap schema, the bounded `harness` metric label, `gb spawn --harness`, and the Slack `--harness`/`--model gemini` parse). Provider validation is table-driven: `harnessProviders` in `cmd/controller/harness_providers.go` maps `codex -> openai` and `gemini -> gemini`, and `claude` is every provider no other harness claims, so `applyHarness`, `accountMatchesHarness` and the spread-pool filter share one rule. A mismatch (including a Claude spec on a Gemini account) fails the spawn naming both values.

The spawn gate that §7 asked for is the controller setting `GEMINI_HARNESS_ENABLED` (`config.GeminiHarnessEnabled`, default false): while off, a spec that resolves to `gemini` is refused with a permanent harness error. Turn it on only when phases 2-3 are live. The tests that asserted `gemini` is unknown now use `gpt` as the unknown value.

Not in phase 1, and still Claude-shaped for a gemini pod: `relayHarness`, `stop-gate --format`, `internal/harness` launch/model/env, `gb setup gemini`, Slack aliases, `deathtranscript` (all phases 2-4).

## 14. Amendment: phase 2 landed, config + hooks + gate (gd-xdnGt0w9tW)

`gb setup gemini` and everything that makes a `harness=gemini` pod launch and gate correctly on the `gb` side. Verified against the pinned Gemini CLI 0.64.0 with the phase 0 mock API (no key needed); the [LIVE] items from §11 are unchanged.

**Delivered**

| ADR item | Implementation |
|---|---|
| §5 E config | `gb setup gemini` (`cmd/gb/setup_gemini.go`) writes `~/.gemini/settings.json` (0600) and `~/.gemini/GEMINI.md` (`$GEMINI_CLI_HOME/.gemini` when set; `--local` confines both to `<ws>/.gemini`). Pins `security.auth.selectedType=gemini-api-key`, `privacy.usageStatisticsEnabled=false`, `general.enableAutoUpdate{,Notification}=false`, `model.name=$GEMINI_MODEL`, `context.fileName=[GEMINI.md, CLAUDE.md]` (so the shared workspace `CLAUDE.md` loads, verified), the translated hooks and `mcpServers` (stdio, `httpUrl` for streamable HTTP, `url` for SSE; all `trust: true`). Golden: `cmd/gb/testdata/gemini_settings_crew.golden.json`. |
| `gemini-config` bead | Merged over the defaults, but through an **allowlist** (`model`, `context`, `ui`, `output`), not Codex's denylist: the Gemini settings surface is large and most of it runs a program or redirects requests (`tools.discoveryCommand`, `proxy`, `telemetry`, `hooks`, `mcpServers`, `security`). The three first-run gates cannot be unpinned from a bead. `gemini-instructions` (`gemini_md`) appends to `GEMINI.md`. |
| §5 E hook translation | `cmd/gb/setup_gemini_hooks.go`: Claude `PreToolUse/PostToolUse/Stop/UserPromptSubmit/PreCompact/SessionStart/SessionEnd/Notification` become `BeforeTool/AfterTool/AfterAgent/BeforeAgent/PreCompress/...`; the rest are dropped and logged. The command allowlist is the Codex one (renamed `isAllowedHookCommand`; `--format` now admits `gemini`). Timeouts are converted to **milliseconds** (Gemini's unit; Claude and Codex use seconds). A tool-name matcher on a tool event is refused rather than installed as match-all (Claude names its tools differently). |
| context hooks | `gb hook prime`, `load-state`, `check-mail` and the `drain-queue/check-*` scripts print plain text for the model. **Verified on 0.64.0: plain hook stdout never reaches the model; only JSON `hookSpecificOutput.additionalContext` does** (a BeforeAgent hook printing text was absent from the request, the JSON form present). They are skipped rather than installed to run for nothing. Advice reaches the model through `GEMINI.md`. Mail and nudges are the driver's job (see follow-ups). |
| `relayHarness` | Accepts `gemini` from `GARDENIO_HARNESS`; without it, sniffs Gemini-only event names (`BeforeTool`, `AfterAgent`, ...) and a `transcript_path` under `/.gemini/` (the shared names `SessionStart/SessionEnd/Notification` need the path). `buildRelayEvent` rewrites Gemini payloads to the canonical shape (`BeforeTool` -> `PreToolUse`, `AfterAgent` -> `Stop` with `prompt_response` as `last_assistant_message`) so consumers, subjects and the harness-tax metric see one vocabulary. |
| §6 stop gate | `gb hook stop-gate --format gemini` (default from `GARDENIO_HARNESS`): `{"decision":"block","reason":...}` on stdout, exit 0, the checkpoint prose folded into the reason. Gate policy (cooldown, backoff, `stopGateMaxBlocks`, yield marker, wrap-up) is shared and unchanged. |
| real-binary test | `cmd/gb/hook_stopgate_gemini_realbinary_test.go` runs the real CLI with the settings the product writes and the real `gb hook stop-gate` as the AfterAgent subprocess against a fake daemon. It asserts a **second model call** carrying the gate's reason (the turn was continued, not just a string printed), a control run where the gate allows (exactly one call), and that the relay publishes canonical `PreToolUse/PostToolUse` with `harness=gemini` from the real payloads. Skips unless `gemini` (or `GEMINI_BIN`) and `node` exist. |
| `internal/harness` | `GeminiCommand = "gemini --acp"`, `CoopAgent` -> `gemini`, `ModelEnv` -> `GEMINI_MODEL`, `WithModel` -> `--model`, `StateDir`/`GeminiHome` -> `$GEMINI_CLI_HOME/.gemini`, `IsDeniedEnv` strips `GEMINI_*`/`GOOGLE_*` outside `{GEMINI_MODEL, GEMINI_CLI_HOME, GEMINI_CLI_TRUST_WORKSPACE}` (so an inherited `GEMINI_API_KEY` or `GOOGLE_GEMINI_BASE_URL` cannot win over coop's credential push). |
| gb launch paths | `gb init`, `gb agent start` (local and k8s) and `prepareAgentPrompt` branch on the harness: `setupGeminiSettings`, `GEMINI.md` instead of `--append-system-prompt-file`, no Claude session-dir cleanup, no dev channel, `BOAT_MODEL_PINNED` re-assert goes to `GEMINI_MODEL`. |
| controller | `shapeHarnessEnv` sets `GARDENIO_HARNESS=gemini`, `GEMINI_MODEL` (from the resolved model) and `GEMINI_CLI_TRUST_WORKSPACE=true` and drops the Claude pins; the `~/.gemini` PVC state mount replaces `~/.claude` (`HarnessStateMount`); `lambda_microvm` and an explicit `--ultracode=on` are refused for gemini as for codex. `GEMINI_HARNESS_ENABLED` still gates the whole harness off until phase 3's live pass. |

**Not done, and why**

1. **The coop curl shim is not deleted.** §12 deferred it to this phase, but deleting Rust from a pod with no cargo build cache cannot be verified here, and the shim is inert in a non-root pod (§11.2: the system settings file it lives in is skipped), so it cannot double-gate alongside `gb setup gemini`. It also still feeds the PTY driver's Tier 1 pipe, which stays until ACP is proven live. Follow-up: strip the `AfterAgent` curl-gate half and keep the pipe write.
2. **Context injection hooks for Gemini.** `prime`, `check-mail` and `load-state` need a `--format gemini` that emits `additionalContext` JSON before they can run on `BeforeAgent`/`SessionStart`. Until then advice rides in `GEMINI.md` and mail arrives through the driver's input queue. Follow-up bead.
3. **ACP fires only `BeforeAgent`, `AfterAgent`, `PreCompress`, `SessionEnd`** (§11.4): under ACP the `SessionStart` liveness relay and the tool relay do not fire, so those events come from the driver. The stop gate is an `AfterAgent` hook and does fire.
4. Slack aliases, `deathtranscript`/`contextwindow`/session-usage for gemini and the viewer chip are phase 4 (gd-sXOmQvpFWR); see §15.

## 15. Amendment: phase 4 landed, Slack, observability, docs (gd-sXOmQvpFWR)

Operator guide: [`gardenio/docs/harness-gemini.md`](../../gardenio/docs/harness-gemini.md).

| ADR item | Implementation |
|---|---|
| §5 D Slack | The Claude-only `if` chain in `resolveHarnessModel` is now a per-harness table, `harnessModelProfiles` (id shape, example id, `Aliases`), shared by Codex and Gemini. A Claude id on Codex or Gemini, and a Codex id on Gemini or the reverse, is refused with a message naming the owning harness; the `--model <harness>` shorthand and the thread-resume path (`bot_agent_kill.go`) use the same `isHarnessShorthand`. **No Gemini short aliases ship**: the spike (R6) reads the ids from the binary but cannot say which a key may call or how the API treats an unknown one, so a test pins `Aliases` empty and exercises the lookup path with a stand-in alias. Adding aliases after the live pass is a one-line table edit. |
| viewer chip | `/agents` list rows (full page and the htmx row partial) chip every non-`claude` harness instead of hard-coding `codex`; the detail card already rendered any `resolved_harness`. |
| `contextwindow` | `AgentWindow` treats `gemini` like `codex`: Claude's window tables never apply, and the percentage is shown only when the agent bead carries `model_context_window`. Nothing writes that field yet (the ACP `usage_update.size` is the natural source). |
| session usage | `sessionUsageFromCoop` is true for `gemini`: usage and the close-time snapshot come from coop's `/api/v1/session/usage`, as for Codex. |
| `deathtranscript` | `IsEventStreamHarness` accepts `gemini`: death reports read coop's harness-neutral hook/state event logs. |
| metric label | Already `claude\|codex\|gemini\|unknown` from phase 1/2; verified, no change. |

**Not done, and why**

1. **Grafana panels.** Splitting panels by `harness` edits `gardenio/helm/**`, which is outside the crew lane. Filed as a helm-lane escalation on gd-sXOmQvpFWR.
2. **Writing `model_context_window` for Gemini and Codex.** Needs the driver to publish the reported window to the agent bead; follow-up bead, independent of the harness.
3. **UX acceptance on the Slack change** (§7 phase 4 gate): the change is error text only (no Block Kit layout), covered by unit tests; the Slack test workspace session (gd-ClHbsJtneO, gd-fWeCNSRdCd) is not available, so no screenshots were taken.

## Amendment 3 (2026-10-16): Google-account login

Non-goal C in §1 ("Google-account OAuth/subscription login") is reversed: operators need to run Gemini agents without per-token billing, and the contract turned out to be simpler than Codex's. The CLI refreshes `oauth_creds.json` itself and Google does not rotate its refresh token on use, so there is no keeper and no placeholder token: the controller mounts the Secret read-only, `gb setup gemini` installs a writable copy and pins `selectedType=oauth-personal`, and coop authenticates over ACP with `oauth-personal`. Selected per account by `credential=secret:<name>#oauth_creds.json`; operator guide in [harness-gemini.md](../../gardenio/docs/harness-gemini.md#google-account-login-subscription--free-tier-no-per-token-billing). The live behaviour is [LIVE], carried with the rest of §9.
