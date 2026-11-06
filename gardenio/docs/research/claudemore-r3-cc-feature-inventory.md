# claudemore R3 — First-Class Claude Code Feature Inventory

> **Bead:** ka-0000003637 (R3) · **Epic:** ka-0000006064 (claudemore research) · **Feeds:** ka-0000001264 (R5 synthesis / goal-ladder)
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Author:** gardenio-r3-firstclass-claude-d3c (crew) · **Date:** 2026-06-14
>
> **Method:** 5 parallel feature investigators (Workflow tool, hooks, slash/skills, MCP, subagents),
> each followed by an adversarial completeness reviewer that independently grepped the tree to
> refute or correct the classification, then a cross-feature synthesis. All file:line citations
> below were emitted by the investigators and spot-verified by the author
> (`symlinkClaudeExtensions`, `molecule_rollup.go` WorkflowSpend, absence of an `s3-resume` binary,
> coop `<session>/workflow/` S3 sync, teams env injection, MCP 13-scenario e2e).
>
> **Scope note:** "claudemore" is a real production LLM proxy that speaks Claude Code (per scope
> decision ka-0000007358); e2e is one use case, not the whole point. This inventory frames the
> per-feature gap in terms of "what a real model in the loop would let an e2e assert."

---

# Claude Code Feature Support Matrix — claudemore Epic (R3)

## 1. Support Matrix

| Feature | Classification | Current state (one line) | Gap to first-class (one line) |
|---|---|---|---|
| **Hooks** (SessionStart/PreToolUse/PostToolUse/Stop/…) | **have** | Two-layer hook plumbing (gb-materialized `settings.local.json` + coop FIFO/curl gates), real Go/shell impls, full relay→NATS→Slack, Stop-gate tested e2e | Only Stop is exercised end-to-end; injected `<system-reminder>` stdout is opaque to claudeless — no real model acts on prime/mail/block text |
| **MCP servers** | **partial** | `.mcp.json` materialization first-class + 13-scenario live e2e; gardenio-channel server fully plumbed but disabled in mock mode, off by default, httptest-only | No test launches a real Claude that connects to an MCP server and calls a tool; channel nudge/permission round-trip never exercised in a live pod |
| **Subagents / teams** | **partial** | Hook-event observability (SubagentStart/Stop, TaskCreated/Completed, TeammateIdle) + teams env enablement + `workflow_spend` cost attribution, all real | No real-run e2e drives an actual subagent/team; in-process subagents get cost rollup but no per-subagent bead; `SubagentStop` output dropped |
| **Slash commands + Skills** | **partial** | `symlinkClaudeExtensions` (project `.claude/{agents,skills,commands}`) + rwx@rwx plugin/marketplace; settings write path has e2e coverage | Symlink path untested; nothing proves a skill/command is discovered, executed, and its effect observed by a real agent; no `.claude/commands/` ships |
| **Workflow tool** (agent()/parallel()/pipeline(), resume via runId) | **partial** | coop S3 companion-dir sync (real, tested), `workflowhost` resume/lease logic (real, tested), cost rollup (live HTTP path) — all wired internally | Never triggered by any runtime caller; entrypoint has no rehydrate step; `s3-resume` binary does not exist; no real Workflow tool run anywhere |

**Classification adjustments:** None. All five verdicts left the investigator's classification unchanged (`have`, `partial`, `partial`, `partial`, `partial`). The one verdict flagged `accurate: false` (Subagents) still affirmed `classification_should_be: "partial"` — the inaccuracy was in the *current-state narrative* (it missed the `workflow_spend` attribution layer), not the rating. I have incorporated that correction below.

---

## 2. Per-Feature Detail

### Hooks — **have**

