# The "First Mate" Fleet Role — Design Proposal

> **Status:** PROPOSAL for operator review (do **not** build yet).
> **Task:** ka-0000006062 (WS-C of epic ka-0000002254).
> **Operator:** Matt Baker (`oidc:matthew.baker@acme.example`).
> **Origin:** Slack thread (2026-06-11) — *"add a 'first mate' role that keeps
> gardeners' plans on track and manages succession / retirement / creation."*
> **Reads on:** [`gardener-resume-observability-and-succession-plan.md`](./gardener-resume-observability-and-succession-plan.md) §2 (succession),
> [`gardener-resume-design-2026-06-09.md`](./gardener-resume-design-2026-06-09.md) (master gate + resumer watchdog),
> [`gardener-meta-scoring-prompt.md`](./gardener-meta-scoring-prompt.md) +
> [`gardener-effectiveness-rubric.md`](./gardener-effectiveness-rubric.md) (the scorer),
> [`gardener-resumer-watchdog-prompt.md`](./gardener-resumer-watchdog-prompt.md) (the watchdog).

---

## 0. TL;DR

The fleet today has three **reflexes** — fast, narrow, mechanical:

1. **Engine master-gate** (merged, `gardener_evaluator.go`): on a *premature* stop,
   re-arm-or-escalate so no gardener silently abandons an unmet `terminal_goal`.
2. **Resumer watchdog** (meta-gardener, ~4h): revive gardeners the engine can't see
   from inside its own loop — disabled, crashed, legacy-budget-closed.
3. **Meta-scorer** (meta-gardener, 12h): grade D1–D6 effectiveness and propose
   tuning / stop of *weak* gardeners via operator decisions.

All three answer **"is this gardener running, and running efficiently?"** None
answers **"is the gardener's PLAN actually converging on its goal — or is it alive,
busy, and going nowhere?"** That is the gap. A gardener can be perfectly live
(watchdog happy) and produce a progress event every run (scorer's D1 high) while
**thrashing on the wrong sub-problem, drifting off the documented plan, or sitting
on a phase that finished a day ago and should have handed the baton.**

The **first mate** is the fleet's first **deliberative** supervisor (vs. the
reflexes). It reads each gardener's *plan of record* (terminal_goal + epic child
structure + any plan doc/formula) and judges **trajectory**, not activity. From
that judgment it drives the three lifecycle actions Matt asked for:

- **Plan-tracking** — render a per-gardener *trajectory verdict* (on-track /
  blocked / thrashing / drifting / phase-complete / effort-complete).
- **Succession** — when a phase is genuinely done, instantiate the heir from the
  declarative `successor` block (§2) with provenance, and retire the predecessor:
  research → impl → improvement → regression-checking, self-propagating.
- **Retirement / creation** — graceful, goal-driven decommission of finished
  gardeners; operator-gated creation of new ones.

**Recommendation:** build the first mate as a **third meta-gardener**
("gardener-of-gardeners"), agent-side, sibling to the watchdog and scorer under epic
ka-0000007687 — the same maturation path the master gate took (prove as an agent,
pull mechanical bits engine-side later). Zero new engine authority for v0. All
mutating actions (create / re-plan / retire-and-replace) go through `gb decision`.

**Model default (operator directive, Slack 2026-06-11):** ship mates pinned to
**Haiku** out of the box as a strong default — it keeps them in-scope (a smaller
model is less prone to wandering off the narrow supervisory remit) and fast/cheap
(mates run often, fleet-wide). **Caveat the operator flagged: we have never run a
full agent session on Haiku before** — only narrow sub-tasks (namegen, health
probes, bridge summaries). So Haiku-as-mate is a *validated default*, not a blind
config flip: phase 0 of the build is a real shake-out (§7.0, §9). The blast-radius
math makes this safe to try — a mate has **zero autonomous destructive authority**,
so a weak Haiku run yields a *proposal the operator declines*, never damage.

---

## 1. On-track signals — what tells us a plan is converging vs. not

The task names four seed signals (progress beads closed/run, decision backlog,
escalation age, child-task velocity). The critical refinement: the scorer's **D1
progress-yield** already measures *whether runs produce progress events*. That is
**activity**, not **convergence**. A gardener can score a progress event every wake
(open a child, dispatch crew, churn an MR) and still not move toward the
`terminal_goal`. The first mate measures **convergence on the goal**, which the
scorer deliberately does not.

