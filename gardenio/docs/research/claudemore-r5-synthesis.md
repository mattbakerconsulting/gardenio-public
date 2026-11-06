# claudemore R5 — Synthesis: recommended architecture, goal ladder, build-epic skeleton, coexistence policy

> **Bead:** ka-0000001264 (R5 synthesis) · **Epic:** ka-0000006064 (claudemore research) · **Bundle:** /bundle/ka-0000006676
> **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Author:** gardenio-r5-synthesis-goal-5dp (crew) · **Date:** 2026-06-14
> **Scope decision:** ka-0000007358 — claudemore is a real **production** LLM proxy that
> speaks Claude Code, sitting alongside/in front of litellm; **e2e is ONE use case,
> not the whole point.** The build epic this report skeletons is therefore
> **production-grade**, not just a test harness.
>
> **Status:** research-only. This report synthesizes R1–R4 into a decision-grade
> deliverable and files an operator go/refine/defer checkpoint. **No build work is
> started; no claudemore code ships; no new e2e scenarios are authored.**

---

## 0. TL;DR

- **GO is the converged verdict of the research**, with a hard scope fence. R1 returns
  **GO** (with a cost fence); R2 recommends a concrete buildable shape; R3 returns a
  per-feature gap matrix + roadmap order; R4 returns **🟡 GO-WITH-GUARDRAILS** with five
  must-build preconditions. None of the four found a hard blocker.
- **Recommended architecture (R2 candidate d, scoped to a+c):** claudemore is a **thin,
  Claude-Code-semantics-aware recording reverse-proxy** on the LLM path, exposed as
  `ANTHROPIC_BASE_URL`, **fronting (never replacing) litellm**, with two modes —
  **LIVE/RECORD** (production: forward to litellm→Anthropic + capture cassettes) and
  **REPLAY** (CI: serve cassettes at $0/deterministic). Rich CC semantics (hooks,
  Workflow, MCP, subagents) are reached by **composing with coop** — the real `claude`
  binary runs under coop exactly as in prod — **not** by absorbing orchestration into
  the proxy.
- **The crux finding that shapes everything (R2 §2):** most Claude Code semantics are
  **client-side** and **invisible on the Anthropic Messages wire**. A wire-level proxy
  is a great *recorder of LLM exchanges* and a weak *observer of CC semantics*. That is
  why the architecture is proxy **+** coop, not proxy-alone.
- **The production half already pays for itself** (R1 §5): the `litellm_thinking_callback.py`
  hack exists *only* because the proxy is the one vantage point that sees reasoning
  plaintext — and it pays a tax (header-scraped identity, observe-only-after-success)
  that a CC-aware proxy removes. claudemore turns a one-off hack into a first-class layer.
- **Goal ladder:** **MVP** = LIVE pass-through + record proving transparency &
  reversibility; **Target** = REPLAY-backed deterministic e2e tier running the real
  `claude` binary at $0, grounded in the R1 12 failure classes; **Stretch** = the full
  R3 feature roadmap (Hooks→Subagents→MCP→Skills→Workflow) + staleness automation +
  beads-viewer surface.
- **Coexistence (R1 §4 + R4 §3):** **claudemore AUGMENTS, never replaces, claudeless.**
  claudeless owns the deterministic CLI/hook/bead **contract**; claudemore owns
  **reasoning/integration outcomes**. Cost metering stays **exclusively** in litellm.
  claudemore is **never a per-PR hard gate.**

---

## 1. What each research task concluded (and how they converge)