**Current state.** The most-exercised CC feature in the suite, with two coexisting layers:
- **Layer 1 (gb-materialized).** `defaultHookSettings()` emits a full map — SessionStart, PreCompact, Stop, UserPromptSubmit, PreToolUse, PostToolUse + ~20 relay-only events (`gardenio/controller/cmd/gb/setup_config.go:337-450`), with JIRA-conditional Bash hooks (`setup_config.go:459-509`). Real Go impls under `gb hook` (`hook.go:96-311`: prime/stop-gate with debounce/escape-hatch/exit-2 protocol); relay event→NATS mapping (`hook_relay.go:34-285`); materialized at boot via `gb setup claude` (`setup.go:30-169`, `--check` hard-requires SessionStart+Stop at `setup.go:169`).
- **Layer 2 (coop driver).** coop synthesizes its own SessionStart/Stop/Notification/PreToolUse hooks to a FIFO + curl gating endpoints (`coop/crates/cli/src/driver/claude/hooks.rs:8-70`; note the JSON key is `SessionStart`, not `Start`), merged into `--settings` (`setup.rs:209-256`).
- **Server side.** `kbeads/internal/hooks/` executes advice-hooks + gate verdict; relay→NATS `HOOK_EVENTS`→bridge `HookStream`→Slack card (`hookstream.go` + `hookstream_e2e_test.go`).

Tested: Stop-gate cooldown/escape-hatch/fail-open via claudeless TOMLs (`stop-gate-cooldown.toml`, `gate-decision-flow.toml`, `gate-no-identity.toml`), prime/check-mail exit-0 + injection format (`local-prime-fetch.toml`), API-level emit/execute/publish (`test-hooks-system.sh:198-310`).

*Verdict note:* `gardenio/images/agent/hooks/stop-gate.sh` is the **dormant legacy** script — the live Stop path is the Go `gb hook stop-gate` subcommand. Still COPY'd to `/hooks/` but superseded.

**Gap to first-class.** (1) Only Stop closes through the gate end-to-end; PreToolUse/PostToolUse/UserPromptSubmit/PreCompact/SessionStart-as-K8s-event marked **N** in the parity doc. (2) Defining limitation: claudeless treats injected `<system-reminder>` stdout as **opaque** (`claudeless-critical-path-parity.md:310`) — the *format* of prime injection is asserted, but "agent reads prime and acts on assignment" is not. (3) Full Stop loop (block exit 2 → agent runs `gb decision create` → human responds → next Stop exits 0) is split across TOMLs; no single scenario closes it because claudeless cannot *decide* to create a decision. (4) Layer-1 + Layer-2 merge/coexistence never asserted in one real session.

**First-class + tested.** Complete role-merged hooks map installed [DONE]; each hook a tested impl [DONE]; Stop gate enforces block/allow over HTTP with exit-0/2 protocol [DONE]; **hook stdout injecting `<system-reminder>` is consumed by a real model that changes behavior** [claudemore-only]; full Stop loop closes in one session; relay→NATS→Slack verified live.

**claudemore e2e assertions.**
- SessionStart in a pod with a claimed bead → hooks fire AND the model's first turn references the prime-injected assignment (proves consumption, not just emission).
- Stop full loop: drive toward stop with no open decision → `stop-gate` exits 2 → real model responds with `gb decision create` + `gb yield` → resolve → next Stop exits 0 → bead closes.
- PreToolUse/PostToolUse: each real tool call emits NATS events with correct `tool_name`/`tool_input`, rendered live on the agent card.
- PreCompact re-prime: force compaction → `prime --enqueue` → next PostToolUse `drain-queue.sh` re-injects → model re-grounds.
- Layer-1 + Layer-2 Stop hooks both fire from a valid merged `--settings`, no clobber.

---

### MCP servers — **partial**

**Current state.** Two concerns at different maturity:
- **(1) Config materialization — first-class + tested.** `gb setup claude` resolves `claude-mcp` config beads and writes merged `{workspace}/.mcp.json` with env `${VAR}` expansion + Playwright auto-fixes (`setup_config.go:50-263`); `claude-mcp` is a registered config namespace (`config.go:712`). Unit tests + **13-scenario live e2e** (`test-config-mcp.sh`, wired into shard 3 at `run-shard.sh:780`).
- **(2) gardenio-channel MCP server — plumbed, NOT exercised.** stdio MCP server with `claude/channel` capabilities, `reply` tool, token-authed HTTP listener on :8789 (`gardenio/images/agent/channel/gardenio-channel.ts`). Controller injects `GARDENIO_CHANNEL=1` (`podspec.go:763`), exposes `ChannelPort=8789` (`manager.go:281`), agent loop adds `--dangerously-load-development-channels` **only when `!mockMode`** (`init_agent_loop.go:471`). Bridge delivers nudges with coop PTY fallback (`nudge.go:95-132`) and relays permission verdicts (`bot_permission.go`). Defaults off (`values.yaml:2167`); httptest-only tests.