### 1.1 The signal set

| # | Signal | What it detects | Source data |
|---|---|---|---|
| S1 | **Burndown trajectory / ETA** | Is the weighted open-child count trending to zero? Extrapolated ETA inside the plan horizon? | `kd list --parent <epic>` counts now vs. recorded-last-run; closure slope |
| S2 | **Net-velocity sign** | *Thrashing* = children churn (open↔close) with net-zero or negative burndown; or new children created faster than closed (scope creep vs. healthy decomposition) | child create/close deltas over K windows |
| S3 | **Plan-vs-reality drift** | Do the live children still map to the documented plan (terminal_goal phases / plan-of-record doc / formula)? Orphan children with no plan lineage, or plan milestones with no bead = drift | epic child set vs. terminal_goal text + linked plan doc/formula |
| S4 | **Fruitless-resume slope** | `resumes_without_progress` *climbing* (engine field) is the leading indicator that the gardener cannot execute its current plan — **before** the engine escalates at budget | `GardenerSpec.ResumesWithoutProgress` / `LastResumeReason` (already persisted) |
| S5 | **Decision backlog + escalation age** | Progress *gated on a human* (open/stale decisions in subtree) → **blocked**, not thrashing — different remedy (ping/unblock vs. re-plan) | open decisions in epic subtree; `awaiting=human` age |
| S6 | **Crew-regression loop** | Same crew task re-dispatched N× without closing, or crew MRs that never merge → the *plan* is asking crew to do the impossible | crew dispatched in subtree; re-dispatch count; MR merge state |
| S7 | **Phase-exit criteria met** | The current phase's `terminal_goal` is satisfied (the succession trigger) | last run `goal_satisfied=="yes"` OR explicit exit-criteria check |

S1/S2/S3/S7 are the genuinely new, plan-level signals. S4 is the clean seam to the
engine (the engine *counts* fruitless resumes; the first mate *diagnoses why* and
acts before budget exhaustion). S5/S6 overlap the scorer's D5/D4 in *data* but not
in *question* — the scorer asks "does this gardener over-escalate / leak crew"; the
first mate asks "is the plan blocked / impossible" (see §3 for the non-overlap
contract).

### 1.2 The Trajectory Verdict (per gardener, per first-mate run)

The signals collapse into **one verdict** that maps cleanly to an action:

```text
ON-TRACK       S1 ETA within horizon, S2 net-positive, S3 aligned,
               S4 flat, S5 fresh         → log only, no action
BLOCKED        S5 open/stale decision OR external dep gating progress
                                          → ping operator / propose unblock
THRASHING      S2 net-zero over K windows OR S4 rising OR S6 regression loop
                                          → propose RE-PLAN (operator decision)
DRIFTING       S3 children diverging from documented plan / scope creep
                                          → propose RE-SCOPE or split (decision)
PHASE-COMPLETE S7 met AND a successor is defined or warranted
                                          → SUCCESSION (instantiate heir, retire pred)
EFFORT-COMPLETE S7 met, no successor, no open subtree work
                                          → graceful RETIRE (notify, no gate)
```

This verdict→action→touchpoint table is the spine of the role; everything else
serves it.

---

## 2. Where the first mate lives — three placements, with tradeoffs

### (A) Gardener-of-gardeners — a third meta-gardener  ✅ **recommended for v0**

A `type=trigger`, `role=gardener` bead, sibling to the watchdog and scorer under
epic ka-0000007687, event-driven with a cron backstop (see §6).

- **Pros.** Reuses the entire gardener primitive — wake (cron + bead events),
  `run_log`, stop-spec, `gb decision`/`gb yield`, self-block — so **zero new engine
  code** and **zero new engine authority**. The verdict in §1.2 is inherently an
  LLM reasoning task (read a plan, judge trajectory); a gardener-agent is the right
  substrate. Ships in days. Composes naturally with the two existing
  meta-gardeners, which already share the same epic and shape.
- **Cons.** Polling/event latency (not instantaneous — acceptable, plans move in
  hours/days). Adds a third meta-gardener (loop-hygiene + cost). Overlap risk with
  the scorer if scope isn't pinned (addressed in §3). "Who watches the first mate?"
  → it is itself a gardener, so the master-gate resumes it and the watchdog revives
  it. Clean recursion, no special-casing.

### (B) Engine-side — extend `gardener_evaluator.go`

Track plan-trajectory mechanically in the fire path.

