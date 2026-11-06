# Gardener Resume — terminal_goal as the master gate + resumer watchdog

> **Origin:** Slack thread (Matt Baker) — *"revisit our meta gardeners, we need
> something to make sure gardeners who are stuck will resume to continue their
> goals until they declare themselves to be done."*
> **Tracking bead:** ka-0000005353 · **Decision:** ka-0000003963 (chosen: *both* — engine + watchdog)
> **Operator:** Matt Baker (`oidc:matthew.baker@acme.example`)

## TL;DR

Gardeners today can **stop** for six reasons; only **one** of them means "the
goal is done." The other five are *premature* — they silently abandon unmet
goals. We make **`terminal_goal` (self-declared done) the master gate**: a
gardener may only truly terminate when it has declared its goal satisfied.
Every other stop signal converts from *kill* to *escalate-or-resume*. A new
**resumer watchdog** meta-gardener is the safety net for the cases the engine
cannot self-heal (disabled, crashed agent, budget-exhausted-and-closed).

## 1. The problem, precisely

`internal/scheduler/gardener_stop.go` + `internal/bridge/gardener_evaluator.go`
implement a **termination-only** stop machine. A gardener's loop ends on any of:

| Stop reason | Where | Means goal done? |
|---|---|---|
| `terminal_goal` reached (agent declares `goal_satisfied=="yes"` / `gb stop`) | wrap-up path | ✅ **YES** |
| `run_budget` / `max_runs` exhausted | `gardenerPreFireCheck` (1) | ❌ no — just out of runs |
| `no_progress_for` N runs | `gardener_stop.go` `StopKindNoProgressFor` | ❌ no — wedged, not done |
| `wall_clock` elapsed | `StopKindWallClock` | ❌ no — out of time |
| `consecutive_failures` cap | `StopKindConsecutiveFailures` | ❌ no — crashing |
| cron preflight goes dormant (epic has no open children) | trigger preflight | ⚠️ maybe |
| pod crash / agent leak / gardener `enabled=false` | infra / operator | ❌ no — accident |

Only row 1 is "done." Every other row stops a gardener whose `terminal_goal`
may be unmet, and **nothing brings it back.** Live proof: gardener
`ka-0000006201` is `enabled=false` while its epic `ka-0000005370` is still
open.

There is already a run-level resume primitive — `wrapUpProgressing()` in
`bot_agents_wrapup.go` re-spawns an agent that wrote `goal_satisfied:"no"`.
The gap is at the **gardener-loop** level: that signal is not consulted when
the loop decides to terminate.

## 2. Reconciling with advice ka-0000000974

Advice ka-0000000974 mandates every gardener self-block so it can't spin
forever. This design does **not** weaken that — it *relocates* the backstop:

- The backstops still fire. What changes is the **action** on firing:
  `no_progress_for` / `max_runs` / `wall_clock` / `consecutive_failures` no
  longer silently close the gardener. They raise an **escalation or a re-fire**.
- Unbounded spin is still bounded — by the **resume budget** (max K resumes
  without progress → escalate to operator), not by silent death.

So "must self-block" becomes "must self-block **into an operator-visible
escalation**, not into the void." Strictly safer: nothing dies unattended,
and genuine spin reaches a human instead of being swept under a closed bead.

## 3. Part B — Engine change (`gardener_stop.go` + `gardener_evaluator.go`)

### B1. Classify stop kinds: terminal vs. premature

Add a helper `stopKindIsTerminal(kind string) bool`:

- **Terminal** (goal genuinely satisfied / operator-willed): `manual`,
  `manual_force`, `specific_bead_closed`, `gate_present`/`gate_absent` when
  used as an explicit done-gate, and the new `goal_satisfied` kind (B2).
- **Premature**: `max_runs`, `wall_clock`, `no_progress_for`,
  `consecutive_failures`, `bead_query` (epic-empty preflight is ambiguous —
  treated premature unless paired with a goal-satisfied check).

### B2. New stop kind `goal_satisfied` (the master gate)