| | Task | Verdict / core output | What it hands R5 |
|---|---|---|---|
| **R1** | ka-0000003209 — why claudemore | **GO** with a hard cost-scope fence. 12 failure classes only real-reasoning e2e catches; overlap/boundary map; "do-not-over-build" refuted list (cost metering, per-turn spend, model-alias routing stay in litellm). | The **12 failure classes** = the source of MVP/target acceptance criteria. §4 boundary rules + §6 refuted list = the scope fence. |
| **R2** | ka-0000005113 — architecture survey (MR !4272) | Recommends **candidate (d) scoped to (a)+(c)**: thin CC-semantics-aware record/replay proxy fronting litellm + coop as the real-CC client. **Crux:** CC semantics are client-side, invisible on the wire. | The **architecture of record** + a goal-ladder seed (MVP/target/stretch) + COEXIST+FRONT litellm stance. |
| **R3** | ka-0000003637 — CC-feature inventory (MR !4274) | Per-feature matrix: **Hooks=HAVE**; MCP / Subagents+teams / Slash+Skills / Workflow = **PARTIAL**. Roadmap order **Hooks(S) → Subagents/teams(M) → MCP(M) → Slash+Skills(M) → Workflow(L)**. 6 open questions for R5. | The **phased build-epic ordering** + per-feature "first-class + tested" definitions + the open questions §3 resolves. |
| **R4** | ka-0000004533 — de-risking | **🟡 GO-WITH-GUARDRAILS.** Risk register across cost/determinism/secrets/isolation/coexistence; 5 must-build guardrails; coexistence policy. | The **preconditions** (Phase 0 of the build epic) + the operator-checkpoint questions + the cost envelope. |

**Convergence.** All four independently land on the same shape: a proxy that **fronts**
litellm, **records** real CC↔LLM exchanges, and **replays** them deterministically for
e2e — with coop as the real-CC engine and claudeless retained as the fast plumbing tier.
R1's "augment-never-replace" boundary, R2's "front-never-replace litellm" stance, R3's
"plumbing is solid, the gap is a real model in the loop," and R4's "claudeless owns the
contract, claudemore owns reasoning" are **the same policy from four angles.** That
agreement is the strongest signal in the bundle: the architecture is not contested.

---

## 2. Recommended architecture

### 2.1 Shape (R2 candidate d → scoped to a+c, composed with coop)

```text
Production (LIVE/RECORD):
  agent pod ── coop ──► claude (real binary)
                          │  ANTHROPIC_BASE_URL=claudemore:PORT
                          ▼
                     claudemore  ──(forward + record cassette)──►  litellm ──► Anthropic
                          │                                          │
                     cassette store (S3, reuse coop record/redact)   └─ spend_sync + reconcile (UNCHANGED)

CI e2e (REPLAY):
  agent pod ── coop ──► claude (real binary)   ← hooks/Workflow/MCP/subagents all REAL
                          │  ANTHROPIC_BASE_URL=claudemore:PORT  (replay mode)
                          ▼
                     claudemore  ──(serve cassette, $0, deterministic)──►  ✗ litellm/Anthropic bypassed
```

Three load-bearing properties:

1. **claudemore is a thin Messages-API recording reverse-proxy.** In LIVE it unmarshals
   the Anthropic Messages request/response, captures the exchange to a cassette, and
   forwards to litellm unchanged. In REPLAY it canonicalizes the request, hashes it, and
   serves the matching cassette byte-for-byte. This is R2 (a)+(c) packaged as one binary
   — the coherent core of (d).
2. **It does NOT absorb orchestration.** The "drive a real CC subprocess" capability
   (R2 candidate b) **already exists in coop** (PTY spawn, S3 persistence, NATS thinking,
   Workflow companion-dir sync/resume). claudemore *composes with* coop: coop runs the
   real `claude`; the proxy records/replays its LLM traffic. **Folding coop's
   PTY/FIFO/NATS surface into the proxy is the over-engineering trap (R2 §6) — avoid it.**
3. **litellm is fronted, never replaced (R1 §4, R2 §4).** Spend tracking, virtual keys,
   model-alias routing, OAuth forwarding all stay in litellm, untouched. In LIVE,
   litellm still sees every request and tags by `agent:<name>`; `spend_sync.go` and
   `reconcile_session_spend.go` keep working with zero change. In REPLAY, litellm is
   bypassed entirely — which is correct, because a replayed session has no real cost.

### 2.2 Why this shape and not the alternatives (R2 §6)

- **(a) wire-proxy alone** — too weak: wire observability of `tool_use` *names* isn't
  worth a new service when litellm owns cost and the real semantics aren't on the wire.