**Gap to first-class.** (a) The materialization e2e asserts only `.mcp.json` *shape* — never launches a real Claude that connects to a server and calls a tool. (b) The channel server is disabled in mock mode, off by default, and has no e2e booting a pod with `GARDENIO_CHANNEL=1` to prove a nudge lands in a live session or a permission verdict round-trips. claudeless replays only Bash + AskUserQuestion (`parity:~314`).

**First-class + tested.** Provisioning [HAVE]; secret/env injection [plumbed, file-level only]; **real claude connects to each configured server** [NOT tested]; **agent calls an MCP tool, gets a result** [NOT tested]; **gardenio-channel message delivery + permission round-trip against live server in a real pod** [NOT tested].

**claudemore e2e assertions.**
- Boot pod with a stub MCP server bead → after setup, `.mcp.json` has it AND `claude mcp list` / startup output shows it connected with tools registered.
- Prompt a real agent to call the stub tool → result appears in transcript (connect+invoke).
- Provision Playwright → agent navigates + snapshots successfully (auto-fixes correct against live server).
- `${VAR}` secret → running server receives the *resolved* secret (auth-gated call succeeds).
- Deploy `agents.gardenioChannel=true` → POST nudge → arrives as `<channel source="gardenio">` and agent reacts via `gardenio__reply`.
- Permission round-trip: tool prompt → channel relays `permission_request` to Slack → allow verdict → agent proceeds. Negative: no bearer → 401; no token → process fails closed. Channel down → coop PTY fallback still delivers.

---

### Subagents / teams — **partial**

**Current state.** *(Corrected per verdict — the investigator missed the attribution layer.)*
- **Model.** Subagents/teammates run **in-process** in the same pod (own CC Node session), NOT as separate gardenio pods (`podspec.go:676-678`); no controller spawns a pod or bead per subagent. This is distinct from the pod-level gardener→crew spawn path.
- **Hook-event observability (real).** SubagentStart/Stop, TaskCreated/Completed, TeammateIdle are default hooks (`setup_config.go:394-409`), parsed by relay reading `agent_id`/`agent_type` (`hook_relay.go:226-230` — *not* `subagent_id`), eventbus types (`types.go:23-24`), dispatched by hookstream → Slack `task_update` chunks (`agentstream.go:814-934`).
- **Teams enablement (real).** `CLAUDE_TEAMS_ENABLED`→`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` (`setup_config.go:312-325`); config + podspec inject mode/max + resource overrides (`config.go:188-200`, `podspec.go:672-698`). Settings injection tested at `test-config-user-settings.sh` Scenario 8/9 (on + unset).
- **Cost attribution (real, unit-tested — investigator missed this).** `beadsapi/molecule_rollup.go`: `WorkflowSpend` (token + `total_cost_usd` + `agents_spawned`), `ReportWorkflowSpend` writes `workflow_spend` onto the host step bead, `RollupMoleculeUsage` SUMS the in-session subagent pool onto the host across the molecule subtree. `molecule_rollup_test.go` round-trips it. The in-process subagent construct *is* gardenio's `agent()`/`parallel()`/`pipeline()` (see Workflow tool).
- **Session handling.** `retireAgentSessions` skips `subagents/*.jsonl` (`more_coverage_test.go:244-273`). coop has zero subagent/teams plumbing.

**Gap to first-class.** (1) No real-run e2e — all "e2e" feed synthetic JSON/NATS through Go handlers (`hookstream_e2e_test.go`); claudeless can't emit genuine events. (2) No per-subagent bead (cost *is* attributed via `workflow_spend`, but there is no operated_by/parent-child bead per teammate). (3) `teams-mode.toml` stops at settings materialization and is not wired into any runner. (4) `SubagentStop.last_assistant_message` is parsed (`hook_relay.go:230`) but **dropped at dispatch** — `HandleSubagentStop` hardcodes `''` output (`agentstream.go:828-840`), so subagent results never surface.

