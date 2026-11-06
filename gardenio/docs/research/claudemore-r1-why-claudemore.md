# Why claudemore — R1 validate-goals report

**Bead:** ka-0000003209 (R1 of research epic ka-0000006064).
**Author:** `gardenio-r1-validate-goals-cmt`.
**Operator:** Matt Baker (oidc:matthew.baker@acme.example).
**Scope decision:** ka-0000007358 — claudemore is a real **production** LLM proxy
that speaks Claude Code, sitting alongside/in front of litellm; e2e is **one** use
case, not the whole point.

This report validates the premise of claudemore: what it concretely buys gardenio
over the three things that exist today — **claudeless** (scripted mock), **litellm**
(real model-byte proxy), and the **current E2E suite**. Method: a fan-out analysis
across four facets, an adversarial verification pass that tried to *refute* every
"only real-reasoning catches this" claim (29 of 31 survived; 2 refuted are surfaced
honestly in §6), and a synthesis. Research only — no shipping code.

------

## 1. TL;DR

- **The gap is structural, not incidental.** claudeless proves gardenio *plumbing*
  behaves given a fixed agent trajectory; litellm proves model *bytes* flow and are
  billed. Neither asserts anything about what real Claude Code *decides, routes,
  reads, refrains from, or loops on*. Verified against
  `gardenio/tests/e2e/claudeless/*.toml` (53 `Bash` + 1 `AskUserQuestion` tool calls,
  **zero** model calls) and `gardenio/docs/litellm-production-cutover.md`.
- **The codebase already conceded this.**
  `gardenio/docs/research/claudeless-critical-path-parity.md` §3.1–§3.3 explicitly
  DROPs prompt interpretation, bead selection, `<system-reminder>` parsing,
  MCP/subagent tool-use, and prohibited-tool avoidance as "real-Claude E2E" concerns
  claudeless "structurally cannot" cover.
- **Operator scope is production-proxy-first.** Decision `ka-0000007358` chose
  `proxy` over `harness`: "a real production LLM proxy that speaks Claude Code,
  sitting alongside/in front of litellm, with e2e as one use." The build epic
  `ka-0000006064` is explicitly "LARGER than a test harness."
- **The production half is already proven by a one-off hack.**
  `litellm_thinking_callback.py` exists because the proxy is the *only* vantage point
  that sees reasoning plaintext — and it pays a tax (header-scraped identity,
  best-effort session id, observe-only-after-success) that a CC-aware proxy removes.
- **Stay disciplined on cost.** Do NOT build a second spend path;
  `litellm-only-cost-estimation-design.md` makes litellm the single source of truth
  and that pipeline is already E2E- and unit-tested. claudemore augments litellm; it
  does not replace its routing/metering layer.

## 2. The crisp problem statement

> gardenio treats Claude Code as an opaque endpoint: litellm (`gardenio-litellm`)
> proxies its bytes for cost, routing, and budget but is blind to its protocol, and
> claudeless only fakes that protocol with scripted TOML inside the test suite.
> Nothing in the running system *speaks Claude Code* — understands its tool calls,
> Workflow runs, hooks, skills, MCP, and subagent semantics — so those features have
> no first-class, observed, tested home. claudemore is a production LLM proxy that
> speaks Claude Code, sitting alongside/in front of litellm. Because it sits in the
> real traffic path and comprehends the protocol, it makes CC features governable in
> production *and* enables high-fidelity E2E with real reasoning — closing the gap
> claudeless can't reach. E2E is one beneficiary of the proxy, not its reason for
> existing.

## 3. Failure classes only real-reasoning E2E catches

Each class below survived adversarial verification: the failure lives in the agent's
*trajectory*, not the plumbing, so claudeless (pre-scripted) and litellm (byte-level)
are structurally blind. A green claudeless run certifies these as fine while
production degrades.

