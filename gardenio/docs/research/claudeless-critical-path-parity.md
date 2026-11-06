# Claudeless critical-path parity — research report

**Bead:** ka-0000003888 (research phase of epic ka-0000000619).
**Author:** `gardenio-research-claudeless-parity-04t`.
**Operator:** whoever spawned `gardenio-new-epic-to-refresh-claudeless-961f`.

This is Phase 1 of the refresh-claudeless epic. The goal is to enumerate the
gardenio agent critical path, map it against the 42 claudeless TOML scenarios
that ship today, identify the gaps, classify the upstream-claudeless
limitations, and produce a prioritised scenario backlog that a follow-up
crew can pick up row-by-row.

This report is **research only** — no TOML scenarios are authored, no
runner changes proposed, no upstream PRs drafted.

------

## 0. Definitions and scope

- **Claudeless** — github.com/alfredjeanlab/claudeless v0.4.0, installed at
  `/usr/local/bin/claudeless` in `<registry>/gardenio/agent:latest`.
  It is the `claude` binary plus four flags:
  `--scenario <file>`, `--capture <file>`, `--claude-version <v>`, `--failure <mode>`
  (where mode ∈ `network-unreachable|connection-timeout|auth-error|rate-limit|out-of-credits|partial-response|malformed-json`).
- **Scenario** — a TOML file under `gardenio/tests/e2e/claudeless/` whose
  shape is `[claude]`, one or more `[[responses]]` blocks (matched in order
  by `on = "*"` or `on = { contains = "…" }`), an optional `[default]`,
  and a `[tools]` block. Tool calls are dispatched by `tools = [{ call,
  input = { command = "…" } }]` — overwhelmingly `Bash` today.
- **Critical path** — the `gb`/`kd` subcommands and runtime hooks an
  agent invokes during a normal lifecycle (boot → claim → work → squawk
  → decision/escalation → bundle → MR → stop), as documented in
  `.gb/system-prompt.md` and the role nudges materialised by
  `gardenio/controller/internal/bridge/init_prompts.go`.
- **End-to-end assertion** in the coverage matrix means the scenario
  drives the same code path a production agent does — either by calling
  the binary against a live daemon (`BEADS_HTTP_URL` set) or by exercising
  the local Bash/filesystem hook code paths. It does *not* mean a Slack
  message round-trips or a real human reads anything.

------

## 1. Critical-path inventory

Sourced from `init_prompts.go` (`config:nudge-prompts:global` + per-role
overrides), the global `.gb/system-prompt.md`, `gardenio/docs/local-agent-runbook.md`,
the `images/agent/hooks/*` scripts, and `gb` / `kd` top-level help. Each
row links the implementation site and at least one place the agent is
told to call it.

### 1.1 BOOT — pod start, session init

| Surface | Impl site | Used in (role nudge / hook / sys prompt) |
|---|---|---|
| `gb init` (entrypoint) | `gardenio/images/agent/entrypoint.sh:39` → `gb init` (`gardenio/controller/cmd/gb/init.go`) | Container entrypoint; runs setup + restart-loop. |
| SessionStart → `prime.sh` → `gb prime` | `gardenio/images/agent/hooks/prime.sh`, `gardenio/controller/cmd/gb/prime.go` | `init_prompts.go::session_resumption`; system prompt §"Context Recovery". |
| SessionStart → `check-mail.sh` → `kd list --type mail` | `gardenio/images/agent/hooks/check-mail.sh` | Always-on SessionStart hook; relied on by jira-mention and crew nudges. |
| `gb setup claude` | `gardenio/controller/cmd/gb/setup.go` | Materialises `.claude/settings.local.json` + hooks for local mode (`local-agent-runbook.md`). |
| `gb nudge-prompt` | `gardenio/controller/cmd/gb/nudge_prompt.go`, `init_prompts.go::configBeadEntries` | Resolves which role/global nudge to inject as the first user prompt. |
| `coop --resume` | `coop/crates/cli/` | Entrypoint resume on pod restart (system prompt §"Conversation resume"). |
| `gb prime --for <agent>` | `gardenio/controller/cmd/gb/prime_progress.go::outputProgressBlock` | Gardener progress surface (T9 plan, `gardener-progress-prime` scenario). |

### 1.2 CLAIM — finding work