**First-class + tested.** Real pod spawns in-process subagent/team when teams enabled; real binary emits the hook events → relay → NATS → bridge → Slack/viewer chunks; pod correctly sized; teammate sessions survive retire/resume; optionally durable per-subagent attribution — all asserted by ≥1 e2e driving a **real** run.

**claudemore e2e assertions.**
- Real agent uses Agent/Task tool → SubagentStart reaches `hooks.<agent>.SubagentStart` with id+type → bridge emits in_progress chunk.
- SubagentStop → complete chunk AND `last_assistant_message` surfaced (currently dropped).
- Real team run → TaskCreated/Completed + TeammateIdle flow to Slack/viewer.
- Teammates run in same pod (single agent bead, no per-teammate pod) with teams resource overrides + env applied; `workflow_spend` rolls the subagent pool onto the host bead.
- Subagent jsonl survives session-retire + resumes across pod restart.

---

### Slash commands + Skills — **partial**

**Current state.**
- **Install/discovery (real, mostly untested).** `symlinkClaudeExtensions(workspace)` symlinks project `.claude/{agents,skills,commands}` into the agent workspace (`agent_k8s_workspace.go:123-165`), wired at `setup.go:379-380` — **zero test coverage**.
- **Plugin/marketplace.** `appendDetectedPlugins` enables `rwx@rwx`; `ensureRWXMarketplace` declares `rwx-cloud/skills` (`setup_config.go:267-310`). Unit tests on the settings map (`setup_test.go:290-345,396-442`) **plus** an e2e write-path check the investigator missed: `test-config-user-settings.sh:362-368` (Scenario 7b) runs the real `gb setup` binary, reads the materialized `~/.claude/settings.json`, and asserts `rwx@rwx` + `rwx-cloud/skills` present.
- **Shipped skills.** `.claude/skills/video/SKILL.md`, `kbeads/.claude/skills/ci-perf/SKILL.md`. **No `.claude/commands/` exists anywhere.** No image-bake install path — skills reach pods only via symlink or marketplace auto-install.
- **coop.** Parses the `Skill` tool name → activity label (`parse.rs:56`, unit-tested); does NOT recognize a `SlashCommand` tool. Controller has no awareness of Skill invocations.

**Gap to first-class.** (1) `symlinkClaudeExtensions` is wholly untested — nothing asserts symlinks resolve or claude then discovers the skill. (2) Plugin install is verified only as JSON keys + the gb write path; nothing proves Claude contacts the marketplace, installs, and exposes skills. (3) claudeless can't emit a `Skill` tool_use, so no e2e exercises invocation through the runtime. (4) No controller telemetry to assert on.

**First-class + tested.** (a) gardenio installs project-local (symlink) + platform (rwx@rwx) skills with tests that symlinks resolve and settings enable the plugin; AND (b) an e2e under claudemore drives a real agent to invoke a `/command` and/or Skill and asserts it was discovered, executed, and its effect observed.

**claudemore e2e assertions.**
- Project-shipped skill appears under workspace `.claude/skills` via symlink and claude lists it.
- Real agent invokes the Skill tool → coop emits a `Skill` activity label with the right name.
- Skill side effect observable (file/bead/stdout) — proves the body ran.
- `rwx@rwx` plugin installed at startup and ≥1 rwx skill invocable.
- A `.claude/commands/` slash command (none ship today) is discoverable + runnable.
- Unit/integration: `symlinkClaudeExtensions` creates the three symlinks, skips when source missing / dest exists.

---

### Workflow tool — **partial**