| # | Failure class | Surface | Why claudeless/litellm can't |
|---|---|---|---|
| 1 | **Non-divergent / daily-check-in decision** | Smell test in `.gb/system-prompt.md`; guard in `kbeads/internal/server/decision_validation.go` | Guard is *intentionally permissive* — rejects only zero-option / single-ack. The "options must genuinely diverge" and "no daily check-in" (`e2e-direction.md`) rules are **prose to the model**, enforced nowhere in code. claudeless files whatever the TOML author wrote. |
| 2 | **Channel misrouting** (squawk vs decision vs escalation vs mail) | `.gb/system-prompt.md` "When to use what" table; advice `ka-0000001089` | `gb squawk` appears in **zero** of the ~54 TOMLs; channel is a hardwired `on={contains=...}` → fixed `Bash` block, never the model's choice. Parity doc names `gb squawk` "the single biggest unasserted surface." |
| 3 | **Wrap-up `blockers` misuse** | `gb stop --wrapup`; `controller/cmd/gb/wrapup.go` | Server validates only required-field *presence*; `blockers:"none"` passes and the card-render path deterministically stamps the Slack card ⛔ blocked. The omit-when-clean judgment is the model's; the unit test even bakes in `blockers:"none"` as valid. |
| 4 | **`<system-reminder>` comprehension** | `images/agent/hooks/check-mail.sh`, `stop-gate.sh` inject reminders | claudeless asserts only the inject-*queue content* (`mail-nudge-delivery.toml`); it treats the reminder as opaque text on its next scripted turn. Parity doc §3.2: "the parsing side is what real Claude does … lives in real-Claude E2E." |
| 5 | **Wrong `gb ready` bead selection** | `controller/cmd/gb/ready.go`; `agent-crew-lifecycle.toml` | TOML runs `gb ready` (output unasserted) and claims via `echo 'Simulated claim…'` — never real `kd claim`. Parity §3.1 explicitly DROPs "picks the right bead." Unit tests assert query *construction* only. |
| 6 | **Epic decomposition judgment** | `kd dep add --type parent-child\|blocks`; advice `ka-0000005995` | Coverage matrix marks `kd dep add` **N** (setup-only, never asserted). All uses are `--type parent-child`; `--type blocks` appears in zero scenarios. Whether real Claude builds an epic vs. fans out orphans is author-encoded. |
| 7 | **Gardener no-progress self-termination** | `scheduler/gardener_stop.go`; `progress/evaluator.go` | Unit tests pin the evaluator *math* on hand-built `progress.State`. A real gardener that churns one bead per cycle scores ≥1 forever (credited-event rescue) and never self-blocks — runaway spend; or a productive gardener emitting score-0 wrap-ups trips the barren backstop and is wrongly killed. Both depend on real run behavior. |
| 8 | **Stop-gate rapid-fire loop** | `stop-gate.sh` cooldown + escape hatch + yield marker | `stop-gate-cooldown.toml` hand-seeds `block-count` and tests the *filesystem* paths. The actual defended-against loop — a real model that re-stops immediately after an exit-2 block — is emergent from generation. Parity doc concedes "full-cycle … not asserted." |
| 9 | **MCP / non-Bash tool-call shape** (Playwright, Mezmo, Edit/Write) | `images/agent/install/playwright.sh`; agent MCP servers | All scenarios use only Bash + AskUserQuestion; Edit/Write/Read funneled through `sed`/`cat`. coop's `parse_tests.rs` asserts *display labels* on hand-authored fixtures, not that a real CC release emits well-formed calls. A renamed Edit key passes every test. |
| 10 | **Prohibited blocking-tool avoidance** | `.gb/system-prompt.md` (TodoWrite/EnterPlanMode/AskUserQuestion) | A *negative* behavioral constraint — claudeless can't prove a model *spontaneously refrains*; `local-coop-states.toml` uses the AskUserQuestion shape *intentionally*. A unit test can assert the prohibition string is present, not that the model obeys it. |
| 11 | **Slack-thread read-before-reply** | advice `ka-0000002045`; `gb slack reply`/`thread` | Coverage matrix marks both **N** ("needs a Slack stub"). Whether real Claude *chooses to read full history first* is a sequencing/reasoning behavior, not a CLI contract. |
| 12 | **Subagent / Workflow / hook feature usage** | `ka-0000006064`; `images/agent/scenarios/teams-mode.toml` | Only *config-presence* is tested (`teams-mode.toml` `cat`s `settings.json`); `hook_relay_test.go` is pure payload marshalling. No real-reasoning test asserts correct use of TeamCreate/SendMessage/subagents/Workflow. |

## 4. Overlap & boundary map