- **Pros.** Real-time, no polling.
- **Cons.** The engine has **no LLM** — it cannot "read a plan and judge whether
  reality is converging." It can only threshold mechanical counters, which reduces
  the rich judgment (thrashing vs. healthy decomposition; drift vs. legitimate
  re-scope) to brittle rules. Worse, plan-level lifecycle (create / retire / re-plan
  gardeners) engine-side means **granting the engine "create/destroy a gardener"
  authority** — exactly the blast-radius concern §2d of the succession plan flags
  and leans away from. Wrong tool for the deliberative part.

### (C) A new first-class role type — distinct controller subsystem

A non-gardener role with its own scheduler integration and lifecycle.

- **Pros.** Cleanest conceptual separation; room for bespoke UI.
- **Cons.** Largest build — new role type, scheduler wiring, prompt plumbing,
  tests, docs — *before* we know the exact signal set or cadence. Premature; the
  gardener primitive already provides everything v0 needs.

### Verdict & maturation path

**Build (A).** It mirrors the pattern the fleet has already validated twice: the
watchdog and scorer began as meta-gardeners, and the master-gate logic was later
pulled engine-side **only after** the agent version proved the rules. The first
mate follows the same arc:

```
v0  agent-side meta-gardener        → prove the verdict rules + succession on real gardeners
v1  pull S1/S2/S4 mechanical       → engine emits a "trajectory" gauge / off-track label
    signal-gathering engine-side      the first mate reads (cheaper, faster), keeps the
                                       deliberation (re-plan / succession / retire) agent-side
v2  (only if warranted) promote    → engine-side auto-succession within an
    proven, low-risk transitions      operator-pre-approved pipeline (§4, §5)
```

The deliberation never leaves the agent; only the *mechanical sensing* migrates as
it's proven — the same split the master gate uses (engine counts resumes; an agent
decides what a stuck gardener's goal should become).

---

## 3. Composition with the existing reflexes — the non-overlap contract

The first mate is **deliberation**; the other three are **reflexes**. They divide
along the question each one answers, not the data each one reads.

| Question answered | Owner | Cadence | Autonomous authority | Gated authority |
|---|---|---|---|---|
| Is the gardener *running* on a premature stop? | **engine master-gate** | realtime (fire path) | re-arm (refill budget) | flag `gardener-stuck-escalate` at budget |
| Is a stopped gardener *recoverable* (disabled/crashed/legacy-closed)? | **resumer watchdog** | ~4h | re-enable + refill + re-fire | redefine-goal / stop (decision) |
| Is the gardener *efficient* (compute / throughput / hygiene)? | **meta-scorer** | 12h | comment (A/B) / file tune-task (C) | config edit (D) / stop (F) via decision |
| Is the *plan converging* on the goal? Hand off / re-plan / retire? | **first mate** | per-milestone / event | observe, report verdict, **ping** operator | succession / re-plan / retire-and-replace (decision) |

### Explicit non-overlap rules (the seams)

1. **First mate never re-arms or revives a stopped gardener.** Liveness is the
   engine's and watchdog's job. If the first mate finds a *dead* gardener whose plan
   is on-track, it defers to the watchdog (or simply notes it; the watchdog runs
   more often). The first mate acts on gardeners that are **alive but off-trajectory**
   — precisely the blind spot of liveness reflexes.
2. **First mate never grades efficiency or tunes cron/cooldown/concurrency.** That
   is the scorer's lane (D1–D6, §5 tuning tiers). Where both read the same data
   (S5↔D5 decisions, S6↔D4 crew): the **scorer** asks *"is this gardener
   over-escalating / leaking crew"* (a behavior problem → tune the gardener); the
   **first mate** asks *"is the plan blocked / impossible"* (a plan problem →
   re-plan or unblock). Different remedy, no collision.
3. **Stop precedence (the one place authority could clash).** The scorer can
   propose `gb gardener stop` at grade F (performance-driven kill). The first mate
   can propose **retirement** (goal-driven graceful decommission). These are
   different verbs with different operator framing:
   - Scorer-F stop = *"this gardener is broken/wasteful, kill or redesign it."*
   - First-mate retire = *"this gardener's effort is complete / baton passed —
     decommission it cleanly."*
   To prevent a gardener getting two conflicting stop decisions in one window, the
   first mate runs its verdict **first** in any shared window and stamps a short-TTL
   `fm-verdict:<state>` label; the scorer **skips the §5 D/F decision** for any
   gardener already carrying a fresh `fm-verdict:phase-complete|effort-complete`
   (it's being retired for the right reason) or `fm-verdict:re-planning` (a re-plan
   decision is already open). The scorer still *scores and reports*; it just doesn't
   stack a redundant stop decision. (Cheap, label-based, no engine change.)