**Current state.** Substantial plumbing, never closed against a real run:
- **coop S3 companion-dir sync (real, ~10 unit tests).** `workflow_companion_dir` strips `.jsonl` (`s3.rs:918-928`); `upload_workflow_dir` pushes `agent-*.jsonl` + `runs/<id>.json` under `<session>/workflow/` (`s3.rs:958-1003`), wired into shutdown drain (`s3.rs:773`) + periodic (~858/874); `restore_workflow_dir` wired into resume (`run.rs:536-553`).
- **Re-entrancy logic (real, tested, UNWIRED).** `workflowhost.go:42-217` — `DecideResume`, `EvaluateLease` (15m TTL), persisted fields; 11 tests. Surfaced as `gb workflow persist|rehydrate|lease` (`workflow.go`, 8 tests).
- **Cost rollup (real, the one genuinely end-to-end-wired sliver).** `molecule_rollup.go` parses/sums `workflow_spend`; `RollupMoleculeUsage` is on the beads-viewer daemon interface (`server.go:205`), routed at `GET /molecule/{id}/cost` (`server.go:1038`), rendered with split + partial banner — a live, reachable HTTP path. This is what justifies `partial` over `missing`.
- **E2E (simulation only).** `test-dynworkflow-resume.sh` builds journals by hand with `printf` and seeds beads by curl; B6 live-pod check skips synthetic agents. No real claude, no real Workflow tool.

**Gap to first-class.** (1) Entrypoint has **no** rehydrate step (`entrypoint.sh` does `coop --resume` only). (2) The `s3-resume` binary **does not exist** anywhere, yet `gb workflow rehydrate` shells out to it (`workflow.go:48-53`). (3) No real LLM/Workflow tool ever runs — every test fabricates journals. (4) No claudeless workflow scenario. (5) Controller/reconciler/podmanager/statusreporter have **zero** workflow awareness; gardener/reaper don't honor `workflow_lease` (enforced only in the unwired CLI). (6) Most of the design (partial status, on_complete threading, exec_hint classifier, idempotency keys) is `[net-new]`.

**First-class + tested.** A pod driven by coop against real claudemore runs a script invoking the actual Workflow tool; coop persists journals to S3; agent persists `workflow_run_id`/`script_path`/`s3_uri` (runId-first); on pod kill, controller respawns, entrypoint runs lease-gated `gb workflow rehydrate`, restores journal, invokes `Workflow({scriptPath, resumeFromRunId})` so the completed prefix returns **cached** (zero re-spawn) and only the tail runs live; lease prevents double-dispatch and gardener/reaper honor it; host reports `workflow_spend`, viewer rolls it up. Asserted e2e against the real binary, not a hand-built fixture.

**claudemore e2e assertions.**
- Real Workflow tool returns a runId persisted to the step bead's `workflow_run_id` before the first `agent()` settles.
- After an `agent()` completes, the PVC companion dir holds a real `agent-1.jsonl` (not printf) and coop's S3 sync uploads it under `<session>/workflow/`.
- Kill mid-run (SIGKILL/137) → respawn → entrypoint `gb workflow rehydrate` acquires lease, `s3-resume pull` runs, emits mode=resume with the same runId.
- `Workflow({resumeFromRunId})` returns completed `agent()` from cache (byte-identical, zero new prefix spend); only the tail runs live (asserted via `completed_agents` + live spend).
- Two concurrent re-dispatch: second rehydrate REFUSED (non-zero) on a fresh lease; stale lease taken over. Gardener does not reclaim a fresh-lease step.
- Cold-restart before runId persisted → mode=cold-start, clean restart.

---

## 3. Prioritized First-Class Roadmap

Ordered by (value to claudemore × distance from first-class). claudemore's core unlock is *a real reasoning model in the loop*, so features whose entire gap is "no real model to act on the plumbing" rank highest when the plumbing is already solid.