claudemore **augments** all three — it replaces none. claudeless stays the cheap,
deterministic, zero-spend plumbing net (`run-shard.sh` "no model spend"); litellm
stays the single source of truth for cost; the E2E suite gains a new real-reasoning
module class.

| Capability | claudeless | litellm | E2E-today | claudemore |
|---|---|---|---|---|
| Model byte routing | Gap (no model) | **Covered** | Covered (via litellm) | Pass-through to litellm |
| Cost / spend tracking | Gap | **Covered** (`spend_sync.go`, SoT) | Covered (`litellm-cost-tracking`) | **Gap by design** — must not duplicate |
| Plumbing lifecycle (CLI/hook/gardener) | **Covered** (fixed trajectory) | Gap | Partial | Partial (same plumbing, real call shapes) |
| **Agent-choice correctness** (classes 1–8, 11) | **Gap** (pre-scripted) | Gap | **Gap** (out of charter) | **Covered — headline net-new** |
| **CC-feature correctness** (MCP, subagents, Workflow, hooks; classes 9, 12) | Gap (Bash-only) | Gap | Gap | **Covered** |
| **`<system-reminder>` interpretation** (class 4) | **Gap** (opaque text) | Gap | Gap | **Covered** |
| Determinism / zero-spend CI net | **Covered** | Partial (real spend) | Partial | Gap (real reasoning costs spend, nondeterministic) |

**Boundary rules:**

- **claudeless** — owns deterministic, zero-spend verification that gardenio
  *plumbing* behaves when the agent's choices are pre-scripted. Stops the instant an
  assertion depends on what a real model would decide, parse, or call.
- **litellm** — owns the production model-byte plane: Anthropic API routing + single
  source of truth for cost (`litellm-only-cost-estimation-design.md`). Stops at the
  bytes; no concept of lifecycle or choice. **Cost metering stays exclusively here.**
- **E2E-today** — owns full-suite integration against gardenio-rwx (deploy, smoke,
  cost-tracking, scripted-agent plumbing), explicitly excluding prompt interpretation
  (`e2e-direction.md`). claudemore adds a real-reasoning module class *inside* this
  suite.
- **claudemore** — sits in front of litellm on a higher protocol altitude:
  `Agent → claudemore (speaks Claude Code) → litellm (routing + spend) → Anthropic`.
  Owns orchestration-protocol concerns litellm has no concept of. The one redundancy
  to guard against is cost — keep metering in litellm.

## 5. Production-proxy value over litellm

litellm is a model-bytes proxy that has *already been forced past its byte layer
once*, and the seam is the evidence.
`gardenio/helm/gardenio/files/litellm/litellm_thinking_callback.py` exists because
reasoning plaintext is visible *only* in the live streamed response in transit
through the proxy (Anthropic strips it on persist —
`{"thinking":"","signature":"..."}`). That callback proves both halves of the
claudemore premise: (a) the proxy is the right place to observe agent semantics, and
(b) litellm is the wrong *shape* for it — it has zero native notion of the agent
session, so it recovers the actor by string-matching a header
(`x-litellm-tags: agent:<name>`), best-efforts `session_id` from the litellm call id,
and runs observe-only *after* `async_log_success_event`.

A Claude-Code-aware proxy turns that one-off hack into a first-class layer:

1. **Native session/run/subagent identity** instead of header archaeology. It parses
   the request/response as Claude Code messages and *knows* this is turn N of session
   S, subagent depth D, Workflow run R. Every observation (cost, thinking, latency)
   attaches to real identity for free.
2. **Reasoning-step observability**, not just the token bill — `Task(...)` spawns,
   `EnterPlanMode`, `<system-reminder>` injections: the actual control flow, at the
   one chokepoint every agent transits, with no agent-pod change.
