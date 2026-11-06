# D1 Progress-Signal Design (Phase 3)

> **Status:** v1 (2026-08-12). Phase 3 of the *Formalize gardeners* epic
> ([[ka-0000004384]]), delivered against task [[ka-0000004240]]. Builds on the
> Phase 1 feature trace ([[ka-0000005039]], bundle [[ka-0000002547]]) and the
> Phase 2 wrap-up audit + schema ([[ka-0000003135]],
> [`wrapup-specification.md`](./wrapup-specification.md),
> [`schemas/wrapup.schema.json`](./schemas/wrapup.schema.json)). It hands the
> Phase 5 implementation task ([[ka-0000003456]]) a concrete plan.

This document answers the Phase 3 question:

> **How should a gardener emit progress evidence so the meta-gardener can score
> D1 (progress yield) correctly across every gardener generation?**

---

## TL;DR (the decision)

**Adopt Option C — dual-source with graph-first, self-report-fallback — and it
is already the implemented signal.** The daemon-computed graph signal
(`progress_state.events`: `child_closed` / `mr_merged` / `decision_resolved`)
is authoritative; the agent's self-reported `progress_score` is the fallback
and the tie-breaker for runs whose outcome is non-graph (a genuine
prose-only advance). A run counts as a **progress run** when:

```
progress_run(run) := run.progress_score >= 1
                     OR ∃ credited event e ∈ progress_state.events
                        with e.occurred_at ∈ [run.started_at, next_run.started_at)
```

The one remaining gap is **enforcement location**: D1 is still computed by the
meta-gardener *agent* by eye, so two runs can score the same gardener differently
(the exact class of bug [[ka-0000003502]] documents). Phase 5's headline task is
therefore to **make D1 machine-enforceable — a deterministic `bridge.ScoreGardenerD1`
alongside the existing `bridge.ScoreGardenerD6`** — plus backfill a progress
spec onto the handful of spec-less NEW gardeners. No new signal needs to be
invented; the design is to *pin* the one that already works.

---

## 1. Why this was broken (the original symptom)

The first meta-gardener scoring run ([[ka-0000006820]]) computed **D1 = 0 across
all 8 gardeners while D2 = 5** (children *were* closing). That is definitionally
impossible — a gardener whose children are closing is not spinning — so it was a
measurement bug, not a fleet of broken gardeners. Two independent root causes,
both now understood:

1. **No signal to read.** The scored gardeners had no `progress` spec, so the
   `gardenerDiscoverProgress` hook never fired and `progress_state` was never
   written. D1 had literally nothing to count.
2. **The async-attribution gap.** Even with a spec, a healthy *delegating*
   gardener dispatches crew and honestly wraps up at `progress_score: 0`; the
   crew then close the epic's children **asynchronously**, in the gap *after*
   the dispatch run's `closed_at`. If D1 reads `progress_score` alone it records
   a false 0 on every delegator (gardener ka-0000002880 scored **C (D1 0%)** one
   window and **A (D1 100%)** the next with no config change — bug
   [[ka-0000003502]]).

Both are addressed by the dual-source signal below; the second is why the
signal cannot be self-report-only.

## 2. The three options

| | Signal | Source of truth |
|---|---|---|
| **A** | `progress_state.events` only (daemon-computed) | Graph — the daemon credits `child_closed` / `mr_merged` / `decision_resolved` events against the run window. |
| **B** | `run_log.progress_score` only (agent self-report) | Wrap-up — the agent declares `progress_score ∈ {0,1,≥2}` at `gb stop`. |
| **C** | Dual-source: graph event **OR** self-report ≥ 1, with async reconciliation | Graph first, self-report as fallback / tie-breaker. |

### 2.1 Analysis against the Phase-3 criteria

**Backward compatibility with existing gardeners.**