| Surface | Impl site | Used in |
|---|---|---|
| `gb ready` | `gardenio/controller/cmd/gb/ready.go` | Global default nudge: "Check `gb ready` for your workflow steps". |
| `gb news` (`--active-only`, `--stale-only`, `--ids-only`) | `gardenio/controller/cmd/gb/news.go` | Gardener workflows (Stale-vs-Live Triage), crew startup guard. |
| `kd list` (`--status`, `--type`, `--assignee`, `--json`, `-f` filters) | `kbeads/cmd/kd/list.go` | Gardener command palette; crew probe-for-orphans. |
| `kd show <id>` (`--json`) | `kbeads/cmd/kd/show.go` | Every assignment intake. |
| `kd claim <id>` | `kbeads/cmd/kd/claim.go` | Crew "Starting work" workflow. |
| `kd unclaim` / `kd release` | `kbeads/cmd/kd/{unclaim,release}.go` | Stale-vs-live rescue path (gardener advice ka-0000007726). |

### 1.3 WORK — doing the job

| Surface | Impl site | Used in |
|---|---|---|
| `kd create` (`--type`, `-l`, `-d`, `--priority`) | `kbeads/cmd/kd/create.go` | Common Workflows §"Creating & Updating". |
| `kd update` (`--status`, `--assignee`, custom fields) | `kbeads/cmd/kd/update.go` | Gardener rescue of stale beads; status drift fixes. |
| `kd dep add … --type parent-child|blocks` | `kbeads/cmd/kd/dep.go` | "Breaking down multi-step work" workflow. |
| `kd close <id>` | `kbeads/cmd/kd/close.go` | Session close checklist. |
| `kd comment add <id>` | `kbeads/cmd/kd/comment.go` | Decisions docstring §"Follow-up note tied to a bead". |
| `kd label` / `kd mention` / `kd refs` | `kbeads/cmd/kd/{label,refs}.go` | Typed-mention advice (ka-0000007977). |
| `gb workspace setup|teardown|list|audit` | `gardenio/controller/cmd/gb/workspace.go` | Per-bead worktree isolation (core rules). |
| Bash/Edit/Write/Read (Claude Code built-ins) | n/a | Implicit; every scenario uses them. |
| `git add/commit/push` | shell | Session close checklist steps 2–4. |

### 1.4 COMMUNICATE — squawk, mail, Slack, escalations

| Surface | Impl site | Used in |
|---|---|---|
| `gb squawk` | `gardenio/controller/cmd/gb/squawk.go` | "Just reporting progress / a finding" — decisions table. |
| `gb slack reply` / `gb slack thread` | `gardenio/controller/cmd/gb/slack.go` | Thread role nudge; advice ka-0000004818. |
| `gb mail send` / `gb inbox` | `gardenio/controller/cmd/gb/mail.go` | Gardener monitoring; check-mail hook. |
| `gb dm` | `gardenio/controller/cmd/gb/dm.go` | Identity-resolved DMs. |
| `gb escalation create|approve|reject|show|close|stats` | `gardenio/controller/cmd/gb/escalation.go` | Advice ka-0000001089 (privileged operator ask). |
| `kd comment` | `kbeads/cmd/kd/comment.go` | Bead-attached notes. |
| `gb bus emit` | `gardenio/controller/cmd/gb/bus.go` | Hook protocol (`Stop`, `PreToolUse`, etc.). |

### 1.5 DECISION / YIELD / GATE

| Surface | Impl site | Used in |
|---|---|---|
| `gb decision create` (`--prompt`, `--options`, `--no-wait`, `--context`, `--priority`) | `gardenio/controller/cmd/gb/decision.go` | Decisions section (every nudge). |
| `gb decision list` / `show` / `cancel` / `reflag` / `report` | `gardenio/controller/cmd/gb/decision.go` | Operator + agent inspection. |
| `gb yield` | `gardenio/controller/cmd/gb/yield.go` | Blocks until decision resolved or mail arrives. |
| `gb gate status` / `mark` / `clear` | `gardenio/controller/cmd/gb/gate.go` | "Project Health" command; gardener status checks. |
| Stop hook (`gb hook stop-gate`, `stop-gate.sh`) | `gardenio/controller/cmd/gb/hook.go`, `gardenio/images/agent/hooks/stop-gate.sh` | Auto-fires at session end. |

### 1.6 BUNDLE — long-form artifacts

| Surface | Impl site | Used in |
|---|---|---|
| `gb bundle create|add|order|show` | `gardenio/controller/cmd/gb/bundle.go` | Advice ka-0000004517 (long-form belongs in bundles). |
| `gb bundle add --doc <path>` | same | The very deliverable shape of this report. |

### 1.7 MR / CI / external integrations

| Surface | Impl site | Used in |
|---|---|---|
| `gb mr` | `gardenio/controller/cmd/gb/mr.go` | Gardener MR pipeline workflow. |
| `gb pipeline retrigger` | `gardenio/controller/cmd/gb/pipeline.go` | Global advice — never close/reopen MR for CI. |
| `gb ci` | `gardenio/controller/cmd/gb/ci.go` | Gardener triage. |
| `gb jira` / `kd attachments` | `gardenio/controller/cmd/gb/jira.go` | Jira-mention role. |
| `gb pipeline` polling (gh/glab pass-through) | same | RWX tester gardener. |

