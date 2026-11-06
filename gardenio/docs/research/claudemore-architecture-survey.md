# claudemore architecture survey — what is "an LLM proxy that talks to Claude Code"?

**Bead:** ka-0000005113 (R2 of EPIC ka-0000006064 — claudemore research).
**Author:** `gardenio-r2-architecture-survey-fre`.
**Operator:** Matt Baker (he/him), via Slack scope decision ka-0000007358.
**Status:** research only — no shipping code, no build started.

> This report enumerates and evaluates candidate architectures for **claudemore**,
> now scoped (operator decision ka-0000007358) as a **real production LLM proxy that
> speaks Claude Code**, sitting alongside / in front of litellm — with deterministic
> e2e as **one** use case, not the whole point. It ends with a recommended
> production-proxy shape and its e2e tier, and an explicit coexistence stance with
> litellm. The recommendation is decision-grade input for R5 synthesis, not a
> go-ahead to build.

---

## 0. The semantic question, resolved first

The phrase "an LLM proxy that talks to Claude Code" is genuinely ambiguous, and the
ambiguity is the single most important thing to nail before comparing designs.

There are two readings, and they point at **opposite ends of the agent**:

| Reading | Where it sits | What it "talks to" |
|---|---|---|
| **(i) Downstream LLM-path proxy** | Between the `claude` binary and Anthropic | Claude Code talks *to* it (it is the LLM endpoint). This is exactly what **litellm** already is. |
| **(ii) Upstream CC orchestrator** | In front of the `claude` binary | It talks *to* Claude Code — it drives the agent, feeds it prompts, observes its behavior. |

litellm is unambiguously reading (i): the `claude` binary points `ANTHROPIC_BASE_URL`
at `http://gardenio-litellm:4000`, sends `Authorization: Bearer <OAuth>` +
`x-litellm-api-key`, and litellm reverse-proxies to `api.anthropic.com` while tracking
spend and routing model aliases (`gardenio/docs/litellm-production-cutover.md`,
`controller/.../podspec.go`).

**The decisive grounding fact** (from the cross-cutting wire-visibility analysis below):
the Claude-Code features we want first-class support for are **overwhelmingly
client-side** — they live *inside* the `claude` binary and are *invisible* on the
Anthropic Messages wire. A reading-(i) proxy that sits on the LLM path can see the
`tool_use` block *names and arguments* a turn produces, but it cannot see hooks fire,
cannot see slash-command routing, cannot see the Workflow runtime spawn subagents,
cannot see MCP protocol traffic, and cannot see hook outputs (prime context, mail,
stop-gate decisions). Those happen in the binary and over local FIFO/NATS, never on
the HTTPS request to Anthropic.

That fact is what makes the architecture choice non-obvious. If you want claudemore to
genuinely "speak Claude Code" — to be aware of and able to drive/observe CC semantics —
a pure LLM-path proxy structurally **cannot** do it. It can only observe the thin
wire-level shadow (tool_use names) of a much richer client-side reality.

### Operator scope (ka-0000007358), taken literally

> claudemore = a real **production** LLM proxy that **speaks Claude Code**, sitting
> alongside/in front of litellm; **e2e is ONE use case, not the whole point**.

Reconciling the scope with the wire-visibility fact yields the framing this report
uses: claudemore is a **production proxy on the LLM path** (so it coexists with
litellm and rides the existing `ANTHROPIC_BASE_URL` plumbing), whose distinguishing
value over plain litellm is being **Claude-Code-semantics-aware at the wire level**
(it parses Anthropic Messages, understands the *shape* of CC's tool_use vocabulary,
and can **record** complete CC↔LLM exchanges). The "speaks Claude Code" claim is
honored at the wire layer; the richer client-side semantics are reached **not** by the
proxy but by reusing **coop**, which already fronts the real `claude` subprocess and
already captures hooks/thinking/Workflow to S3 + NATS. Production traffic flows
through the proxy; deterministic e2e is the **replay** of what the proxy recorded.

---

## 1. The four candidate architectures

Each was evaluated independently and in depth against the full R2 rubric (production
traffic path, where CC semantics live, e2e use, gardenio integration, litellm
coexistence, determinism/flake, prod + CI token cost, secret/isolation, operability,
effort). Summaries follow; the comparison matrix is §3.

### (a) CC-semantics-aware proxy on the LLM path, beside/in front of litellm

A Messages-API-compatible proxy exposed as `ANTHROPIC_BASE_URL`, sitting
`CC → claudemore → litellm → Anthropic`. It unmarshals the Anthropic Messages
request/response, observes `tool_use` blocks (Workflow, Skill, Agent, MCP-defined
tools) at the wire level, and forwards everything to litellm unchanged.