- **(b) CC-subprocess alone** — this *is* coop; it adds no determinism and costs real
  tokens in CI. Real value, but already in gardenio: reuse, don't rebuild.
- **(c) record/replay alone** — it *is* the e2e tier, but a tier *on* (a), not a
  free-standing architecture.
- **full (d) with ORCHESTRATE folded in** — XL effort, kitchen-sink; duplicates coop.

### 2.3 The crux: where CC semantics live (R2 §2)

This is the single most important finding and the reason the architecture is
proxy **+** coop. For each first-class feature: is it visible on the Anthropic wire?

| Feature | Visible on Messages wire? | Reached by |
|---|---|---|
| Workflow tool (+ built-in Bash/Read/Write/Edit) | **No** — only the enclosing `tool_use` *name* | coop (runs the binary) |
| Slash commands | **No** — parsed in the CC REPL | coop |
| Skills | **Partial** — `Skill` tool invocation name+args; impl internal | coop |
| Hooks (SessionStart/PreToolUse/Stop/…) | **No** — FIFO/shell, local files/NATS | coop (relays them) |
| MCP servers | **No** (protocol) / **Partial** (defined tools as generic `tool_use`) | coop |
| Subagents / teams | **Partial** — `Agent` tool_use visible; execution separate | coop (+ proxy if subagent LLM calls route through it) |

**Takeaway:** the proxy is the **recorder of LLM exchanges**; coop is the **observer of
CC semantics**. Any design that tries to make one wire-level proxy "speak Claude Code"
in the rich sense structurally cannot — it sees only the thin wire-shadow.

### 2.4 Determinism model (R2 §5, R4 §2.2)

- **claudeless fakes the agent (the binary).** Deterministic by construction, $0, fast —
  but it can *never* run the real `claude` binary, so it can never validate real
  hook/Workflow/MCP behavior.
- **claudemore-REPLAY fakes only the LLM**, keeping the agent **real**. Same request hash
  → byte-identical canned response → the real binary exercises hooks, Workflow, tool
  dispatch, MCP for real. This is the structural advantage claudeless cannot match.
- **Top determinism risk: cassette staleness** (R2 §5). A stale cassette silently becomes
  "truth." Mitigations: version cassettes by model tag; periodic LIVE-vs-cassette canary
  to detect drift; explicit `fail_on_miss` so an incomplete set fails loudly rather than
  silently falling back to LIVE.
- **Determinism is 🟢 only conditional on BOTH record/replay AND an explicit per-account
  model pin** (R4 §2.2): there is no opus-4-8 default — `claudeModel: ""` resolves to
  Claude Code's built-in default (Sonnet), which Anthropic can change silently. The pin
  is **load-bearing, not belt-and-suspenders.**

---

## 3. Resolving R3's open questions (so the build epic can execute)

R3 §4 raised six questions R5 must answer for the goal ladder to be executable. These
are **synthesis recommendations**, not unilateral decisions — they become the build
epic's design tasks, and the operator decision (§7) can override any of them.