### 1.8 SCHEDULED GARDENERS

| Surface | Impl site | Used in |
|---|---|---|
| `gb gardener start|stop|status` | `gardenio/controller/cmd/gb/gardener.go` | Gardener role nudge §Lifecycle. |
| `gb schedule …` | `gardenio/controller/cmd/gb/schedule.go` | Scheduled gardener primitive (`gardener-loop-runbook.md`). |
| Trigger / stop / progress evaluators | `gardenio/controller/internal/scheduler/` | Reconcile pass; live-flow scenarios. |
| `gb crew` / `gb spawn` / `gb start` | `gardenio/controller/cmd/gb/{crew,spawn,start}.go` | Gardener role nudge §Commands. |
| `gb pool` | `gardenio/controller/cmd/gb/pool.go` | Prewarmed-agent path. |

### 1.9 STOP

| Surface | Impl site | Used in |
|---|---|---|
| `gb stop` (`--wrapup`) | `gardenio/controller/cmd/gb/stop.go` | Session close checklist step 5. |
| Stop hook protocol (exit 0/2) | `gardenio/images/agent/hooks/stop-gate.sh` + `gb hook stop-gate` | Auto-invoked by Claude Code. |
| `kd close` | `kbeads/cmd/kd/close.go` | Step 1 of "Completing work" workflow. |

------

## 2. Coverage matrix

Y = a current claudeless scenario asserts the surface end-to-end (file
called out). N = gap. Mixed/partial calls are explicit. Surfaces are
ordered by lifecycle phase from §1.

### 2.1 BOOT

| Surface | Asserted? | Scenario / note |
|---|---|---|
| Entrypoint `gb init` | **N** | No scenario boots a real `gb init` — claudeless is invoked directly. |
| SessionStart → prime | **Y (partial)** | `local-prime-fetch.toml` asserts `gb hook prime` exits 0 + injection-format. K8s SessionStart not exercised. |
| SessionStart → check-mail | **Y (partial)** | `mail-nudge-delivery.toml` (no-identity guard, drain-queue round-trip). Identity-set positive path **N**. |
| `gb setup claude` | **N** | No scenario re-runs setup; relies on pre-existing hook config. |
| `gb nudge-prompt` | **N (indirect)** | Local-mode scenarios consume sidecar startup prompts but no scenario asserts the resolution path itself. |
| `coop --resume` after restart | **N** | Resume path is entrypoint-managed; no claudeless coverage. |
| `gb prime --for <agent>` | **Y** | `gardener-progress-prime.toml` asserts "## Loop Progress" block appears for a gardener-linked agent. |

### 2.2 CLAIM

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb ready` | **Y (smoke)** | `agent-crew-lifecycle.toml` runs it; output is not asserted, only exit 0. |
| `gb news` (any flag) | **N** | No scenario asserts active-only/stale-only/ids-only or live-vs-stale split — primary tool of every gardener cycle. |
| `kd list` | **Y (smoke)** | Every gardener scenario lists schedule beads; specifically `gardener-conditions-fan-in.toml` etc. |
| `kd show` | **Y (smoke)** | All gardener scenarios. |
| `kd claim` | **N (smoke only)** | `agent-crew-lifecycle.toml` simulates claim with `echo`; never invokes real `kd claim` against a real bead. `local-lifecycle.toml` claims via the runner, not from inside the scenario. |
| `kd unclaim` / stale-bead recovery | **N** | No scenario reproduces the "agent died holding the claim" recovery path. |

### 2.3 WORK

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `kd create` | **Y** | All gardener-trigger-* + `gardener-conditions-fan-in.toml` create epics + child beads. |
| `kd update` | **Y (partial)** | `gardener-stop-hard-stop-consecutive-failures.toml` patches `consecutive_failures` on a schedule. Status/assignee mutations **not** asserted in isolation. |
| `kd dep add` | **N** | Used in setup of trigger live-flow scenarios but never asserted as an independent surface (no scenario verifies parent-child / blocks edge semantics). |
| `kd close` | **Y** | Every gardener live-flow scenario; `agent-crew-lifecycle.toml` (smoke). |
| `kd comment` | **N** | Surface is documented and used by humans, but no claudeless scenario exercises it. |
| `kd label` / `kd refs` / typed mentions | **N** | Beads-viewer ingests typed mentions but no scenario asserts the parser. |
| `gb workspace` | **N** | Worktree isolation has zero scenario coverage. |
| git add/commit/push | **N** (and not a candidate — needs a real repo state). |

### 2.4 COMMUNICATE

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb squawk` | **N** | Most-used outbound channel in the system; not asserted. |
| `gb slack reply` / `gb slack thread` | **N** | Thread role depends on these. No scenario; needs a Slack stub. |
| `gb mail send` | **N** | Gardener command palette uses it; not asserted. |
| `gb inbox` / `kd list --type mail` | **Y (partial)** | `mail-nudge-delivery.toml` drains the queue; `gb inbox` itself **N**. |
| `gb dm` | **N** | Identity-resolved DMs uncovered. |
| `gb escalation create/approve/reject` | **Y (round-trip)** | `local-escalation-roundtrip.toml` covers the SSE → coop nudge → agent reaction path. Cluster-mode `gb escalation` not exercised. |
| `gb bus emit --hook=…` | **Y (Stop)** | `stop-gate-cooldown.toml` + `gate-decision-flow.toml` + `gate-no-identity.toml`. Other hook events (`PreToolUse`, `PostToolUse`, `UserPromptSubmit`) **N**. |