`StopKindGoalSatisfied = "goal_satisfied"`. Fires only when the gardener's most
recent run declared its `terminal_goal` met — read from the gardener's
`run_log` last record (`outcome==done && goal_satisfied=="yes"`), reusing the
`wrapUpProgressing`/`goal_satisfied` plumbing already in the wrap-up path.
This is the canonical "gardener declares itself done."

### B3. `EvaluateGardenerStopSpec` precedence change

Today: `hard_stop > loop_end > run_end`. New rule:

- A **premature** `loop_end` entry firing no longer closes the gardener. It
  produces a `StopMatch{Premature:true}`. `gardenerPreFireCheck` translates a
  premature match into **re-fire-or-escalate** (Part B4) instead of
  `gardenerTerminate`.
- A **terminal** entry (`goal_satisfied`, `manual*`, `specific_bead_closed`)
  closes the gardener as today.
- `hard_stop` is unchanged — a true emergency brake (5 consecutive failures
  still kills the pod), but it now **escalates** rather than silently closing
  (Part B4), so the operator sees a crashing gardener.

### B4. Premature-stop → resume-or-escalate

New `gardenerResumeOrEscalate(ctx, daemon, trig, match)`:

1. Read resume bookkeeping from the gardener block: `resumes_without_progress`
   (new int field) and a `resume_budget` (new field, default 5).
2. If `resumes_without_progress < resume_budget`: **re-arm** — re-enable the
   gardener (`enabled=true` if disabled), reset the spent `run_budget` headroom
   by bumping `run_budget` by a refill quantum, comment the re-fire reason,
   and let the next preflight fire. Increment `resumes_without_progress`.