| Option | Verdict |
|---|---|
| A | Needs a `progress` spec on every gardener to emit events. **Live-fleet reality (surveyed 2026-08-12): 16/16 gardeners that have ever run already carry a `progress` spec and a populated `progress_state.events`; only NEW gardeners (runs = 0) lack one.** So A is *nearly* already satisfied — the spec-less set is exactly the set with nothing to score yet. |
| B | Needs a structured JSON wrap-up with `progress_score`. Phase 2 found `progress_score` present on **15/23 (65%)** of runs — and the 8 misses are gardeners whose schedule never set `progress.surface.in_wrapup=true`, i.e. opted out on purpose. B silently under-counts every opted-out gardener. |
| C | Degrades gracefully: a gardener with a spec is scored on events; a gardener with only a self-report is scored on that; a NEW gardener is graded **NEW** (§4 of the rubric) and re-evaluated next window. No gardener generation is left unscoreable. |

**Implementation complexity (controller / bridge work).**

| Option | Verdict |
|---|---|
| A | Event discovery + persistence: **already built** (`gardenerDiscoverProgress` → `progress.Evaluator.Discover` → `Gardener.ProgressState`). The async-attribution fix (`ReconcileAsyncProgress`) is **already wired** into the trigger engine (`triggers.go:2248`). |
| B | `ExtractWrapupProgress` + `FinalizeRun` already parse and record `progress_score`. Also built. |
| C | C is *the composition of A and B that already exists in code* — `FinalizeRun` rescues a score-0 run whose window contains a credited event (`HasCreditedEventInWindow`), and `ReconcileAsyncProgress` re-scores score-0 runs whose *attribution interval* later gains an event. The remaining work is **not** signal plumbing; it is lifting D1 out of agent-eyeball into a deterministic scorer (see §5). |

**Confidence in the signal (false-positive / false-negative rates).**

- **Option A false-negatives:** a real prose-only advance (e.g. a gardener that
  adjudicates a hard call, writes a design bead, or unblocks a sibling without
  closing a child) emits no credited event and scores 0. Rare but real.
- **Option B false-negatives:** the async-attribution artifact — *systematic*,
  not rare. Every delegating gardener's dispatch runs read 0. This is the bug
  that actually shipped bad grades.
- **Option B false-positives (gaming):** a gardener could self-report
  `progress_score: 2` on a barren run. The graph signal cannot be gamed this
  way — `crew_dispatched` and `wrapup_field` are deliberately **excluded** from
  `CreditedProgressKinds` (dispatching is not yet an outcome; a self-report is
  not an independent fact), and `cancel_duplicates` decision events never enter
  `state.Events` at all.
- **Option C** takes the union: it recovers A's prose-only advances via the
  self-report fallback, and recovers B's async closures via the graph events —
  each covering the other's blind spot. Its residual false-positive surface is
  the self-report path, bounded by the D1/D2 **consistency guard** (§2.2).

**Alignment with Phase 1 findings.** The Phase 1 trace of E-TOPO found that
*stale-crew / no-progress detection was human-eyeball on gardener cycle 3 — the
exact D1 progress-yield gap this epic names*, and that the workflow's real
bottlenecks were mechanical (arming the gardener, CI-red disambiguation), **not**
human judgment. That argues directly for a signal that is (a) daemon-computed so
it needs no human read, and (b) tolerant of the delegating pattern E-TOPO used
(a scheduled gardener that dispatched crew who closed 5 items async in ~2.5h).
Option C is the only option that scores that exact run correctly.

### 2.2 The D1/D2 consistency guard (the anti-false-positive backstop)

D1 and D2 read the **same underlying fact** — the epic's children closing — from
two angles. They cannot legitimately diverge to the extremes. The rubric's
mandatory guard (§2.1 of the rubric) is load-bearing for this design:

> **If D2 ≥ 4 (children actively closing this window) then D1 must be ≥ 3.** A
> computed `D1 == 0` alongside `D2 ≥ 4` is definitionally a measurement bug in
> the D1 count, never a real "pure spin" gardener.

This guard is what makes Option C *safe* even though it includes a gameable
self-report path: the graph-derived D2 acts as an independent cross-check on
D1, so an inflated or deflated D1 that contradicts real throughput is caught
and re-derived from events.

## 3. Decision

**Option C, graph-first.** Rationale in one line per criterion:

- **Backward-compat:** only option that scores every gardener generation without
  a fleet-wide migration (16/16 run-having gardeners already emit events).
- **Complexity:** the signal is *already implemented*; the decision is to pin
  it, not build it.