### 2.5 DECISION / YIELD / GATE

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb decision create` shape (`id/short/label/artifact_type`) | **Y (smoke)** | `gate-decision-flow.toml` calls a fake echo; `local-yield-close.toml` calls the real CLI. Server-side rejection of single-`ack` / zero-option **N**. |
| `gb decision report` (artifact required path) | **N** | Critical-but-rare surface; an artifact_type=plan flow that returns ARTIFACT_REQUIRED is unasserted. |
| `gb decision list / show / cancel / reflag` | **N** | All inspection/mutation paths uncovered. |
| `gb yield` end-to-end | **Y** | `local-yield-close.toml` exercises the full create → external resolve → yield-returns → close flow. |
| `gb gate status / mark / clear` | **N** | No scenario inspects gate state transitions through the public CLI. |
| Stop hook cooldown / escape hatch / yield marker | **Y** | `stop-gate-cooldown.toml` covers all five filesystem paths. |
| No-identity fail-open | **Y** | `gate-no-identity.toml`. |
| Decision-gate block + satisfy + allow | **Y (smoke)** | `gate-decision-flow.toml` (fake decision id). Real-decision satisfy path **N**. |

### 2.6 BUNDLE

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb bundle create / add / order / show` | **N** | Zero coverage. The system advises every long-form artifact go through bundles; the CLI is unasserted. |

### 2.7 MR / CI

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb mr` / `gb pipeline` / `gb ci` | **N** | All real-GitLab surfaces; out of scope for claudeless without a GitLab stub (see §3). |

### 2.8 SCHEDULED GARDENERS

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb gardener start` idempotency | **Y** | `gardener-gardener-start-idempotent.toml`. |
| `@gardenio <epic>` mention → schedule | **Y (proxy)** | `gardener-mention-entrypoint.toml` asserts the schedule shape; the Slack→bridge half is not in scope. |
| Triggers `cron + bead_event + decision_resolved + manual` fan-in shape | **Y** | `gardener-conditions-fan-in.toml`. |
| Triggers live-fire (bead_event closed parent / created / subtree / label-predicate) | **Y (quarantined)** | `gardener-trigger-bead-{closed,created,event-*}.toml` exist but are **skipped** by `test-claudeless.sh` (SKIP_SCENARIOS list, ka-0000005245 — too slow). |
| Decision-resolved Match | **Y (quarantined)** | `gardener-trigger-decision-resolved-match.toml` (skipped). |
| Throttle / debounce / fan-in-cron-precedence | **Y (quarantined)** | `gardener-trigger-{throttle-burst,debounce-coalesce,fan-in-cron-precedence}.toml` (all skipped). |
| Stop blocks: `no_progress_for` shape | **Y** | `gardener-stop-no-progress.toml`. |
| Stop blocks: `wall_clock` default + override | **Y** | `gardener-stop-wall-clock.toml`. |
| Stop blocks: `bead_query` live-fire | **Y** | `gardener-stop-loop-end-bead-query.toml`. |
| Stop blocks: `hard_stop.consecutive_failures` | **Y** | `gardener-stop-hard-stop-consecutive-failures.toml`. |
| Progress block `in_prime=true` rendered | **Y** | `gardener-progress-prime.toml`. |
| `gb crew` / `gb spawn` / `gb start` | **N** | Gardener command palette; not asserted. |
| `gb pool` (prewarmed) | **N** | Pool nudge prompt referenced in init_prompts.go is uncovered. |
| Trigger-source engine (`gb trigger`, cron schedule, restart dedup, disable, condition filter) | **Y** | `trigger-source-{schedule-fires,schedule-conditions,restart-dedup,disable-stop}.toml`. |

### 2.9 STOP