| # | R3 question | R5 recommendation |
|---|---|---|
| Q1 | Does claudemore emit the exact wire artifacts the plumbing parses (hook stdin JSON, subagent events, Workflow journals)? | **Out of claudemore's hands by design.** Hook JSON, SubagentStart/Stop events, and Workflow `agent-*.jsonl` journals are produced by the **real `claude` binary under coop**, not by the proxy. claudemore only records/replays the **LLM** exchange. This is exactly why the architecture is proxy+coop. The build epic's **Phase 1 (Hooks)** is the proving ground that confirms the real binary emits these against a replayed LLM. |
| Q2 | Workflow: must the entrypoint gain a rehydrate step + `s3-resume` binary be built, or does coop's `restore_workflow_dir` subsume it? | **Defer to Workflow phase (last).** This is a real dependency knot (two disjoint restore paths; `s3-resume` referenced but nonexistent). It is **internal to the Workflow-resume story** and does not block the proxy MVP/target. Scope it as a design spike *inside* the Workflow phase, gated on Q1c being answered by earlier phases. |
| Q3 | MCP/channel: will `gardenioChannel` default true / run a `true` e2e variant? Is the `!mockMode` gate permanent? Is the rwx marketplace reachable from locked-down pods? | **Keep default off; add a `gardenioChannel=true` e2e *variant* under claudemore** rather than flipping the global default. The `!mockMode` gate becomes testable precisely because claudemore runs a *real* (non-mock) binary. Marketplace reachability must be **validated against R4's egress allowlist** — if network policy blocks it, plugin skills are effectively unavailable and that becomes a Skills-phase finding. |
| Q4 | Skills/commands need a stable target: ship a canonical `/command` or skill? Distinct `SlashCommand` tool_use or always `Skill`? | **Ship one canonical agent-facing skill as the e2e target** (none exist in `.claude/commands/` today). coop only recognizes the `Skill` tool, so treat slash commands as surfaced via `Skill` for now; a distinct `SlashCommand` recognizer is a Skills-phase stretch item. |
| Q5 | Subagents: is durable per-subagent bead attribution in scope, or is `workflow_spend` rollup sufficient? Is teams still experimental? | **`workflow_spend` rollup + transient Slack chunks are sufficient for first-class; durable per-subagent beads are STRETCH.** Teams remains behind `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` — parity-test the in-process default; treat tmux-pane viewer awareness as stretch. Also fix the trivial `SubagentStop` output-drop (`agentstream.go:828-840`) early — it's a one-line correctness bug, not a feature. |
| Q6 | Can claudemore be driven to *deterministically* invoke a named MCP tool / Skill / Workflow? | **Determinism comes from REPLAY, not from prompting.** Assertions target **outcomes/side-effects**, never model text or exact tool-ordering (R4-DET-1/2). A cassette pins the model's behavior for a given request; the e2e asserts the *effect* (file/bead/stdout/NATS event), which is stable across the replayed run. Natural-language prompting that "may flake" is avoided by construction. |

---

## 4. Goal ladder

Each rung has **acceptance criteria**. MVP criteria are grounded in the **R1 12 failure
classes** (the structural gaps only real-reasoning e2e can catch) and the R4 preconditions.

### MVP — "the proxy is real, transparent, reversible, and records"

**Goal:** claudemore exists as a production LIVE pass-through proxy fronting litellm, with
cassette **record**, proving it changes nothing about the existing system and can be
rolled back instantly.

**Acceptance criteria:**
- **A1 (transparency).** With agents pointed at `ANTHROPIC_BASE_URL=claudemore`, litellm
  still sees every request tagged `agent:<name>`; `spend_sync.go` + `reconcile_session_spend.go`
  produce **identical** `total_cost_usd` / `gardenio_agent_spend_usd_total` vs. a litellm-direct
  baseline. **claudemore meters no spend itself** (R1 §6 fence; R2 §4).
- **A2 (reversibility).** Reverting the account-bead `proxy_url` field falls back to
  `litellm:4000` within ~60s (the litellm-cutover rollback property, R2 §4). A documented,
  tested rollback runbook exists.
- **A3 (record fidelity).** A real production session is captured to an S3 cassette
  reusing coop's record + secret-redaction machinery (`record.rs`, `s3.rs`); the cassette
  round-trips (request canonicalization + hash is stable across re-record of the same
  input).
- **A4 (redaction — R4-SEC-3).** Recorded cassettes contain **no** `sk-ant-` / litellm
  `sk-`/`vk-` shaped secrets (redaction layer F16 live and asserted on a deliberately
  key-echoing session).
- **A5 (Phase-0 preconditions in place).** The R4 must-build guardrails that gate *any
  real cred touching CI* are wired and verified (see §6 Phase 0). Specifically: per-CI
  `max_budget` virtual key 429s at cap on a deliberate runaway test; explicit per-account
  model pin set; egress allowlist blocks `0.0.0.0/0`; `buildCoopEnv` allowlist (F2) closed.