- **What it can see:** outbound system prompt + conversation history; inbound
  `tool_use` block *names and arguments*; token counts.
- **What it cannot see:** hooks firing, hook outputs, slash-command routing, the
  Workflow runtime's internal execution, MCP protocol traffic, subagent execution.
  These are client-side (§2).
- **Determinism:** **adds none.** e2e determinism today comes from claudeless mocking
  the *binary*; a passive observer on the LLM path changes nothing. To observe
  anything in e2e it must forward to a *real* LLM, which *adds* cost and flake.
- **litellm coexistence:** front (sidecar) is feasible; replacement is not (would
  require re-implementing litellm's spend pipeline — see §4).
- **Effort:** **M.** Mostly an HTTP pass-through with Messages parsing.
- **Verdict:** Cheap to build, but its standalone value is **observability/audit of
  tool_use at the wire**, which is a weak win — litellm already owns cost/spend, and
  the semantics we care about aren't on the wire. **Necessary as a substrate, not
  sufficient as the whole answer.**

### (b) Claude Code as a controllable subprocess/sidecar that claudemore fronts

claudemore is a pure **orchestrator** (not an LLM endpoint). It PTY-spawns the real
`claude` binary, feeds it prompts, and captures the *complete* picture — hooks,
Workflow, MCP, subagents, skills — because the binary is real. This is reading (ii),
and it is **essentially what coop already does** (`coop/crates/cli/src/s3.rs` +
the PTY spawn path + NATS thinking-forward).

- **CC semantics:** **exact.** Nothing is approximated; every CC feature works because
  it *is* Claude Code. Lowest possible semantic gap.
- **litellm coexistence:** litellm stays exactly where it is — *downstream* of the CC
  subprocess. The subprocess inherits `ANTHROPIC_BASE_URL=litellm:4000`. claudemore
  front-doors orchestration, not the LLM path. **Zero disruption to spend tracking.**
- **e2e:** real CC against a real (or test) LLM → real tokens, stochastic.
  Determinism for e2e still has to come from somewhere (claudeless for the binary, or
  a replay tier for the LLM — candidate c).
- **Effort:** **M–L,** but mostly **already built in coop.** The hard parts
  (PTY/signal/reap, concurrent subagent PTYs, NATS event ordering) are coop's existing
  burden, not net-new.
- **Verdict:** This is the **only candidate that truly "speaks Claude Code"** in the
  rich sense — and gardenio *already owns the machinery* (coop). The insight is that
  claudemore-as-orchestrator should be understood as **"use/extend coop," not "build a
  new orchestrator."** Its weakness is that, alone, it is non-deterministic and
  token-costly for CI.

### (c) Record/replay tier on top of the production proxy

Built **on** candidate (a)'s proxy. In **RECORD** mode (production), the proxy captures
real CC↔LLM exchanges (Messages requests, streamed responses, tool_use, thinking) to
**cassettes** (reusing coop's S3 recordings + event-log + secret-redaction patterns,
`coop/crates/cli/src/record.rs`, `s3.rs`). In **REPLAY** mode (CI), it serves those
cassettes deterministically with **zero real tokens** — while the **real `claude`
binary** runs against it, so hooks, Workflow, tool dispatch, and MCP all execute for
real. Only the LLM responses are canned.

- **The key contrast with claudeless:** claudeless fakes the **agent** (the binary);
  record/replay fakes only the **LLM**, keeping the agent real. This closes the
  fidelity gap claudeless structurally cannot — you exercise the *actual* CC client
  with *deterministic* LLM responses.
- **Determinism:** strong in REPLAY (same request hash → byte-identical response).
  Residual flake sources: request-hash drift (prompt/state changes the request),
  missing cassettes, streaming-rate sensitivity, and the fact that hooks still call the
  live beads daemon (orthogonal state).
- **Cost:** prod adds ~nothing (pass-through + cassette write); **CI ≈ $0** in REPLAY.
- **litellm coexistence:** RECORD fronts litellm; REPLAY bypasses both litellm and
  Anthropic entirely (the cost win). spend_sync sees normal traffic in RECORD, nothing
  in REPLAY.
- **Effort:** **M–L.** Hardest pieces: cassette schema + streaming capture, request
  canonicalization/hashing, and cassette **staleness/refresh** governance.
- **Verdict:** This is the **deterministic e2e tier** the epic is implicitly asking
  for — "real Claude Code in the loop" without per-run token cost. It is a *tier on
  top of (a)*, not a standalone architecture.

### (d) Hybrid — one binary, mode-switched

A single CC-semantics-aware proxy on the LLM path with a pluggable backend:
**LIVE** (forward to litellm→Anthropic + record), **REPLAY** (serve cassettes,
$0/deterministic), and optionally **ORCHESTRATE** (drive a real CC subprocess à la b).
One codebase, mode selected per-agent/per-project via bead fields and a query param on
`ANTHROPIC_BASE_URL`.

- **Coherence check (asked honestly):** LIVE+RECORD and REPLAY are genuinely the same
  object viewed in two directions — record what flows through, replay it later. That
  union is **coherent** (it is exactly how HTTP VCR / cassette libraries work). The
  **ORCHESTRATE** mode is the part that risks kitchen-sink: it drags coop's entire
  PTY/FIFO/NATS surface into the proxy binary. The honest read is that ORCHESTRATE
  should **stay in coop** and be *composed with* (not absorbed into) the proxy.
- **Effort:** **L** for LIVE+REPLAY; **XL** if ORCHESTRATE is folded in.
- **Verdict:** The LIVE+REPLAY hybrid = candidate (a) + candidate (c) as one binary,
  which is the natural and correct packaging. The full three-mode hybrid is
  over-engineered; the orchestration concern is already solved by coop.

---

## 2. Where Claude-Code semantics actually live (wire-visibility table)

This is the crux finding. For each first-class CC feature, is it client-side (in the
`claude` binary / coop) or visible on the Anthropic Messages wire?

| Feature | Side | Visible on Messages wire? | Implication for a wire-level proxy |
|---|---|---|---|
| **Workflow tool** (and built-in tools: Bash/Read/Write/Edit) | client-side | **No** — only the *enclosing* `tool_use` block name is seen; the Workflow runtime that spawns subagents executes inside the binary | A proxy sees "a tool named Workflow was called" but not what it did. Cannot drive or assert on Workflow semantics. |
| **Slash commands** (`/review`, `/run`, …) | client-side | **No** — parsed in the CC REPL, never a tool_use | Completely invisible. Proxy cannot observe or test. |
| **Skills** (deep-research, verify, code-review) | client-side | **Partial** — invoked via the `Skill` tool, so the *invocation* name+args are wire-visible; the *implementation* is internal | Proxy can log that a skill was invoked, not what it did. |
| **Hooks** (SessionStart→prime, PreToolUse, Stop→stop-gate) | client-side | **No** — run by the binary over FIFO/shell; outputs go to local files / NATS | A proxy cannot see hooks fire or their effects. coop *can* (it relays them). |
| **MCP servers** | client-side | **No** (protocol) / **Partial** (tools) — MCP runs as local child processes; only the `tool_use` blocks MCP *defines* appear on the wire | Proxy sees MCP-defined tool calls as generic tool_use; the MCP protocol itself is invisible. |
| **Subagents** (Agent tool, teammates) | mixed | **Partial** — the `Agent` tool_use is wire-visible; subagent *execution* is separate and only visible if those subagent LLM calls *also* route through the proxy | Proxy sees a subagent was requested; sees its work only if all its calls flow through claudemore too. |

**Takeaway:** a wire-level proxy is a good **recorder of LLM exchanges** and a weak
**observer of CC semantics**. The rich semantics are reachable only by being the thing
that *runs* the binary — which is coop. This is why the recommendation pairs a thin
proxy (for the LLM path + record/replay) with coop (for the real CC client), rather
than trying to make one wire-level proxy do everything.

---

## 3. Comparison matrix

| Dimension | (a) wire proxy | (b) CC-subprocess (≈coop) | (c) record/replay | (d) hybrid (a+c) |
|---|---|---|---|---|
| **Truly "speaks CC"** | wire-shadow only | **yes, fully** | wire + real binary | wire + real binary |
| **Prod traffic path** | CC→**cm**→litellm→Anthropic | CC(sub)→litellm→Anthropic; cm orchestrates | CC→**cm**(record)→litellm→Anthropic | LIVE: as (a)+record |
| **CC semantics seen** | tool_use names only | **everything** (real binary) | everything (real binary) + LLM recorded | everything + LLM recorded |
| **e2e determinism** | none added | none (real LLM) / via claudeless | **strong (REPLAY)** | **strong (REPLAY)** |
| **Prod token cost** | pass-through (≈0 added) | pass-through (≈0 added) | pass-through (≈0 added) | pass-through (≈0 added) |
| **CI token cost** | real LLM if observing (~$60/mo) | real LLM (stochastic) | **≈ $0 (REPLAY)** | **≈ $0 (REPLAY)** |
| **litellm coexistence** | front (sidecar); replace infeasible | downstream, untouched | front (record) / bypass (replay) | front / bypass |
| **Reuses existing code** | little | **coop (most of it)** | coop record + S3 + redaction | coop + new proxy |
| **gardenio integration** | account-bead `proxy_url`, podspec env | coop already integrated | + cassette store, mode fields | + mode fields |
| **Effort** | M | M–L (mostly built) | M–L | L (a+c) / XL (+b) |
| **Standalone verdict** | substrate, not sufficient | the real-CC engine (=coop) | the e2e tier | best *packaging* of a+c |

---

## 4. litellm coexistence stance (explicit)

**Recommendation: COEXIST + FRONT. Never replace.**

litellm is the **single, authoritative cost pipeline** and must remain so
(`gardenio/docs/litellm-only-cost-estimation-design.md`). Cost enters the system through
exactly two controller writers, both of which query litellm directly:

- **spend_sync.go** (5-min loop): `GET /key/info` + `GET /spend/logs` → writes
  `total_cost_usd`/`daily_cost_usd`/`weekly_cost_usd` onto **account beads**.
- **reconcile_session_spend.go** (per session): snapshots `GET /spend/tags`
  (matched on `agent:<name>`) at spawn, reconciles delta at close → writes
  `total_cost_usd` + `litellm_spend_close` onto **agent beads** and increments the
  `gardenio_agent_spend_usd_total` Prometheus counter.

Replacing litellm means re-implementing virtual keys, per-account budgets, model-alias
routing, OAuth forwarding, and this entire spend pipeline — a large, high-risk rebuild
for no benefit. **Out of scope, full stop.**

Under the recommended shape:

- **LIVE/RECORD:** claudemore fronts litellm (`CC → claudemore → litellm → Anthropic`).
  All spend tracking is **unchanged** — litellm still sees every request, still tags by
  `agent:<name>`, spend_sync and reconcile keep working untouched. claudemore is a
  transparent recording reverse-proxy; it stores cassette metadata but never owns cost.
- **REPLAY (CI):** claudemore is the *only* endpoint the agent can reach; litellm and
  Anthropic are bypassed entirely. No tokens, no spend — which is correct, because a
  replayed session has no real cost. spend_sync simply finds nothing to reconcile.
- **Fallback:** if claudemore is unreachable in prod, agents fall back to
  `ANTHROPIC_BASE_URL=litellm:4000` directly (revert the account-bead `proxy_url`
  field; the credential sync loop picks it up within ~60s per the cutover runbook).
  Rollback is instant and reversible — the same property the litellm cutover relies on.

---

## 5. e2e tier (the "one use case")

The epic's animating goal — "prove gardenio behaves with **real Claude Code** in the
loop" — is delivered by the **record/replay tier (candidate c)** running on top of the
production proxy:

1. **RECORD** real production (or a curated golden session) → cassettes in S3,
   reusing coop's recording + redaction + event-log machinery.
2. **REPLAY** in CI: the **real `claude` binary** (driven by coop, exactly as in prod)
   runs against claudemore-in-replay. Hooks fire for real, Workflow runs for real, tool
   dispatch happens for real, MCP servers run for real — only the LLM responses are
   canned. **$0 tokens, deterministic, real CC client behavior.**
3. **claudeless stays** as the fast, $0, binary-level mock for critical-path plumbing
   regression (the 54 scenarios). claudemore **complements** it — claudeless proves
   "gardenio plumbing behaves against fake reasoning"; claudemore-replay proves "the real
   CC client behaves against deterministic reasoning." Two tiers, different fidelity,
   neither replaces the other.

**The cheapest path to deterministic e2e with real CC behavior is record/replay**, and
it is cheap precisely because the expensive/stochastic part (the LLM) is the *only*
thing canned. This is the structural advantage claudeless cannot match: claudeless can
never run the real `claude` binary, so it can never validate real hook/Workflow/MCP
behavior.

**Top determinism risk to design against:** cassette **staleness**. A stale cassette
silently becomes "truth" and tests pass against behavior that no longer matches prod.
Mitigations: version cassettes by model tag; periodic LIVE-vs-cassette smoke to detect
drift; explicit `fail_on_miss` policy so an incomplete cassette set fails loudly rather
than silently falling back to LIVE.

---

## 6. Recommendation

**Recommended production-proxy shape: candidate (d), scoped to (a) + (c) — i.e. a
single CC-semantics-aware recording proxy on the LLM path with LIVE/RECORD and REPLAY
modes — fronting (never replacing) litellm, and composed with coop for the real-CC
client (candidate b's value reached via reuse, not absorption).**

Concretely:

1. **claudemore = a thin Messages-API recording reverse-proxy** exposed as
   `ANTHROPIC_BASE_URL`, sitting in front of litellm. In LIVE it forwards + records; in
   REPLAY it serves cassettes. This is candidates (a)+(c) packaged as one binary —
   the coherent core of (d).
2. **It does NOT absorb orchestration.** The "drive a real CC subprocess" capability
   (candidate b) **already exists in coop** (PTY spawn, S3 persistence, NATS thinking,
   Workflow resume). claudemore *composes with* coop — coop runs the real `claude`, the
   proxy records/replays its LLM traffic. Folding coop into the proxy is the
   over-engineering trap; avoid it.
3. **litellm is fronted, never replaced.** Spend tracking, virtual keys, model aliases,
   OAuth forwarding all stay in litellm, untouched (§4).
4. **The e2e tier is REPLAY** (§5): real CC binary + canned LLM = deterministic, $0,
   high-fidelity. claudeless remains the fast plumbing-regression tier alongside it.

**Why not the others standalone:**
- (a) alone: too weak — wire observability of tool_use names isn't worth a new
  service when litellm already owns cost and the real semantics aren't on the wire.
- (b) alone: this is just coop; it gives no determinism and costs real tokens in CI.
  Its value is real, but it's *already in gardenio* and should be reused, not rebuilt.
- (c) alone: it *is* the e2e tier, but it's a tier on (a), not a free-standing arch.
- full (d) with ORCHESTRATE folded in: XL effort and kitchen-sink; the orchestration
  it would duplicate is already coop's job.

**Effort:** **L** for the LIVE+REPLAY proxy (the recommended scope). The two hardest
pieces are (1) **request canonicalization/hashing** (too loose → cassette collisions
and wrong replays; too tight → cassettes expire instantly — get this right *before*
recording at scale) and (2) **cassette staleness governance** (versioning by model +
drift detection + `fail_on_miss`). Streaming capture/replay fidelity is the third.

---

## 7. What this hands to R5 (synthesis)

- **Architecture of record:** thin recording proxy (a+c) on the LLM path + coop for
  real CC + litellm fronted. R5 should scope the **build epic** around this, *not* an
  e2e-only harness (per the operator scope decision).
- **Goal ladder seed** (R5 to finalize):
  - **MVP:** LIVE pass-through proxy fronting litellm with cassette **record**; prove
    transparent (spend tracking unaffected) and reversible (fallback to litellm).
  - **Target:** REPLAY mode + deterministic CI tier running the real `claude` binary
    against cassettes at $0; `fail_on_miss` + coverage reporting.
  - **Stretch:** cassette staleness/drift automation; per-project/per-agent mode
    rollout; optional wire-level tool_use audit surface feeding beads-viewer.
- **Coexistence stance for the build epic:** COEXIST+FRONT with litellm; claudeless
  stays as the complementary fast tier (neither replaces the other).
- **Open de-risking items** (hand to R4 if not already covered): request-hash design,
  cassette secret-redaction, cassette storage growth, streaming-rate fidelity.

---

## 8. Evidence / files inspected

- `gardenio/docs/litellm-production-cutover.md` — litellm prod proxy, `ANTHROPIC_BASE_URL`,
  virtual keys, spend_sync, reversible cutover/fallback.
- `gardenio/docs/litellm-only-cost-estimation-design.md` — the two authoritative cost
  writers (spend_sync.go, reconcile_session_spend.go), tag-based attribution.
- `gardenio/docs/research/claudeless-critical-path-parity.md` — claudeless = binary-level
  TOML mock; gardenio critical-path inventory.
- `coop/crates/cli/src/s3.rs` — S3 persistence (transcripts, recordings, event logs),
  `S3Storage` trait, session meta — the cassette substrate.
- `coop/crates/cli/src/record.rs` — recording + secret redaction patterns.
- `coop/crates/cli/src/driver/claude/parse.rs`, `prompt.rs` — CC Messages parsing,
  tool_use/thinking/stop_reason extraction.
- `coop/CLAUDE.md` — coop PTY spawn, hook FIFO, NATS thinking-forward (candidate b).
- `gardenio/controller/internal/beadsapi/accounts.go` — account-bead `BaseURL`/`LiteLLMKey`
  (where a `proxy_url` field would live).
- `gardenio/controller/cmd/controller/podspec.go` — controller injects `ANTHROPIC_BASE_URL`
  + `ANTHROPIC_CUSTOM_HEADERS` into agent pods.
- `gardenio/images/agent/hooks/*.sh` — SessionStart/PreToolUse/Stop hooks (client-side,
  invisible on the LLM wire).