- **Confidence:** union of A and B covers each other's blind spots; the D1/D2
  guard bounds the residual self-report false-positive.
- **Phase-1 alignment:** matches the observed delegating-gardener workflow and
  removes the human-eyeball step Phase 1 flagged.

**Signal contract (canonical):**

```
# A gardener run is a PROGRESS RUN iff either holds:
#   (self-report)  run.progress_score >= 1
#   (graph)        a credited event (child_closed | mr_merged | decision_resolved)
#                  in progress_state.events has occurred_at in the run's
#                  ATTRIBUTION INTERVAL [run.started_at, next_run.started_at)
#                  — the freshest run owns [started_at, +inf).
#
# yield  = progress_runs / scored_runs        (scored_runs = last N done|failed runs)
# D1_raw = round(5 * yield)
#
# GUARD: if D2 >= 4 then D1 must be >= 3; a computed D1==0 with D2>=4 is a
#        scorer bug — re-derive from events, never emit the low grade.
```

Canonical field references: the self-report source is
`wrapup.schema.json#/properties/progress_score`; the graph source is
`progress.CreditedProgressKinds`. Do **not** clamp the event's upper attribution
bound to the run's `closed_at` — the child closes *after* the run ends, so the
window is `[started_at, next_run.started_at)`, half-open (this is the exact
semantics of `State.ReconcileAsyncProgress` / `hasCreditedEventInInterval`).

## 4. What already exists (do not rebuild)

The dual-source signal is live in the trigger engine today. Phase 5 must not
re-implement any of this — it must *consume* it:

| Piece | Location | Role |
|---|---|---|
| Progress spec parse | `scheduler/progress/spec.go` (`ParseSpec`) | Declares what counts as progress per gardener. |
| Event discovery | `scheduler/progress/evaluator.go` (`Evaluator.Discover`) | Queries the graph, appends `child_closed`/`mr_merged`/`decision_resolved` to `state.Events`. |
| Persistence hook | `bridge/gardener_hooks.go` (`gardenerDiscoverProgress`) | Writes new events back to `Gardener.ProgressState` each cycle. |
| Self-report extract | `scheduler/progress/evaluator.go` (`ExtractWrapupProgress`) | Parses `progress_score` / `progress_summary` from the wrap-up. |
| Synchronous rescue | `scheduler/progress/evaluator.go` (`FinalizeRun` → `HasCreditedEventInWindow`) | Lifts a score-0 run whose window already contains a credited event. |
| Async reconciliation | `scheduler/progress/state.go` (`ReconcileAsyncProgress`) + wiring at `bridge/triggers.go:2248` | Re-scores score-0 runs whose attribution interval *later* gains an event — fixes the delegator false-0. |
| Anti-gaming | `scheduler/progress/state.go` (`CreditedProgressKinds`) | `crew_dispatched` + `wrapup_field` excluded so dispatching/self-report can't self-credit. |
| Iteration finalizer | `bridge/gardener_hooks.go` (`gardenerFinalizeRunsFromIterations`) | Rebuilds `ProgressState.Runs` for concurrent subtree gardeners whose single `LastAgentID` pointer is overwritten before finalize. |

## 5. Phase 5 implementation plan

The signal is done; the **scoring** of it is not machine-pinned. This is the
same maturity gap D6 had before [[ka-0000000452]] made it deterministic. Mirror
that solution.

### T1 — `bridge.ScoreGardenerD1` (deterministic D1 scorer) — **primary**

Add `gardenio/controller/internal/bridge/gardener_d1.go`, modeled on
`gardener_d6.go`:

- **Input:** the decoded `gardener` blob (same one `ScoreGardenerD6` reads) — its
  `progress_state.runs` and `progress_state.events` — plus the D2 window's
  closed-child count (for the consistency guard).
- **Output:** a `D1Verdict{ Raw int; Weighted float64; ProgressRuns int;
  ScoredRuns int; Runs []D1RunCredit }` where each `D1RunCredit` records *why* a
  run was credited (`self_report` | `graph_event:<kind>:<target>` | `none`) so
  the scorecard and `gb gardener status` can show the derivation, not just the
  number.