**Grounding in R1 failure classes:** MVP does not yet *assert* the 12 classes — it builds
the **substrate** that makes them assertable (a real binary against a deterministic LLM).
A4/A5 directly address the R4 secrets/cost preconditions that must hold before that
substrate touches CI.

### Target — "deterministic real-CC e2e tier, grounded in the R1 failure classes"

**Goal:** REPLAY mode + a deterministic CI e2e tier running the **real `claude` binary**
against cassettes at **$0**, asserting the headline real-reasoning failure classes that
claudeless structurally cannot.

**Acceptance criteria** (each maps to R1 §3 failure classes; assert **outcomes**, never
model text — R4-DET-1):
- **T1 (REPLAY $0/deterministic).** A claudemore-REPLAY e2e module runs the real binary
  under coop with **zero real tokens** and **byte-identical** results across reruns;
  `fail_on_miss` makes an incomplete cassette set fail loudly (R2 §5).
- **T2 (right-bead selection — R1 class 5).** Real agent given `gb ready` output **claims
  the correct bead** (real `kd claim`, not `echo`), asserted via bead state — the gap the
  parity doc explicitly DROPs.
- **T3 (decision divergence / no daily-check-in — R1 class 1).** Real agent files a
  `gb decision` only when options genuinely diverge; a non-divergent prompt does **not**
  produce a decision — the "prose-to-the-model" rule enforced nowhere in code.
- **T4 (wrap-up blockers judgment — R1 class 3).** Real agent on a clean run **omits**
  the `blockers` field (does not emit `blockers:"none"`), so the Slack card does not
  stamp ⛔.
- **T5 (`<system-reminder>` comprehension — R1 class 4).** SessionStart prime injection
  → the model's first turn **references the prime-injected assignment** (consumption, not
  just emission) — R3 Hooks proving-ground assertion.
- **T6 (Stop full loop — R1 classes 7/8).** Drive toward Stop with no open decision →
  `gb hook stop-gate` exits 2 → real model responds with `gb decision create` + `gb yield`
  → resolve → next Stop exits 0 → bead closes. One scenario closes the loop claudeless
  cannot decide to create.
- **T7 (CI-tier placement — R4 §3).** The module runs **scheduled-tier, non-blocking**;
  it is **never** a per-PR/main-push hard gate; a live canary detects cassette drift at
  ≤24h cadence.

**Why these six classes for Target:** they are the highest-value, nearest-to-first-class
subset of R1's 12 — all live in the **Hooks** and **agent-choice** territory R3 ranks #1
(smallest gap, exercises the relay→NATS→Slack spine every other feature reuses). The
remaining classes (9 MCP/tool-shape, 6 epic-decomposition, 11 Slack-read, 12 Workflow)
are STRETCH because they ride on later R3 phases.

### Stretch — "full first-class CC-feature coverage + operational maturity"

**Goal:** the complete R3 feature roadmap reaches "first-class + tested," plus the
operational machinery that keeps the suite honest over time.