4. **The first mate is itself a gardener** → governed by the master gate (resumes it
   on premature stop) and the watchdog (revives it if it dies). No exemption.

The mental model: **reflexes keep gardeners alive and lean; the first mate keeps
their plans honest.** Three answer *"is it working?"*; the first mate answers
*"is it working on the right thing, and what happens when this thing is done?"*

---

## 4. Succession mechanics — building on §2's `successor` field

§2 of the succession plan defined the declarative `successor` block on a gardener
bead and left **one open question (§2d): does succession execute engine-side
(`gardenerTerminate`) or agent-side (the gardener spawns its own heir)?** The first
mate resolves it.

### 4.1 v0 resolution of §2d — agent-side, executed by the first mate

§2d leans toward **(b) agent-side** for v0 (no new engine authority). The first
mate makes (b) *better* than the predecessor-spawns-its-own-heir variant, because
the deliberative judgment of **"is this phase truly done?"** belongs to a
supervisor with full plan context — not to the exiting gardener's last gasp, and
certainly not to a crashed gardener that never got to its wrap-up.

Two complementary succession triggers, both routed through the first mate:

- **Predecessor-declared** (§2's original path): a gardener declares
  `goal_satisfied=="yes"` carrying a `successor` block. The master gate sees
  terminal-with-successor; rather than the engine creating a gardener, it stamps a
  `succession-ready` label. The first mate (which wakes on the `goal_satisfied`
  event) verifies and instantiates the heir.
- **First-mate-detected** (new): the first mate's S7 finds a phase's exit criteria
  met even though the gardener **didn't** declare (it's mid-run, or it crashed, or
  it has no `successor` block but downstream work clearly warrants one). It
  *proposes* the successor.

Keeping instantiation in the first mate (not the engine, not the dying
predecessor) means succession survives a predecessor that crashed before wrap-up —
the exact failure the watchdog exists for, now covered for the *hand-off* too.

### 4.2 The succession chain (Matt's pipeline)

```text
research ──succeeds──▶ impl ──succeeds──▶ improvement ──succeeds──▶ regression-checking
   │                     │                    │                          │
   each is a gardener;  terminal_goal = that phase's exit criteria;  successor block → next
```

Each phase gardener carries a `successor` block (§2a schema). On phase-complete the
first mate: (1) **verifies** the phase exit criteria (don't hand off a half-done
phase), (2) **instantiates** the heir from the block — `type=trigger`, `role=gardener`,
pinned to the epic, stamped `succeeds: <predecessor-id>` for provenance,
inheriting `project`/`slack_channel`/`operated_by`, (3) **retires** the predecessor
(§5), (4) writes a hand-off note linking pred→heir so the chain is navigable in
beads-viewer.

### 4.3 Proposed additions to the §2a `successor` schema

§2a is half-designed. To drive the verdict + gating cleanly, add:

```json
"successor": {
  "phase": "impl",
  "terminal_goal": "…next phase goal…",
  "prompt_advice": "kd-…",
  "epic": "{{same|kd-…}}",
  "inherit": ["project","slack_channel","operated_by"],
  "when": "goal_satisfied",
  "exit_criteria": "…machine-or-human-checkable phase-done test (S7)…",   // NEW
  "gate": "operator|auto",   // NEW — operator-gated (v0 default) vs pre-approved
  "succeeds": "{{predecessor-id}}"   // NEW — stamped provenance (set at instantiation)
}
```

`exit_criteria` makes S7 concrete (not just "gardener said done"). `gate` is the
maturation lever: **v0 = `operator` for every succession** (each baton-pass is a
creation, and advice ka-0000003852 requires operator sign-off on new gardeners);
once a pipeline is trusted, the operator approves the *pipeline* and intra-pipeline
hand-offs flip to `auto` (first mate instantiates, then notifies).

---

## 5. Retirement / creation lifecycle + operator touchpoints

### 5.1 Gardener lifecycle states (first-mate-managed)