- **Logic:** exactly the §3 contract. Reuse `progress.State.ReconcileAsyncProgress`
  semantics for attribution rather than re-deriving intervals; call the
  existing `progress.CreditedProgressKinds`. Apply the D1/D2 guard: if
  `D2_raw >= 4` clamp `D1_raw` to `>= 3` and set a `guard_applied` flag on the
  verdict.
- **Determinism pin:** a `TestD1Contract_Doc`-style test (mirror
  `TestD6CheckNames_Doc`) that fixes the credit-reason vocabulary so the code,
  this doc, and the rubric §D1 box can never silently drift.

### T2 — Surface D1 in `gb gardener status`

Render the D1 line (raw, weighted, progress-runs/scored-runs, and the per-run
credit reason) the way the D6 line already renders, and carry the full `d1`
object in `gb gardener status --json`. This lets the meta-gardener (and a human)
read D1 from tooling output instead of re-deriving it by eye — killing the
"two runs, two grades" class of bug at the source.

### T3 — Point the meta-scoring prompt at the deterministic scorer

Update `gardener-meta-scoring-prompt.md` step for D1 to *"read `d1.raw` /
`d1.weighted` from `gb gardener status --json`; do not re-derive"* — the same
language the prompt already uses for D6. Keep the §D1 async-reconciliation and
consistency-guard prose as the *explanation* of what the scorer does, not as a
manual procedure.

### T4 — Backfill progress specs onto spec-less gardeners

The 2026-08-12 survey found NEW gardeners (runs = 0) without a `progress` spec.
Add the spec at **spawn time** so no gardener ever starts unscoreable:

- Have `gb gardener start` emit a default `progress` spec (a `child_closed`
  matcher scoped to the gardener's epic, plus a `since_run_start` window and
  `surface.in_wrapup=true`) when the author supplies none. This is the
  mechanistic fix for root-cause #1 in §1 — a gardener can no longer be born
  without a signal.
- One-shot: backfill the current spec-less live gardeners via `kd update` (safe,
  reversible, additive to the `gardener` blob).

### T5 — Regression fixture

Add a fixture reproducing the [[ka-0000003502]] delegator scenario (a gardener
with a tail of honest `progress_score: 0` dispatch runs and `child_closed`
events landing in the post-`closed_at` gap) and assert `ScoreGardenerD1` returns
`D1_raw >= 3` with `guard_applied` when D2 ≥ 4. This locks the exact false-0
that motivated the whole epic.

### Success criterion (inherited from Phase 5, [[ka-0000003456]])

> D1 scoring computes > 0 for productive runs on the next meta-gardener cycle —
> and computes the **same** value on repeat runs of the same window.

The second clause (determinism) is the part this design adds beyond the original
Phase 5 wording, and is why T1 (not more signal plumbing) is the headline task.

## 6. Rejected alternatives (one line each)

- **Option A (graph-only):** rejected — loses genuine prose-only advances
  (adjudications, design beads) that emit no credited event; a small but real
  false-negative with no recovery path.
- **Option B (self-report-only):** rejected — the async-attribution artifact
  makes it *systematically* wrong for every delegating gardener, and it is
  gameable with no independent cross-check. This is the signal that shipped the
  original bad grades.
- **A new event kind for "prose progress":** rejected — indistinguishable from
  the self-report we already have; would re-introduce the gaming surface
  `CreditedProgressKinds` deliberately closes.

---

## Appendix A — Live-fleet progress-spec adoption (2026-08-12)

Surveyed all 20 open `role:gardener` triggers. **Every gardener with ≥ 1
completed run carries a `progress` spec and a populated `progress_state.events`;
the only spec-less gardeners are NEW (runs = 0).** This is the empirical basis
for the backward-compat verdict in §2.1 and for scoping T4 to spawn-time
defaulting rather than a fleet-wide migration.

| Gardeners surveyed | Have progress spec | Have credited events | Spec-less **and** run-having |
|---|---|---|---|
| 20 | 16 | 8 (all with runs > 0) | **0** |

The 8-with-events / 16-with-spec gap is gardeners that carry a spec but have not
yet closed a creditable child in their retained event window — expected, not a
defect.