| Surface | Asserted? | Scenario / note |
|---|---|---|
| `gb stop` polite-despawn | **Y (smoke)** | `local-lifecycle.toml`, `local-yield-close.toml` etc. close beads + call `gb stop`. |
| `gb stop --wrapup` payload | **N** | The structured-wrapup contract (accomplishments/blockers/handoff_notes) is not asserted. |
| Stop hook block (exit 2) → decision injection → satisfy → exit 0 | **Y (filesystem half)** | `stop-gate-cooldown.toml`. Full-cycle (block → CLI decision create → satisfy → stop=0) not asserted in one TOML. |
| Job-mode bypass (`BOAT_MODE=job`) | **Y** | `stop-gate-cooldown.toml` Test 2. |

### 2.10 Gap summary

Counts (rough; a "Y (partial)" / "Y (smoke)" counts as half a Y):

- BOOT: 7 surfaces, ~2.5 covered → **~36%**
- CLAIM: 6 surfaces, ~2.5 covered → **~42%**
- WORK: 8 surfaces, ~2.5 covered → **~31%**
- COMMUNICATE: 7 surfaces, ~2 covered → **~29%**
- DECISION/YIELD/GATE: 9 surfaces, ~4.5 covered → **~50%**
- BUNDLE: 1 surface, 0 covered → **0%**
- MR/CI: 3 surfaces, 0 covered → **0%** (mostly out-of-scope)
- GARDENERS: 14 surfaces, ~11 covered → **~79%** (but 6 are quarantined for runtime)
- STOP: 4 surfaces, ~2.5 covered → **~63%**

Overall: the gardener side is dense; the **crew/communicate/bundle** side
is sparse. The single biggest unasserted surface is `gb squawk` — the
most-used outbound channel in the system — followed by `gb bundle`,
`gb decision` server-side validation, and `gb workspace`.

------

## 3. Upstream-claudeless gap list

Surfaces the gardenio critical path depends on that claudeless v0.4.0
cannot simulate today. Each is either (a) workable around in scenario
authoring, (b) an upstream PR candidate, or (c) something to drop.

### 3.1 Real model behaviours — DROP (not claudeless candidates)

These surfaces need a real Claude session and are correctly out of scope
for the refresh:

- **Prompt interpretation** — whether the agent reads `gb ready` output
  and picks the right bead. Belongs in real-Claude E2E (kept separate by
  this epic's anti-scope).
- **Tool-use shape regressions** in Anthropic models (e.g. partial
  tool-use streaming format changes). claudeless does ship a `--failure
  partial-response` mode but it returns canned partials, not real
  streaming behaviour.
- **Plan-mode transitions** — `EnterPlanMode`/`ExitPlanMode` are
  Anthropic tool-use shapes; system prompt explicitly prohibits agents
  from using them. No claudeless surface, no need.
- **AskUserQuestion** behaviour with real users — `local-coop-states.toml`
  uses the tool shape to drive coop, but actual UX semantics need a real
  model + human.
- **Compaction / context-window trimming** — happens inside Claude,
  invisible to claudeless. The post-compaction `gb prime` re-injection
  is independently testable (see §4 backlog item P4).

### 3.2 Session-lifecycle gaps — WORKAROUND or upstream

| Gap | Why it matters | Recommended treatment |
|---|---|---|
| **Mid-session credential refresh** | Coopmux rotates API-key profiles in production; an agent in flight gets handed a new account_name. No scenario exercises the rotation. | Workaround: a runner script can flip a coopmux credential and assert the agent's next prompt still completes via the `[tools] mode = "live"` channel. Does **not** need a claudeless change. |
| **Session resume after `coop --resume`** | Entrypoint resumes conversation transcripts on pod restart. claudeless has no transcript model. | DROP for claudeless. Cover with a runner-level test that restarts the host process and asserts the agent bead lifecycle survives. |
| **`--include-partial-messages` streaming** | Real Claude emits incremental tool-use; the bridge's NATS push depends on it. | DROP — needs a real model. |
| **Hook-injected `<system-reminder>` text re-entering claudeless** | The drain-queue and stop-gate hooks inject system-reminders that real Claude parses and acts on. claudeless treats them as opaque text on its next turn — the *parsing* side is what real Claude does. | WORKAROUND: assert the inject-queue *content* (we already do in `mail-nudge-delivery.toml`); the model-side handling lives in real-Claude E2E. |

### 3.3 Tool-call surfaces — UPSTREAM candidate

| Gap | Why it matters | Recommended treatment |
|---|---|---|
| **MCP tool calls beyond Bash** | The agent image ships Playwright, Mezmo, and other MCP servers. claudeless `[tools]` only supports `Bash` + `AskUserQuestion` in current scenarios. | Confirmed by inspection of all 42 scenarios — only those two tool kinds appear. **Upstream PR candidate**: add a generic `[tools.<name>]` block that pre-records a tool's reply blob (no real MCP needed). Until then, **WORKAROUND** by funnelling every assertion through Bash. |
| **Edit / Write / Read tool calls** | Production agents edit files. claudeless can be told to "call Edit" but the canned response is text-only. | WORKAROUND: drive file-edit assertions through Bash (`sed`, `cat <<EOF`). This is what every scenario does today and is acceptable for lifecycle parity. |
| **Failure injection for tool calls** | `--failure auth-error` only injects on the API turn, not on a specific tool. | Defer — surface is small. |

### 3.4 Bridge / Slack surfaces — STUB

| Gap | Why it matters | Recommended treatment |
|---|---|---|
| **Real Slack thread delivery** | `gb squawk` and `gb slack reply` round-trip through the bridge. | WORKAROUND: assert the bead/event the bridge consumes (`type=message`, `decisions.*.DecisionResponded`, etc.) rather than the Slack call itself. The bridge's webhook responses are unit-tested separately. |
| **JIRA bridge** | Jira-mention role nudge is rare; coverage is low-value vs. effort. | Defer — out of refresh scope. |
| **GitLab MR / pipeline** | `gb mr` / `gb pipeline` round-trip GitLab. | Defer to a future epic with a glab stub. Out of refresh scope. |

### 3.5 Quarantined live-flow scenarios — TIMING

`test-claudeless.sh` SKIP_SCENARIOS (ka-0000005245) lists nine
gardener-trigger-* scenarios that each wait through one scheduler
reconcile tick (~60s) plus assertion polling, totalling 150–240s each.
On shard 0 they pushed past the 15-min RWX timeout. They run locally but
are excluded from CI today.

This is **not** an upstream-claudeless gap — claudeless executes the
scenarios fine. The blocker is the cluster scheduler's default
`COOP_SYNC_INTERVAL=60s`. Options:

1. Add a `COOP_SYNC_INTERVAL=5s` (or similar) override in CI test mode.
   Tracked in ka-0000005245's parent epic.
2. Move quarantined live-flow scenarios to a dedicated sub-shard with a
   30-min timeout.

Either path unblocks ~6 gardener surfaces' coverage that already exists
and would let us count them in §2.10.

### 3.6 Claudeless internals — runtime + scenario shape

- **`[[responses]]` ordering** — claudeless matches `on = "*"` and
  `on = { contains = … }` in order, one match per prompt. Multi-turn
  scenarios stack responses; the `max = 1` knob (`local-coop-states.toml`)
  prevents a single response from consuming all prompts. Good enough for
  lifecycle pinning.
- **Tool input shaped as a multi-line Bash command** — every assertion
  scenario today inlines a Bash block. Readable but fragile; a future
  iteration could factor scenarios into helper scripts.
- **Sidecar startup prompts** — `local-*.startup-prompt.txt` files exist
  because claudeless rejects unknown TOML keys (e.g. `[test_scenario]`).
  Not a blocker; just an artefact of the strict TOML schema.

------

## 4. Prioritised scenario backlog

Twelve scenarios, ordered by impact × tractability. Each row gives the
surface tested, the expected pass/fail signal, dependencies, and rough
complexity (S/M/L). A follow-up crew should be able to pick any row and
author the scenario without redoing this analysis.

| # | Title | Surface | Pass/fail signal | Deps | Complexity |
|---|---|---|---|---|---|
| **P1** | `squawk-roundtrip.toml` | `gb squawk '<msg>'` posts a `type=message` bead with the correct fields (`from:<agent>`, body, project label). | Create message via `gb squawk`, list `kd list --type=message -l from:<agent>`, assert exactly one row with matching body. | Live daemon (`BEADS_HTTP_URL`). | **S** |
| **P2** | `decision-create-rejects-acks.toml` | Server-side decision-create rejects zero-option and single-`ack/done/continue/acknowledge/wrap/noop` payloads (per `init_prompts.go` decisions text). | `gb decision create --options='[]'` → exit ≠ 0 + error text "no options"; `gb decision create --options='[{"id":"ack",…}]'` → exit ≠ 0. | Live daemon. | **S** |
| **P3** | `decision-report-artifact-required.toml` | Decision flow with `artifact_type=plan` requires `gb decision report` before resolving; `gb yield` returns `ARTIFACT_REQUIRED` until the report is filed. | Runner-side: create decision, resolve choice, observe yield returns ARTIFACT_REQUIRED; agent calls `gb decision report --content '…'`; subsequent yield returns the resolved decision. | Live daemon; uses the same external-resolve pattern as `local-yield-close.toml`. | **M** |
| **P4** | `bundle-create-add-show.toml` | `gb bundle create` + `gb bundle add --doc <path>` produces a bundle bead with the doc beads as parented children in order; `gb bundle show` lists them. | Create bundle parented to a fresh task; add two docs; assert two `type=doc` children in declared order. | Live daemon; filesystem write. | **S** |
| **P5** | `news-active-vs-stale.toml` | `gb news --active-only`/`--stale-only`/`--ids-only` partition in-progress beads correctly (gardener stale-vs-live triage rule). | Seed two in-progress beads: one held by a live agent (recently-pinged), one by a despawned agent. Assert `--active-only` returns the first, `--stale-only` returns the second, and `--ids-only` emits one ID per line. | Live daemon; needs ability to backdate `last_pinged_at` or equivalent on the stale agent bead — falls back to setting status=in_progress without an agent registration if the field is unavailable. | **M** |
| **P6** | `claim-and-release-roundtrip.toml` | `kd claim <id>` atomically marks `in_progress` + assignee; `kd unclaim` releases. Race: two parallel `kd claim` calls — exactly one wins. | Create task; first claim succeeds, second claim returns error; unclaim returns it to the queue; `gb ready` includes it again. | Live daemon. | **S** |
| **P7** | `mail-send-and-inbox.toml` | `gb mail send` creates a `type=mail` bead assigned to the receiver; `gb inbox` shows it; `check-mail.sh` formats it into the inject queue. | Send mail to a fake receiver name; `gb inbox` shows count=1; set `BEADS_AGENT_NAME=<receiver>` and run `check-mail.sh`; assert `/tmp/inject-queue.jsonl` contains the expected `<system-reminder>` payload. | Live daemon; piggybacks on `mail-nudge-delivery.toml` machinery. | **M** |
| **P8** | `workspace-setup-teardown.toml` | `gb workspace setup <bead>` creates a worktree at the expected path; `gb workspace list` shows it; `gb workspace teardown <bead>` removes it; `gb workspace audit` reports clean. | Run setup; verify `git worktree list` includes it; teardown; verify it's gone. | Local repo state (claudeless agent has a checkout). | **S** |
| **P9** | `escalation-cluster-mode.toml` | `gb escalation create` (no `--watch-escalations`) round-trip: create → approve via runner → `gb escalation show` reflects approval; `gb escalation stats` increments. | Companion to `local-escalation-roundtrip.toml`, but exercises the non-local mode. | Live daemon. | **M** |
| **P10** | `stop-wrapup-payload.toml` | `gb stop --wrapup '{"accomplishments":"…","handoff_notes":"…"}'` records the structured payload on the agent bead; mandatory `accomplishments` field is enforced. | Run `gb stop --wrapup` with valid JSON; assert payload appears under the agent bead's `wrapup` or equivalent field; empty `accomplishments` → exit ≠ 0. | Live daemon. | **S** |
| **P11** | `prime-after-compaction.toml` | `gb prime` re-injects identity + assignment + advice after `/clear`-style restart. Simulates by running prime twice in one scenario and asserting the second run still surfaces the agent's assignment block. | Two successive `gb prime` runs from inside a single claudeless turn; both stdouts contain `## Assignment` and `## Your Identity`. | None beyond `local-prime-fetch.toml` baseline. | **S** |
| **P12** | `gate-decision-full-cycle.toml` | End-to-end: Stop hook blocks (exit 2) → agent calls `gb decision create --no-wait` against the live daemon → operator resolves out-of-band → `gb yield` returns → Stop hook now exits 0. Replaces the smoke version in `gate-decision-flow.toml` with a real CLI loop. | Stop hook protocol exit codes traced through `gb hook stop-gate`; final `gb stop` succeeds. | Live daemon; out-of-band resolver (same pattern as `local-yield-close.toml`). | **L** |

Optional follow-ons (defer unless a bug surfaces):

- `bus-emit-prehook.toml` — claudeless-style assertions for `PreToolUse` /
  `PostToolUse` hook events the way `stop-gate-cooldown.toml` covers Stop.
- `crew-spawn-from-gardener.toml` — a gardener scenario that spawns a crew
  via `gb spawn --task` and asserts the resulting agent bead, currently
  uncovered.
- `pool-warm-and-claim.toml` — exercise `gb pool` prewarmed-agent path.
  Niche; lowest priority.

------

## 5. Recommendation

**Land first (week 1):** P1 `squawk-roundtrip`, P2 `decision-create-rejects-acks`,
P4 `bundle-create-add-show`, P10 `stop-wrapup-payload`, P11
`prime-after-compaction`.

Reason: each is **small**, exercises a surface used by **every** agent
cycle, and depends only on a live daemon (the same dependency `local-*`
scenarios already have). Authored as a batch they close ~40% of the
crew/communicate gap. P1 alone gives us a regression net for the most-used
outbound channel in the system — a missing fields drop would otherwise
silently break gardener status updates.

**Land second (week 2):** P3 `decision-report-artifact-required`,
P6 `claim-and-release-roundtrip`, P12 `gate-decision-full-cycle`.

Reason: medium-complexity, but they pin the decision/yield/claim
contracts that are referenced in every nudge. P12 in particular replaces
a smoke scenario with a real-CLI flow — strictly better coverage at the
same cost.

**Defer to a follow-up bead:** P5 `news-active-vs-stale`,
P7 `mail-send-and-inbox`, P8 `workspace-setup-teardown`, P9
`escalation-cluster-mode`.

Reason: each is independently useful, but requires either a stub
(receiver agent in P7), a fixture (stale-agent simulation in P5), or
already-covered surfaces in another mode (P8 + P9). Tractable but lower
marginal value once the first two batches land.

**Tracking work outside scenario authoring:**

1. **Unquarantine gardener-trigger-* live-flow scenarios** — file a
   follow-up bead under epic ka-0000000619 tracking the SKIP_SCENARIOS
   removal once the scheduler reconcile interval becomes test-tunable
   (depends on ka-0000005245). Adds ~6 surfaces' coverage with zero new
   scenarios.
2. **Optional upstream PR to claudeless** for a generic `[tools.<name>]`
   block (§3.3). Low priority — Bash funnelling covers every current
   case. Worth a 1-page proposal if we ever need real MCP shape pinning.

**Out of refresh scope (explicitly noted, not deferred):** MR/CI surfaces
(§3.4), real-Slack thread delivery, JIRA bridge round-trips. These are
real-integration tests, not claudeless candidates, and the epic
deliverable cleanly excludes them.

------

## Appendix A — File-by-file scenario inventory (42 files)

```text
gate-decision-flow.toml                        gate / decision (smoke)
gate-no-identity.toml                          gate / fails open
stop-gate-cooldown.toml                        stop hook cooldown + escape hatch
mail-nudge-delivery.toml                       check-mail / drain-queue
agent-crew-lifecycle.toml                      crew lifecycle (smoke)

local-lifecycle.toml                           local-mode lifecycle
local-prime-fetch.toml                         local-mode prime + check-mail
local-yield-close.toml                         local-mode decision → yield → resolve
local-sigterm.toml                             local-mode SIGTERM (skipped by claudeless runner)
local-orphan-cleanup.toml                      local-mode reaper (placeholder asserts)
local-task-preassignment.toml                  local-mode --task plumbing
local-escalation-roundtrip.toml                local-mode escalation SSE → coop
local-coop-states.toml                         coop attach/nudge HTTP control plane

gardener-gardener-start-idempotent.toml          gardener start idempotency
gardener-mention-entrypoint.toml                Slack mention → gardener start shape
gardener-conditions-fan-in.toml                 triggers fan-in shape
gardener-progress-prime.toml                    progress block rendered
gardener-stop-no-progress.toml                  stop block no_progress_for
gardener-stop-wall-clock.toml                   stop block wall-clock default + override
gardener-stop-loop-end-bead-query.toml          stop block bead_query live-fire
gardener-stop-hard-stop-consecutive-failures.toml  hard-stop counter live-fire
gardener-trigger-bead-closed-match-parent.toml       *quarantined*
gardener-trigger-bead-created-match-parent.toml      *quarantined*
gardener-trigger-bead-event-in-subtree.toml          *quarantined*
gardener-trigger-bead-event-label-predicate.toml     *quarantined*
gardener-trigger-debounce-coalesce.toml              *quarantined*
gardener-trigger-decision-option-chosen.toml         *quarantined*
gardener-trigger-decision-resolved-match.toml        *quarantined*
gardener-trigger-fan-in-cron-precedence.toml         *quarantined*
gardener-trigger-throttle-burst.toml                 *quarantined*

trigger-source-disable-stop.toml               trigger-engine disable invariant
trigger-source-restart-dedup.toml              trigger-engine dedup across restart
trigger-source-schedule-conditions.toml        false condition → no fire
trigger-source-schedule-fires.toml             cron tick → notify
```

(Plus eight `local-*.startup-prompt.txt` sidecar files.)

## Appendix B — Prior epic ka-0000000165 children (all closed)

```
ka-0000000120  Audit and prune unused local_claude execution_target
ka-0000006370  Add --scenario flag to gb agent start --local
ka-0000008139  Author local-mode claudeless scenarios
ka-0000002599  test-local-mode.sh runner
ka-0000000772  Coop integration tests for local mode
ka-0000007522  Escalation round-trip test for local mode
ka-0000004657  Support --task pre-assignment
ka-0000000375  Crash robustness + orphan-bead reaper
ka-0000007089  Multi-local-agent forward-compat foundations
ka-0000004098  CI wiring + daily-driver runbook
```