**Acceptance criteria:**
- **S1 (Subagents/teams — R1 class 12, R3 #2).** Real subagent/team run drives genuine
  SubagentStart/Stop + TaskCreated/Completed + TeammateIdle to NATS→Slack; `SubagentStop`
  output-drop fixed; `workflow_spend` rolls the subagent pool onto the host bead. (Durable
  per-subagent beads remain optional — Q5.)
- **S2 (MCP connect+invoke — R1 class 9, R3 #3).** Real Claude connects to a stub MCP
  server and calls a tool, result in transcript; Playwright navigates+snapshots against a
  live server; `${VAR}` secret resolves; `gardenioChannel=true` variant proves nudge
  delivery + permission round-trip.
- **S3 (Skills/commands — R3 #4).** A canonical shipped skill/command is discovered,
  executed, and its side-effect observed; `symlinkClaudeExtensions` gains unit coverage.
- **S4 (Workflow capstone — R1 class 12, R3 #5).** Real Workflow tool run persists a
  runId + journals; kill-mid-run → respawn → lease-gated rehydrate → `resumeFromRunId`
  returns the completed prefix from cache (zero re-spawn spend), only the tail runs live.
  Gated on resolving Q1c + Q2 (entrypoint rehydrate / `s3-resume` / lease-awareness).
- **S5 (staleness automation).** Cassette versioning-by-model + automated drift detection
  + re-record workflow; LIVE-vs-cassette canary wired as a non-gating tripwire.
- **S6 (observability surface).** Optional wire-level `tool_use` audit + cost/thinking
  observability surfaced to beads-viewer, keyed to native session/run/subagent identity
  (the R1 §5 production-proxy value: native identity instead of header archaeology).

---

## 5. Coexistence policy — claudemore AUGMENTS, never replaces, claudeless (and never duplicates litellm)

This is **binding governance** for the build epic, synthesized from R1 §4 + R4 §3. It has
two halves: coexistence with **claudeless** (the test tier) and coexistence with
**litellm** (the cost/routing plane).

### 5.1 claudeless ↔ claudemore (the two-oracle boundary)

> **claudeless is the fast, deterministic, tag-and-scheduled gate on PLUMBING — the
> literal `gb`/`kd` CLI contract, hook exit-code protocol, and bead-field shapes.
> claudemore is the slow, stochastic, *sampled and non-blocking* gate on REASONING and
> integration. A surface belongs to claudeless if its failure mode is "the contract
> changed"; to claudemore if its failure mode is "the model behaved differently."
> Neither suite may assert the other's territory.**

| Surface class | claudeless | claudemore |
|---|---|---|
| CLI flag/exit-code/bead-field contract (`gb squawk`, `kd claim`, `gb decision create`) | **OWNS** | no |
| Hook exit-code protocol (Stop/PreToolUse gate) | **OWNS** | no |
| Trigger/scheduler engine shape (cron, conditions, dedup) | **OWNS** | no |
| Failure injection (network/auth/rate-limit/out-of-credits) | **OWNS** | no |
| "Does the agent pick the right bead from `gb ready`" | no | **OWNS** |
| Real tool-choice / multi-step reasoning to completion | no | **OWNS** |
| Compaction + post-compaction prime re-injection (behavior) | contract only | **OWNS** behavior |
| Model-drift / tool-use shape regressions | no | **OWNS** |
| End-to-end lifecycle with a real agent | smoke embed only (`agent-crew-lifecycle`) | **OWNS** the real-reasoning version |

**Forbidden anti-patterns (R4 §3):**
1. **Do not delete a claudeless scenario because "claudemore covers it."** A claudemore
   pass proves the model *can* do it once; it does not pin the contract. A surface leaves
   claudeless only if it was on the parity-doc DROP list to begin with.
2. **Do not gate every PR (or main-push) on real-CC.** A flaky *required* gate trains
   operators to "merge anyway," destroying signal. claudemore is **never** a per-PR hard
   gate.
3. **Do not let claudemore re-test pure plumbing.** Asserting `kd claim` exit codes in
   claudemore pays LLM cost for what claudeless does free and creates a second, flakier
   oracle for the same fact.
4. **Do not let claudeless grow into a slow suite chasing model behavior.** Its value is
   its speed; it structurally cannot do model-shaped scenarios.

**CI-tier placement (R4 §3):**

| Tier | claudeless | claudemore |
|---|---|---|
| **main-push** (every commit; smoke subset) | smoke embed only (`agent-crew-lifecycle`) — as-is | **NEVER** (cost + flake) |
| **tag / pre-release** (`/^20/`) | **full suite, hard gate** — its home today | **sampled subset, advisory → soft-gate** only after flake-rate proven |
| **scheduled / nightly** | full suite, hard gate | **full reasoning suite, non-blocking** (or the live cassette canary) |

### 5.2 litellm ↔ claudemore (the cost fence — R1 §4/§6)

> **claudemore augments litellm with CC-semantic awareness, policy, and record/replay.
> It does NOT reimplement litellm's routing or cost layer.**

- **Cost metering stays exclusively in litellm** — the single source of truth
  (`litellm-only-cost-estimation-design.md`), validated end-to-end by
  `test-litellm-cost-tracking.sh` + cost-guard unit tests. **claudemore metering spend
  would be redundant and would conflict with the SoT.**
- **Do not put per-turn / per-subagent real-time spend back** — the speculative
  `spend_forwarder` and coop `response_cost` plumbing were deleted as dead code;
  session-close reconcile via `/spend/tags` is the chosen granularity. Don't reintroduce
  per-turn cost without an explicit human owner.
- **Model-alias routing & per-account virtual-key budgets stay in litellm** — a CC-aware
  proxy adds zero fidelity to a string-swap or a token-count sum.
- **claudemore MAY subsume the thinking-plaintext callback** if it's already parsing the
  stream (R1 §6) — but building claudemore *solely* to relocate that one callback would
  not pay for itself. It is a free rider on the record path, not a justification.

---

## 6. Phased build-epic skeleton

Ordered per R3's roadmap (value × distance-from-first-class), with R4's preconditions as
a hard **Phase 0** gate.

> **Status update (2026-06-14):** operator decision **ka-0000003663 = GO** (Matt Baker).
> This skeleton has been **instantiated** as build epic **ka-0000006108** (`EPIC:
> claudemore build`), parented under the research epic ka-0000006064 and added to the
> deliverables bundle /bundle/ka-0000006676. Phase mapping: Phase 0 = ka-0000000278
> (sub-epic + 5 tasks), Phase 1 = ka-0000005323 (sub-epic + 3 tasks), Phase 2 =
> ka-0000001159 (sub-epic + 3 tasks); Phases 3–7 = ka-0000007725 / ka-0000007322 /
> ka-0000000524 / ka-0000006035 / ka-0000004578 (scoped tasks, decomposed when reached).
> The critical-path `blocks` edges (0→1→2; 2→{3,4,5,6,7}) are wired. **The GO authorizes
> building Phase 0; it does NOT authorize real creds in CI until Phase 0 verifies green.**

### Phase 0 — Guardrails & preconditions (R4 §5) — **gate: nothing real touches CI until these pass**
Effort: **M.** Owner-hints: controller, infra/litellm, platform/network, coop.
- **0.1** Per-CI scoped litellm virtual key with `max_budget` + `budget_duration`, minted
  at namespace-create, deleted at reap (wire `/key/generate`/`/key/update`, reusing
  read-path fields). **Acceptance: a deliberate runaway test confirms litellm 429s at the
  cap** (R4-COST-1, R4-SEC-1).
- **0.2** Explicit per-account model pin in the e2e account bead (`fields.model`→`CLAUDE_MODEL`);
  no reliance on the Sonnet built-in default (R4-DET-4).
- **0.3** claudemore-specific NetworkPolicy egress allowlist (proxy+kbeads+coopmux+kube-dns
  only); dedicated low-priv prefix-scoped IRSA role (not `gardenio-agents-s3`);
  `clusterAdmin.enabled:false`; gVisor on for real-CC pods (R4-ISO-1/2/3/4).
- **0.4** Close F2 (`buildCoopEnv` allowlist); ship redaction F16/F17 on coop recordings +
  bead writes; master key confirmed controller-side only; empty-string vault guard at
  dispatch; assert `COOP_ALLOW_PLAINTEXT_BOOTSTRAP` unset (R4-SEC-2/3/4/5/6).
- **0.5** Codify the §5 coexistence policy as governance docs in `release-pipeline.md`
  (tier matrix, anti-patterns).

### Phase 1 — Proxy MVP: LIVE pass-through + record (the §4 MVP rung)
Effort: **L** (R2: the two hardest pieces are request canonicalization/hashing and
cassette staleness governance; streaming capture is third). Depends on Phase 0.
- **1.1** Thin Messages-API reverse-proxy fronting litellm; account-bead `proxy_url` +
  podspec env wiring; instant rollback runbook (MVP A1/A2).
- **1.2** Cassette schema + S3 store reusing coop `record.rs`/`s3.rs` + redaction; request
  canonicalization/hashing (MVP A3/A4).
- **1.3** Transparency proof: spend pipeline identical vs. litellm-direct baseline (MVP A1).

### Phase 2 — Hooks proving ground + REPLAY tier (R3 #1, the §4 Target rung)
Effort: **S** (Hooks plumbing is `have`; gap is a real model in the loop) + **M** for the
REPLAY tier. Depends on Phase 1.
- **2.1** REPLAY mode: serve cassettes $0/deterministic; `fail_on_miss`; live-vs-cassette
  canary (Target T1/T7).
- **2.2** Hooks e2e: SessionStart prime consumption (T5); Stop full loop (T6);
  PreToolUse/PostToolUse NATS events.
- **2.3** Agent-choice e2e: right-bead selection (T2); decision divergence (T3); wrap-up
  blockers judgment (T4). **This rung is where the R1 failure classes become live assertions.**

### Phase 3 — Subagents / teams (R3 #2)
Effort: **M** (mostly e2e authoring + the `SubagentStop` output-drop fix). Depends on Phase 2.
- Stretch S1 acceptance.

### Phase 4 — MCP servers (R3 #3)
Effort: **M** (needs a stub server + Helm/CI wiring + `gardenioChannel=true` variant; flip
the `!mockMode` gate). Depends on Phase 2. Parallelizable with Phase 3.
- Stretch S2 acceptance.

### Phase 5 — Slash commands + Skills (R3 #4)
Effort: **M** (cheap unit win on `symlinkClaudeExtensions` is independent; the claudemore
half needs a canonical shipped skill target). Depends on Phase 2. Parallelizable.
- Stretch S3 acceptance.

### Phase 6 — Workflow tool capstone (R3 #5)
Effort: **L** (furthest from first-class: entrypoint unwired, `s3-resume` nonexistent,
zero controller awareness, lease unenforced, much net-new). Depends on Phase 2 + resolves
Q1c/Q2 first.
- Stretch S4 acceptance.

### Phase 7 — Operational maturity
Effort: **M.** Staleness automation (S5) + beads-viewer observability surface (S6).

**Critical path:** Phase 0 → 1 → 2 delivers MVP+Target (the decision-grade value). Phases
3/4/5 are parallelizable mid-tier rungs. Phase 6 (Workflow) is the capstone whose
first-class status depends on the Q1c/Q2 resolution. Phase 7 is ongoing hygiene.

---

## 7. Operator decision checkpoint

Per the epic exit criteria and ka-0000001264, this synthesis was paired with a
`gb decision` offering **go / refine / defer**. The research is converged and decision-grade;
the recommendation was **go** (start the build epic at Phase 0).

> **Resolved (2026-06-14): operator chose `go`** (decision ka-0000003663, Matt Baker).
> The build epic ka-0000006108 is instantiated per §6. The guardrail caveat below is now
> binding governance on that epic: Phase 0 (ka-0000000278) is the hard gate.

**The checkpoint the operator should weigh (R4 §5):** the five must-build guardrails are
**Phase 0 of the build epic**, not preconditions to *starting* it — it is safe to *build
the de-risking*; it is not yet safe to point real creds at CI until Phase 0 verifies
(budget 429s at cap; model pinned; egress blocks `0.0.0.0/0`; F2+redaction shipped; master
key absent from CI; claudemore non-blocking/sampled). A `go` authorizes scoping and
building the epic with Phase 0 as its gate; it does **not** authorize real creds in CI
before Phase 0 is verified-green.

---

## 8. Sources

- R1: `gardenio/docs/research/claudemore-r1-why-claudemore.md` (ka-0000003209)
- R2: `gardenio/docs/research/claudemore-architecture-survey.md` (ka-0000005113, MR !4272)
- R3: `gardenio/docs/research/claudemore-r3-cc-feature-inventory.md` (ka-0000003637, MR !4274)
- R4: `gardenio/docs/research/claudemore-r4-derisking.md` (ka-0000004533)
- Epic ka-0000006064; scope decision ka-0000007358; shared bundle ka-0000006676.