| # | Feature | Rationale | Effort |
|---|---|---|---|
| 1 | **Hooks** | Highest value-per-effort: plumbing is `have` and the *only* remaining gap is exactly what claudemore unlocks — a real model consuming injected `<system-reminder>` text (prime/mail/block) and closing the Stop loop. Nearest to first-class; foundational for every other feature's hook events. | **S** |
| 2 | **Subagents / teams** | High value (teams is a marquee CC capability) and plumbing is mature (events + enablement + `workflow_spend` rollup). Gap is purely "drive a real subagent/team run" + the trivial `SubagentStop` output-drop fix (`agentstream.go:828-840`). Mostly e2e authoring. | **M** |
| 3 | **MCP servers** | High value (Playwright/channel are load-bearing). Config path is first-class; gap is connect+invoke against a live server, plus flipping the `!mockMode` channel gate and a `gardenioChannel=true` e2e variant. Requires a stub server + Helm/CI wiring. | **M** |
| 4 | **Slash commands + Skills** | Moderate value, but cheapest *unit*-test win (`symlinkClaudeExtensions` is trivially testable today, no claudemore needed). The claudemore half needs a canonical shipped command/skill target (none exist) + marketplace-reachability resolution from locked-down pods. | **M** |
| 5 | **Workflow tool** | Highest strategic value (deterministic multi-agent orchestration is the epic's namesake) but **furthest** from first-class: entrypoint unwired, `s3-resume` binary nonexistent, zero controller awareness, lease unenforced, much `[net-new]`. Even with claudemore, resume-replay is untestable unless the Workflow *runtime* itself emits journals + honors `resumeFromRunId`. | **L** |

---

## 4. Open Questions for R5 Synthesis (goal-ladder ka-0000001264)

**Cross-cutting — does claudemore actually unblock these?**
1. Does claudemore emit the **exact wire artifacts/schemas** the plumbing parses? Specifically: (a) the hook stdin JSON schema (`hook_event_name`, `tool_input`, `last_assistant_message`, `source`/`trigger`) that `gb hook relay`/`stop-gate` parse; (b) genuine `SubagentStart/Stop`/`TaskCreated/Completed`/`TeammateIdle` events in the documented schema (no in-repo real-run artifact confirms the wire format); (c) the Workflow tool's `agent-*.jsonl` + `runs/<id>.json` journals and `resumeFromRunId` cache replay. If claudemore can't produce these, the corresponding e2e assertions are unwritable regardless of model quality.

**Per-feature blockers R5 must resolve:**
2. **Workflow** is the largest dependency knot: must the **entrypoint gain a rehydrate step**, and must the **`s3-resume` binary be built** — or does coop's `restore_workflow_dir` (keyed off `coop --resume`) fully subsume `gb workflow rehydrate`'s `journalRestore`? The two restore paths are currently disjoint. And will any controller/reconciler component become **workflow-lease-aware** at reclaim time, or does the lease stay trapped in the unwired CLI?
3. **MCP / channel** policy decisions: will `agents.gardenioChannel` ever default true (or run a `true` variant in e2e/smoke)? Is the `!mockMode` gate (`init_agent_loop.go:471`) a permanent design choice or a stopgap claudemore should remove so channel delivery is testable? Is the rwx marketplace fetch **reachable from locked-down pods**, or does network policy make plugin skills effectively unavailable despite the settings?
4. **Skills/commands** need a stable target: should gardenio ship a canonical agent-facing `/command` or skill (none in `.claude/commands` today) so e2e has something deterministic to assert against? Does claudemore need to emit a distinct `SlashCommand` tool_use, or are slash commands always surfaced as the `Skill` tool (coop only handles `Skill`)?
5. **Subagents** scope: is **durable per-subagent beads attribution** in scope, or is the existing `workflow_spend` cost rollup + transient Slack chunks considered sufficient? Is teams still experimental (`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS`) such that parity testing is premature until the flag stabilizes? Which `CLAUDE_TEAMMATE_MODE` (in-process vs tmux) is the prod default, and does coop need tmux-pane awareness for viewer rendering?
6. **Determinism for e2e**: can claudemore be driven to *deterministically* invoke a named MCP tool / Skill / Workflow script, or will assertions rely on natural-language prompting that may flake? This shapes whether the claudemore e2e suite is reliable enough to gate prod.

**Recommended ladder framing for R5:** treat **Hooks** as the proving ground (smallest gap, exercises the relay→NATS→Slack spine every other feature reuses), then **Subagents** and **MCP** as parallel mid-tier rungs, **Skills** as a cheap independent win, and **Workflow** as the capstone whose first-class status depends on resolving Q1c and Q2 first.