```text
            operator approves (decision)
   PROPOSED ───────────────────────────▶ ACTIVE
      ▲                                   │  │  │
      │ (re-plan / new pipeline)          │  │  └──▶ SUCCEEDING ──▶ (heir ACTIVE, self RETIRED)
      │                                   │  │           (phase-complete, §4)
      └─────────── RE-PLANNING ◀──────────┘  └──▶ RETIRED
                  (thrash/drift, decision)        (effort-complete, notify)
```

- **PROPOSED → ACTIVE.** Creation always begins with an operator `gb decision`
  (advice ka-0000003852). The first mate assembles the gardener spec (goal, prompt
  advice, epic, schedule) and proposes it; on approval it instantiates.
- **ACTIVE → SUCCEEDING → RETIRED(self) + ACTIVE(heir).** Phase-complete (§4):
  baton-pass. Operator-gated per `successor.gate` (v0: gated; trusted pipeline:
  auto-then-notify).
- **ACTIVE → RE-PLANNING.** Thrashing/drifting verdict: the plan is wrong, not the
  gardener. The first mate proposes a re-plan decision (redefine `terminal_goal` /
  restructure the epic / split). Higher stakes than a tuning edit → always gated.
- **ACTIVE → RETIRED.** Effort-complete (goal met, no successor, no open subtree
  work): graceful decommission. The gardener *declared itself done* → **notify
  (squawk), do not gate** (done means done — consistent with the watchdog's "never
  resurrect a done gardener").

### 5.2 Retirement vs. the scorer's stop vs. the watchdog's "leave it"

| Action | Trigger | Owner | Operator touchpoint |
|---|---|---|---|
| **Graceful retire** | effort-complete (goal met, no heir, no work) | first mate | notify (squawk) |
| **Baton-pass retire** | phase-complete, heir instantiated | first mate | gated v0 / notify when auto |
| **Performance stop (F)** | broken/wasteful, score < 40 | scorer | decision (apply redesign / stop / leave) |
| **Retire-and-replace** | thrash/drift — plan wrong, replace gardener+plan | first mate | decision (re-plan) |
| **"Leave it, done"** | declared done OR epic closed | watchdog | none (never resurrect) |

### 5.3 Operator touchpoints — autonomous vs. gated

**Autonomous (squawk / report only, no decision):**
- Render and post the per-gardener **trajectory scorecard** (verdict + signals)
  under epic ka-0000007687, one row per gardener, typed-mentioning each (navigable
  in beads-viewer, per advice ka-0000000831).
- **Ping** the operator on stale subtree decisions (BLOCKED verdict) — a nudge, not
  a new fork.
- **Graceful retire** of an effort-complete gardener (it declared done).
- **Auto baton-pass** within an operator-pre-approved pipeline (`successor.gate ==
  "auto"`), then notify.

**Gated (always a `gb decision` + `gb yield`):**
- **Creation** of any new gardener / first-of-pipeline successor (ka-0000003852).
- **v0 succession** at every phase boundary (until the pipeline is trusted).
- **Re-plan** — redefine a gardener's `terminal_goal` or restructure its epic
  (RE-PLANNING). Never autonomous; mirrors the scorer/watchdog guardrail that goal
  redefinition is operator-only.
- **Retire-and-replace** — killing a misguided gardener to swap its plan.

**Guardrail (carried from the scorer + watchdog).** The first mate has **no
autonomous write authority to mutate another gardener's goal, prompt, or
existence.** Its only autonomous writes are: its own scorecard/report beads,
operator pings, short-TTL `fm-verdict:*` coordination labels, graceful-retire of a
self-declared-done gardener, and auto-succession inside an approved pipeline. Every
creation, re-plan, and replace is a `gb decision` the operator picks. Worst case
for a buggy first-mate run: a spurious decision the operator declines, or a
premature graceful-retire of a gardener that *did* declare done (recoverable —
reopen). Zero unattended destructive writes.

---

## 6. Wake model (ties to the parent epic's event-wake thesis)

The first mate is **inherently event-shaped** — it reacts to plan-state changes —
so it should be the showcase for WS-A's event-wake migration, not a cron poller:

- **Event triggers (primary):** epic child closed, decision resolved in a watched
  subtree, a gardener run finalized with `goal_satisfied=="yes"` (succession seam),
  a gardener stamped `gardener-stuck-escalate` or `succession-ready` by the engine.
- **Cron backstop (secondary):** a slow sweep (e.g. every 6–8h, off the round
  marks per CronCreate guidance) so drift that produces *no* event still gets seen.

This makes the first mate the concrete "event-driven gardener with cron as backup"
exemplar the parent epic ka-0000002254 is chasing.

---

## 7. Build phasing (when approved — NOT now)

0. **Phase 0 — Haiku-on-a-meta-gardener shake-out (the new "we've never done this"
   step).** *Before* the first mate exists, de-risk the model default on a meta-gardener
   we already have. Pin an existing low-blast-radius meta-gardener (the **meta-scorer**
   is the best candidate: read-only, proposes via decision, runs on a cadence) to a
   Haiku account for a bounded window and measure (§9.2): does Haiku reliably drive the
   `gb decision` / `gb yield` / multi-bead-reasoning loop a meta-gardener needs? Compare
   its verdicts against an Opus baseline run on the same gardeners. **Exit criterion:**
   Haiku's proposals are at parity-enough (operator would accept the same calls) and it
   completes its loop without tool-call breakdown. If it fails, the default falls back
   to Sonnet (or Opus) and we revisit — but the first mate still ships; only its model
   pin changes. **This phase has no first-mate dependency and can run today**, in
   parallel with WS-A/WS-B, as the empirical answer to "we have not done this before."
1. **v0 — agent-side meta-gardener, pinned to Haiku by default (§9).** Author the first
   mate as a `type=trigger` `role=gardener` bead under epic ka-0000007687, prompt as an
   advice bead (convention ka-0000002990), **model pinned via a `role:mate`→Haiku-account
   `resolveRoleAccount` pin (§9.1)** carried over from the phase-0 result. Gated behind
   an operator sign-off decision before go-live (ka-0000003852). Implements the §1.2
   verdict, §4 agent-side succession, §5 lifecycle. **No engine change.**
2. **§2a schema additions** (`exit_criteria`, `gate`, `succeeds`) on the
   `successor` block — small, additive, lands with v0's succession path.
3. **Self-block** per advice ka-0000000974: `terminal_goal` = "every live gardener
   is on-track, has an open re-plan/succession decision, or has been retired";
   `run_budget` + a progress-aware `loop_end` (NOT wall-clock-only).
4. **v1 (later, if proven)** — pull S1/S2/S4 mechanical sensing engine-side as a
   `gardener_trajectory` gauge / `off-track` label the first mate reads. Deliberation
   stays agent-side.
5. **v2 (only if warranted)** — engine-side auto-succession for pipelines the
   operator has pre-approved.

---

## 8. Open questions for build time

- **OQ1.** Burndown horizon & thrash threshold (K windows, ETA tolerance) — start
  conservative (flag, don't act) and tune from the first weeks of scorecards.
- **OQ2.** Plan-of-record source for S3: terminal_goal text alone, or require a
  linked plan doc / formula? Proposal: opportunistic — use a linked plan doc/formula
  when present, fall back to terminal_goal + child-title coherence otherwise.
- **OQ3.** Should the first mate, scorer, and watchdog eventually **consolidate**
  into one meta-gardener? They share an epic and cadence-overlap. v0 keeps them
  separate (distinct prompts, distinct questions, smaller blast radius each);
  revisit once all three are live and the operator can judge prompt bloat vs.
  coordination cost.
- **OQ4.** `fm-verdict:*` label TTL and the exact scorer skip-rule (§3.3) — needs a
  real shared-window test once both are live.
- **OQ5.** Auto-succession trust promotion: what evidence flips a pipeline's
  `gate` from `operator` to `auto`? Proposal: N consecutive operator-approved
  hand-offs with no post-hoc reversal.
- **OQ6.** Haiku model parity (§9): does Haiku hold up on the full mate loop, or do
  we need a per-verdict model split (Haiku for cheap sensing sweeps, escalate to
  Sonnet/Opus only when a verdict is THRASHING/DRIFTING and a re-plan proposal must be
  authored)? Resolved empirically by phase-0 (§7.0 / §9.2).

---

## 9. Model default: Haiku out of the box + validation

> Operator directive (Slack 2026-06-11, Matt Baker): *"ship mates with haiku out of
> the box as a strong default; that will help them from getting out of scope and keep
> things fast"* — followed by *"note we have not done this before and so will need to
> test; update all plans accordingly."* This section is that update.

### 9.1 How a mate gets pinned to Haiku (the mechanism — already exists)

Model selection today is **account-driven**, not per-bead. The controller resolves an
agent pod's model from its **account bead**: `applyAccountConfig` sets `ANTHROPIC_MODEL`
and `CLAUDE_MODEL` from `acct.Model` (`podspec.go`), and `gb` passes `--model
$CLAUDE_MODEL` to Claude Code (`init_agent_loop.go`). Crucially there is already a
**role→account pin**: `resolveRoleAccount` lets a role override the project's account
(we use it today to pin `fsm-triage`→`litellm-bedrock`). LiteLLM already aliases
`claude-haiku-4-5` / `claude-haiku-4-5-20251001` (`litellm_aliases.go`).

So shipping the mate on Haiku is a **config composition, not an engine change**:

1. Create (or designate) an **account bead** whose `model` is a Haiku alias
   (e.g. `claude-haiku-4-5`), routed through the existing LiteLLM proxy so spend
   tracking and OAuth forwarding are unchanged.
2. Add a **`role:mate` (or `role:first-mate`) → that-account** pin in the role config
   that `resolveRoleAccount` reads. Any agent spawned with the mate role then inherits
   Haiku automatically — "out of the box," no per-spawn flag.
3. The pin is the **single fallback lever**: if phase-0 says Haiku underperforms, point
   the role at a Sonnet/Opus account instead. One config edit, no code, no re-design.

Caveat to verify at build time: confirm `selectAccount`'s account-pool spreading
respects the role pin for a brand-new role (it does for `fsm-triage`; the mate role
should be added to the same pin path, not the project pool).

### 9.2 Validation track — because we have never run an agent session on Haiku

Today Haiku only powers **narrow, single-shot sub-tasks** (namegen, rate-limit health
probes, bridge summaries). **No full agentic loop** — sustained tool-calling, `gb
decision`/`gb yield`, multi-bead reasoning, self-block discipline — has ever run on
Haiku in this fleet. That is exactly the surface a mate exercises, so we test before we
trust:

**Phase-0 experiment (§7.0), measured against an Opus baseline on the same gardeners:**

| Dimension | Pass bar |
|---|---|
| **Loop integrity** | Completes its run: claims, reasons over the subtree, files proposals, self-blocks correctly — no tool-call malformation, no premature stall. |
| **Verdict parity** | On a labeled set of gardeners, Haiku's trajectory verdicts (§1.2) match the Opus baseline / operator ground truth ≥ an agreed threshold (start ~80%, tune). |
| **Proposal quality** | The `gb decision` proposals it authors (re-plan / succession / retire) are ones the operator would accept — judged on the first weeks of real decisions. |
| **Scope discipline** | Stays inside the supervisory remit (the *reason* Matt wants the small model) — does NOT start doing crew work, editing code, or over-escalating. |
| **Cost / latency** | Per-run cost and wall-clock vs. Opus — quantify the "keep things fast/cheap" win. |

**Outcomes & fallbacks:**
- **Pass** → Haiku becomes the shipped default mate model (the §9.1 role pin).
- **Partial** (cheap sensing fine, weak on authoring re-plans) → **per-verdict model
  split** (OQ6): Haiku runs the routine ON-TRACK/BLOCKED sweeps; escalate to Sonnet/Opus
  only to author a THRASHING/DRIFTING re-plan. Mechanism: a second account + the mate
  prompt choosing when to spawn an Opus sub-agent for the heavy proposal.
- **Fail** → fall back the role pin to Sonnet/Opus; first mate still ships, model TBD.

**Safety net that makes all of this low-risk:** a mate has **zero autonomous
destructive authority** (§5.3 guardrail). The worst a confused Haiku run can do is file
a bad proposal the operator declines, or a premature graceful-retire that's reversible
(reopen). So we can validate Haiku *in the real fleet* behind the operator decision
gate, not just in a sandbox.

### 9.3 Cross-plan impact (other workstreams in epic ka-0000002254)

- **WS-A (event-wake):** unaffected by model choice — wake config is orthogonal to the
  model pin. The mate remains the event-wake showcase (§6) regardless of model.
- **WS-B (resumer-watchdog go-live):** if phase-0 uses the **meta-scorer** as the Haiku
  guinea pig, coordinate so the model experiment doesn't muddy the watchdog
  observability metrics — run the Haiku window on the scorer, keep the watchdog on its
  current model until its own go-live is decided.
- **WS-D (docs consolidation):** the consolidated gardener reference must document the
  `role:mate`→Haiku account pin as the canonical mate-model pattern, plus the
  per-verdict-split fallback if OQ6 lands that way.