3. **Inline feature-level policy** — deny/rewrite at request time ("this account may
   not spawn subagents," "block MCP tool X," "cap recursion depth"). The deployed
   config has **only** a success-logger callback (`configmap.yaml`) and no
   `async_pre_call_hook`/guardrail — litellm's callback model is observe-only by
   design.
4. **Record/replay of real sessions.** coop already persists and resumes the *agent
   side* (Workflow companion dir `agent-*.jsonl` + run-state,
   `coop/crates/cli/src/s3.rs`), but the paired model request/response stream that
   *produced* those decisions is forwarded by litellm and captured nowhere. A
   CC-aware proxy is the missing recorder — keyed to the same identity coop persists —
   giving deterministic replay claudeless can never have.

## 6. What the existing stack already covers (do NOT over-build)

These claims were refuted under verification — the existing stack already handles
them, so claudemore should *not* take them on:

- **Cost / spend metering is fully owned and tested.** litellm is the single source
  of truth (`litellm-only-cost-estimation-design.md`); the pipeline is validated
  end-to-end with no real Claude by `test-litellm-cost-tracking.sh` (run-shard module
  `litellm-cost-tracking`) plus the cost-guard unit tests. **claudemore metering
  spend would be redundant and would conflict with the SoT.**
- **Per-turn / per-subagent real-time spend is out of scope by deliberate decision** —
  the speculative `spend_forwarder` and coop `response_cost` plumbing were deleted as
  dead code. Session-close reconcile via `/spend/tags` is the chosen granularity.
  Don't put per-turn cost back without an explicit human owner.
- **Thinking-plaintext capture already works** via a vanilla LiteLLM `CustomLogger`
  on the *existing* proxy and is unit-tested offline. claudemore should *subsume* this
  if it's already parsing the stream — but building claudemore *solely* to relocate
  this one callback would not pay for itself.
- **Model-alias routing & per-account virtual-key budgets** are deterministic
  request-metadata operations, unit-tested and E2E-covered. A CC-aware proxy adds zero
  fidelity to a string-swap or a token-count sum. Keep these in litellm.
- **Server-side decision guard, required-`accomplishments` enforcement, gardener-stop
  evaluator math, stop-gate filesystem paths** — all genuinely unit-tested. Only the
  *judgment* variants (classes 1, 3, 7, 8) need real reasoning; the mechanical guards
  do not.

## 7. Verdict: is claudemore the right primitive?

**GO** — with a hard scope fence on cost. The "why claudemore" rests on two
independently verified pillars. (a) The *production-proxy* pillar is the operator's
stated intent (`ka-0000007358` chose `proxy` over `harness`; `ka-0000006064` is
"larger than a test harness") and is already proven necessary by the thinking-capture
hack — gardenio has demonstrably needed to observe Claude Code semantics at the proxy
and has paid the tax of doing it blind. (b) The *real-reasoning-E2E* pillar is a
sharp, structural gap that 12 separately-verified failure classes occupy and that the
codebase's own parity analysis concedes claudeless cannot close. The dividing line is
crisp: claudeless + litellm prove "plumbing is correct given a fixed trajectory and a
flowing byte-stream"; every class in §3 is a regression in the *trajectory itself* —
exactly what a Claude Code release or a system-prompt/advice edit introduces, and
exactly what a green claudeless run will certify as fine while production degrades.
The single discipline that keeps this from over-building: claudemore augments litellm
(CC-semantic awareness, policy, record/replay), it does not reimplement litellm's
routing or cost layer — the cost-estimation epic is a standing warning against
duplicating model-bytes plumbing that already works.

------

## Appendix — method & verification

- **Fan-out:** four parallel analysts (fidelity-gap, overlap, production-proxy-value,
  problem-statement) over the grounded inputs below.
- **Adversarial verify:** 31 falsifiable claims ("only real-reasoning catches X" /
  "litellm cannot do Y") each handed to a refuter instructed to default to *refuted*.
  **29 survived, 2 refuted** (the refuted ones are §6's "do not over-build" list).
- **Grounding sources:** `gardenio/docs/research/claudeless-critical-path-parity.md`,
  `gardenio/tests/e2e/claudeless/*.toml`, `gardenio/tests/e2e/scripts/run-shard.sh` +
  `test-claudeless.sh`, `gardenio/docs/e2e-direction.md`,
  `gardenio/docs/litellm-production-cutover.md`,
  `gardenio/docs/litellm-only-cost-estimation-design.md`,
  `gardenio/helm/gardenio/files/litellm/litellm_thinking_callback.py`,
  `coop/crates/cli/src/s3.rs`, epic `ka-0000006064`, decision `ka-0000007358`.
- **Note for R2/R3/R5:** §3 is the failure-class catalog R5's acceptance criteria
  should map onto; §4's boundary rules and §6's "do not over-build" list are the
  scope fence R2's architecture and R5's goal ladder should honor (claudemore augments,
  never replaces, litellm + claudeless).