3. If `>= resume_budget`: **escalate** — the engine has no agent context (it
   can't run `gb decision`), so it **flags** the gardener: stamps a
   `gardener-stuck-escalate` label + `gardener_stuck_escalated=true` field +
   comment, and closes it so it stops spinning. The **resumer watchdog**
   (Part A) lists `gardener-stuck-escalate` gardeners and opens the operator
   decision ("gardener X stuck N resumes, goal still unmet: keep resuming /
   redefine goal / stop"). This split keeps the engine pure (no Slack/decision
   plumbing in the fire path) while still bounding spin at the operator —
   where advice ka-0000000974's bound lives.
4. Any **real progress** event resets `resumes_without_progress` to 0 — a
   gardener making headway resumes indefinitely; only *fruitless* resumes count
   against the budget. Implemented in `gardenerAgentTracker`: when the finalized
   run's `ProgressScore > 0`, the counter is reset on the same persist.

> **Implemented (2026-06-09, MR pending):** `StopKindGoalSatisfied`,
> `stopKindIsTerminal`, `StopMatch.Premature`, the `goal_satisfied` evaluator,
> `RunRecord.GoalSatisfied` + `ExtractWrapupGoalSatisfied`, the `GardenerSpec`
> resume fields (`resume_budget`, `run_budget_refill`,
> `resumes_without_progress`, `last_resume_reason`), `gardenerResumeOrEscalate`
> (re-arm/escalate), and the run_budget step-(1) reroute. 9 tests across
> `scheduler` + `progress` + `bridge`. `ResumeBudget < 0` is an explicit
> opt-out preserving legacy terminate-on-premature behavior.

### B5. `gb gardener start` default stop spec

After Proposal A/B of the 2026-06-07 review lands (schema admits the loop
fields), `gb gardener start` emits `loop_end.any` containing exactly one
**terminal** entry — `{"kind":"goal_satisfied"}` — and moves
`max_runs`/`no_progress_for`/`wall_clock` into a new `resume_triggers` block
(premature). The prose echo stays for humans.

## 4. Part A — Resumer watchdog meta-gardener

The engine (Part B) self-heals gardeners that are **alive and looping**. It
cannot revive a gardener that is `enabled=false`, was `closed` by a legacy
budget exhaustion, or whose pod crashed without writing a wrap-up. The
watchdog covers those.

### Shape (watchdog-style, runbook §5.2 — sibling to the scorer, NOT the scorer)

```
type:      trigger (role=gardener)
cron:      "17 */4 * * *"        # every 4h, off the round marks (scorer is twice-daily)
timezone:  America/Los_Angeles
preflight: there exists ≥1 gardener whose terminal_goal is unmet AND is not live
max_concurrent: 1
terminal_goal: "Every gardener is either live-and-progressing, has declared its goal done, or has an open operator escalation about its stuck state."
resume_budget / max_runs / no_progress_for: per ka-0000000974
```

### Each run

1. Enumerate gardeners: `kd list --type=trigger --label role:gardener`.
2. For each, determine **live?** (open + enabled + recent run / live agent) and
   **goal met?** (last run `goal_satisfied=="yes"`, or epic closed).
3. **Stuck = goal unmet AND not live.** For each stuck gardener, classify why:
   - *disabled* → re-enable + re-fire (if under resume budget).
   - *closed-by-budget* → reopen + refill `run_budget` + re-fire.
   - *dead agent / leaked pod* (in_progress >1h, no live pod — advice
     ka-0000007726) → clear `last_agent_id`, re-fire.
   - *resume budget exhausted* → **escalate to operator**, do not loop.
4. Write a **resume scorecard** bead under epic ka-0000007687 (one row per
   gardener: state, why-stuck, action taken), typed-mention each gardener.
5. `gb squawk` one-line summary; `gb yield` if any escalation open, else
   `gb stop`.

### Why both, not just one

- **Engine (B)** is the correct go-forward default: new gardeners never
  silently abandon a goal. Real-time, no polling.
- **Watchdog (A)** is the safety net for the **installed base** and for
  **out-of-band death** (crash, operator disable) the engine can't observe
  from inside its own loop. Belt and suspenders.

## 5. Guardrails (carried from the scorer's design)

- The watchdog **never edits another gardener's goal/prompt** on its own
  authority — re-enabling + refilling budget + re-firing is the only
  autonomous action. Goal redefinition and permanent stop go through an
  operator `gb decision`.
- A gardener that has **legitimately declared done** (`goal_satisfied=="yes"`)
  is never resumed. Done means done.
- Resume budget makes spin terminate at a human, satisfying ka-0000000974.

## 6. Build order

1. **B1–B3** engine: `stopKindIsTerminal`, `goal_satisfied` kind, premature
   classification in `EvaluateGardenerStopSpec`. Unit tests in
   `gardener_stop_test.go`.
2. **B4** `gardenerResumeOrEscalate` + new gardener-block fields
   (`resume_budget`, `resumes_without_progress`). Wire into
   `gardenerPreFireCheck`. Tests in `gardener_evaluator_test.go`.
3. **A** watchdog meta-gardener authored via `gb gardener start` (or trigger
   bead) under epic ka-0000007687, prompt as advice bead (convention
   ka-0000002990). **Gated behind an operator decision** before it goes live
   (advice ka-0000003852 — new gardener needs operator sign-off).
4. **B5** default-stop-spec change in `gb gardener start` (depends on the
   2026-06-07 schema-widening Proposal A landing first).

## 7. Source map

| Concern | File |
|---|---|
| Stop-spec evaluator | `gardenio/controller/internal/scheduler/gardener_stop.go` |
| Stop slots/kinds | `gardenio/controller/internal/scheduler/stop.go` |
| Gardener pre-fire / terminate | `gardenio/controller/internal/bridge/gardener_evaluator.go` |
| GardenerSpec fields | `gardenio/controller/internal/bridge/triggers.go` |
| Run-level resume primitive (`goal_satisfied`) | `gardenio/controller/internal/bridge/bot_agents_wrapup.go` |
| `gb gardener start` authoring | `gardenio/controller/cmd/gb/gardener.go` |
| Scorer rubric (sibling meta-gardener) | `gardenio/docs/gardener-effectiveness-rubric.md` |
| Prior stop-conditions review (schema gate) | `gardenio/docs/gardener-stop-conditions-review-2026-06-07.md` |
